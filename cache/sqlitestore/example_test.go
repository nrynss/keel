package sqlitestore_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/cache"
	"github.com/nrynss/keel/cache/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// Example keeps one paid generation in SQLite. The maker runs on the first
// call, and a second cache over a reopened store serves the stored entry.
func Example() {
	dir, err := os.MkdirTemp("", "keel-cache-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	path := filepath.Join(dir, "cache.db")

	type verdictKey struct {
		Level    string
		Title    string
		Checksum string
	}
	key, err := cache.Key(verdictKey{Level: "verdict", Title: "A policy", Checksum: "cc44"})
	if err != nil {
		fmt.Println("key failed:", err)
		return
	}

	makes := 0
	verdict := func(ctx context.Context) (cache.Result, error) {
		makes++
		return cache.Result{Payload: []byte(`{"verdict":"clean"}`), ContentType: "application/json", ChargeRef: "charge-2"}, nil
	}

	// Each open takes its own handle, the way a restart would. The store
	// never closes the handle it is given, so the example does.
	firstDB, err := sqlite.Open(ctx, sqlite.Config{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Println("first db failed:", err)
		return
	}
	defer firstDB.Close() // the example owns both handles it opened
	firstStore, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: firstDB})
	if err != nil {
		fmt.Println("first store failed:", err)
		return
	}
	firstCache, err := cache.New(cache.Config{Store: firstStore})
	if err != nil {
		fmt.Println("first cache failed:", err)
		return
	}

	first, err := firstCache.GetOrMake(ctx, key, verdict)
	if err != nil {
		fmt.Println("first make failed:", err)
		return
	}

	secondDB, err := sqlite.Open(ctx, sqlite.Config{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Println("second db failed:", err)
		return
	}
	defer secondDB.Close() // the second handle closes once, like the first
	secondStore, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: secondDB})
	if err != nil {
		fmt.Println("second store failed:", err)
		return
	}
	secondCache, err := cache.New(cache.Config{Store: secondStore})
	if err != nil {
		fmt.Println("second cache failed:", err)
		return
	}

	second, err := secondCache.GetOrMake(ctx, key, verdict)
	if err != nil {
		fmt.Println("second call failed:", err)
		return
	}

	fmt.Println(string(first.Payload), first.ChargeRef)
	fmt.Println(string(second.Payload), second.ChargeRef)
	fmt.Println("makes:", makes)
	// Output:
	// {"verdict":"clean"} charge-2
	// {"verdict":"clean"} charge-2
	// makes: 1
}
