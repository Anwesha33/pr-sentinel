package warm

import "context"

type Fetcher interface {
	Fetch(ctx context.Context, key string) (string, error)
}

// Warmer pre-populates a local map of values.
type Warmer struct {
	fetcher Fetcher
	values  map[string]string
}

func New(f Fetcher) *Warmer {
	return &Warmer{fetcher: f, values: map[string]string{}}
}

func (w *Warmer) Warm(ctx context.Context, keys []string) error {
	for _, k := range keys {
		v, err := w.fetcher.Fetch(ctx, k)
		if err != nil {
			return err
		}
		w.values[k] = v
	}
	return nil
}

func (w *Warmer) Get(k string) (string, bool) {
	v, ok := w.values[k]
	return v, ok
}
