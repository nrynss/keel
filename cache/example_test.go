package cache_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/nrynss/keel/cache"
)

// exampleStore is the smallest cache.Store, enough to show the flow of a
// make and a hit.
type exampleStore struct {
	mu   sync.Mutex
	rows map[string]cache.Entry
}

func (s *exampleStore) Get(_ context.Context, key string) (cache.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.rows[key]
	if !ok {
		return cache.Entry{}, cache.ErrNotFound
	}
	return entry, nil
}

func (s *exampleStore) Put(_ context.Context, key string, entry cache.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key] = entry
	return nil
}

// ExampleCache_GetOrMake keys one generation on its inputs, runs the paid
// make once, and reads the stored entry on the second call.
func ExampleCache_GetOrMake() {
	ctx := context.Background()

	c, err := cache.New(cache.Config{Store: &exampleStore{rows: map[string]cache.Entry{}}})
	if err != nil {
		fmt.Println("new failed:", err)
		return
	}

	// The key struct covers the provider, the model, the parameters and
	// the input hashes, so one request hashes to one key.
	type renderKey struct {
		Provider string
		Model    string
		Kind     string
		InputSHA string
	}
	key, err := cache.Key(renderKey{Provider: "image", Model: "render-2", Kind: "cover", InputSHA: "aa11bb22"})
	if err != nil {
		fmt.Println("key failed:", err)
		return
	}

	makes := 0
	render := func(ctx context.Context) (cache.Result, error) {
		makes++
		// A real maker copies its bytes somewhere durable before it
		// returns, because a provider result URL expires.
		return cache.Result{
			Payload:     []byte(`{"verdict":"keep"}`),
			ContentType: "application/json",
			ChargeRef:   "charge-1",
		}, nil
	}

	first, err := c.GetOrMake(ctx, key, render)
	if err != nil {
		fmt.Println("first failed:", err)
		return
	}
	second, err := c.GetOrMake(ctx, key, render)
	if err != nil {
		fmt.Println("second failed:", err)
		return
	}
	fmt.Println(first.ContentType, string(first.Payload), first.ChargeRef)
	fmt.Println(second.ContentType, string(second.Payload), second.ChargeRef)
	fmt.Println("makes:", makes)
	// Output:
	// application/json {"verdict":"keep"} charge-1
	// application/json {"verdict":"keep"} charge-1
	// makes: 1
}
