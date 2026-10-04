package throttle_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/nrynss/keel/throttle"
)

func ExampleRetry() {
	limited := errors.New("rate limited")
	tries := 0
	err := throttle.Retry(context.Background(), throttle.Config{Attempts: 3, Backoff: 1}, func(err error) bool {
		return errors.Is(err, limited)
	}, func() error {
		tries++
		if tries < 2 {
			return limited
		}
		return nil
	})
	fmt.Println(err)
	fmt.Println(tries)
	// Output:
	// <nil>
	// 2
}

func ExampleNote() {
	limited := errors.New("limited")
	cfg := throttle.Config{Attempts: 1}
	classify := func(err error) bool { return errors.Is(err, limited) }
	err := throttle.Retry(context.Background(), cfg, classify, func() error {
		return limited
	})
	fmt.Println(throttle.Note(err, cfg, classify))
	// Output:
	// still throttled after 1 attempt: limited
}
