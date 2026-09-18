// Package queue is the Kafka transport: the request topic, a tiered set of
// delay topics used for retries, and a dead-letter topic.
//
// Why delay topics rather than sleeping in the main consumer: Kafka has no
// per-message visibility timeout, so the usual way to "retry in 30s" is to
// sleep before committing — which stalls the whole partition and blocks every
// healthy message queued behind the poisoned one. Instead a failed job is
// republished to a retry topic whose consumer does nothing but wait out that
// tier's fixed delay. Head-of-line blocking still exists inside a retry topic,
// but it is bounded by that tier's delay and only affects other failures.
//
//	pr.review.requested ──fail──▶ pr.review.retry.10s ──▶ back to requested
//	                     ──fail──▶ pr.review.retry.60s ──▶ back to requested
//	                     ──fail──▶ pr.review.retry.300s ─▶ back to requested
//	                     ──exhausted──▶ pr.review.dlq
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

// RetryTiers is the backoff ladder. Attempt N (1-based) that fails waits
// RetryTiers[N-1]; past the end of the ladder the job goes to the DLQ.
var RetryTiers = []time.Duration{10 * time.Second, 60 * time.Second, 300 * time.Second}

// ReviewRequest is the message body on every topic in this package.
type ReviewRequest struct {
	JobID     uuid.UUID `json:"job_id"`
	RepoOwner string    `json:"repo_owner"`
	RepoName  string    `json:"repo_name"`
	PRNumber  int       `json:"pr_number"`
	DryRun    bool      `json:"dry_run"`
	// Attempt is the delivery count as seen by the producer. It is carried in
	// the payload as well as a header so that a message inspected with
	// kafkacat is self-describing.
	Attempt int `json:"attempt"`
	// NotBefore lets the retry consumer wait out the remainder of a tier delay
	// even if it restarted in the middle of one.
	NotBefore  time.Time `json:"not_before,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// RetryTopic names the delay topic for a given attempt, and reports whether
// the ladder is exhausted.
func RetryTopic(base string, attempt int) (topic string, delay time.Duration, ok bool) {
	if attempt < 1 || attempt > len(RetryTiers) {
		return "", 0, false
	}
	d := RetryTiers[attempt-1]
	return fmt.Sprintf("%s.retry.%ds", base, int(d.Seconds())), d, true
}

// AllTopics is every topic the system needs; used to pre-create them so the
// first produce does not race auto-creation.
func AllTopics(base, dlq string) []string {
	out := []string{base, dlq}
	for i := range RetryTiers {
		t, _, _ := RetryTopic(base, i+1)
		out = append(out, t)
	}
	return out
}

type Producer struct {
	w *kafka.Writer
}

func NewProducer(brokers []string) *Producer {
	return &Producer{w: &kafka.Writer{
		Addr: kafka.TCP(brokers...),
		// Topic is set per message so one writer serves every topic.
		Balancer: &kafka.Hash{},
		// RequiredAcks=all: losing a review request to a broker failover would
		// silently drop a user's review, and the throughput here is tiny.
		RequiredAcks: kafka.RequireAll,
		Async:        false,
		BatchTimeout: 50 * time.Millisecond,
		MaxAttempts:  5,
	}}
}

func (p *Producer) Close() error { return p.w.Close() }

// Publish writes a request to a topic, keyed by repo/PR so all messages for a
// pull request land on one partition and are processed in order.
func (p *Producer) Publish(ctx context.Context, topic string, req ReviewRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return p.w.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   []byte(PartitionKey(req)),
		Value: body,
		Headers: []kafka.Header{
			{Key: "job_id", Value: []byte(req.JobID.String())},
			{Key: "attempt", Value: []byte(strconv.Itoa(req.Attempt))},
		},
		Time: time.Now(),
	})
}

// PartitionKey keeps every message about one pull request on one partition.
func PartitionKey(r ReviewRequest) string {
	return fmt.Sprintf("%s/%s#%d", r.RepoOwner, r.RepoName, r.PRNumber)
}

type Consumer struct {
	r *kafka.Reader
}

func NewConsumer(brokers []string, topic, group string) *Consumer {
	return &Consumer{r: kafka.NewReader(kafka.ReaderConfig{
		Brokers:  brokers,
		Topic:    topic,
		GroupID:  group,
		MinBytes: 1,
		MaxBytes: 10e6,
		// Offsets are committed explicitly after the handler returns, which is
		// what makes delivery at-least-once rather than at-most-once.
		CommitInterval: 0,
		MaxWait:        500 * time.Millisecond,
	})}
}

func (c *Consumer) Close() error { return c.r.Close() }

// Message pairs a decoded request with the raw Kafka message needed to commit.
type Message struct {
	Request ReviewRequest
	Raw     kafka.Message
}

// Fetch reads the next message without committing it.
func (c *Consumer) Fetch(ctx context.Context) (*Message, error) {
	m, err := c.r.FetchMessage(ctx)
	if err != nil {
		return nil, err
	}
	var req ReviewRequest
	if err := json.Unmarshal(m.Value, &req); err != nil {
		// A message we cannot decode can never succeed on retry. Commit it and
		// surface the error so the caller can count it; leaving it uncommitted
		// would wedge the partition forever.
		_ = c.r.CommitMessages(ctx, m)
		return nil, fmt.Errorf("undecodable message at offset %d (committed to avoid a poison loop): %w", m.Offset, err)
	}
	return &Message{Request: req, Raw: m}, nil
}

func (c *Consumer) Commit(ctx context.Context, m *Message) error {
	return c.r.CommitMessages(ctx, m.Raw)
}

// EnsureTopics creates topics up front. Auto-creation is disabled on most
// managed Kafka clusters, and relying on it makes the first run flaky.
func EnsureTopics(ctx context.Context, brokers []string, topics []string, partitions int) error {
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	cc, err := kafka.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		return err
	}
	defer cc.Close()

	cfgs := make([]kafka.TopicConfig, 0, len(topics))
	for _, t := range topics {
		cfgs = append(cfgs, kafka.TopicConfig{
			Topic:             t,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		})
	}
	// CreateTopics is idempotent in kafka-go: existing topics are not an error.
	return cc.CreateTopics(cfgs...)
}
