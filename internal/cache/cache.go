// Package cache wraps Redis with the three jobs it actually does in this
// system: caching expensive reads, caching LLM responses, and holding the
// per-pull-request lock that stops two workers reviewing the same commit.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	rdb *redis.Client
}

func New(addr, password string, db int) *Cache {
	return &Cache{rdb: redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})}
}

func (c *Cache) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }
func (c *Cache) Close() error                   { return c.rdb.Close() }

// ErrMiss is returned by Get when the key is absent.
var ErrMiss = errors.New("cache miss")

func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrMiss
	}
	return v, err
}

func (c *Cache) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

// Lock is a single-holder lock with a lease. It is deliberately the simple
// SET NX PX form rather than Redlock: a lost lock here costs one duplicated
// review, not a correctness bug, because the job state machine in Postgres is
// the real guard. Redis just saves the wasted work.
type Lock struct {
	c     *Cache
	key   string
	token string
}

func (c *Cache) AcquireLock(ctx context.Context, key string, ttl time.Duration) (*Lock, bool, error) {
	token := randomToken()
	ok, err := c.rdb.SetNX(ctx, "lock:"+key, token, ttl).Result()
	if err != nil || !ok {
		return nil, false, err
	}
	return &Lock{c: c, key: "lock:" + key, token: token}, true, nil
}

// releaseScript deletes the key only when we still own it, so a lock whose
// lease expired and was re-acquired by another worker is never deleted by the
// previous holder.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

func (l *Lock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	return releaseScript.Run(ctx, l.c.rdb, []string{l.key}, l.token).Err()
}

// Extend renews the lease. A review can legitimately take minutes; without
// this, a slow LLM call would let the lock lapse mid-job.
func (l *Lock) Extend(ctx context.Context, ttl time.Duration) error {
	if l == nil {
		return nil
	}
	return l.c.rdb.Expire(ctx, l.key, ttl).Err()
}

// Key builds a namespaced cache key from a content hash, which keeps prompt
// text and file paths out of Redis keys.
func Key(namespace string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%s:%s", namespace, hex.EncodeToString(h.Sum(nil))[:32])
}

func randomToken() string {
	b := make([]byte, 16)
	// crypto/rand via redis-free helper; failure here is not fatal because the
	// token only needs to be unlikely to collide.
	if _, err := randRead(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
