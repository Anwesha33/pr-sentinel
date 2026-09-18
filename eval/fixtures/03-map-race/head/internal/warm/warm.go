package warm

import (
	"context"
	"sync"
)

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

// Warm now fetches keys concurrently to cut cold-start time.
func (w *Warmer) Warm(ctx context.Context, keys []string) error {
	var wg sync.WaitGroup
	var firstErr error

	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			v, err := w.fetcher.Fetch(ctx, key)
			if err != nil {
				firstErr = err
				return
			}
			w.values[key] = v
		}(k)
	}

	wg.Wait()
	return firstErr
}

func (w *Warmer) Get(k string) (string, bool) {
	v, ok := w.values[k]
	return v, ok
}
