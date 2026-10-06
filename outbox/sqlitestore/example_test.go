package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/outbox"
	"github.com/nrynss/keel/outbox/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen stores two entries, ages one with a refused batch, and reads
// back what a pass would still owe.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-outbox-store-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "outbox.db")})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer db.Close()

	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}

	added := time.Date(2025, 4, 1, 9, 30, 0, 0, time.UTC)
	first := outbox.Entry{ID: "a103c27e8f4b41d0a5b6c7d8e9f00112", Payload: []byte("render-finished"), AddedAt: added}
	second := outbox.Entry{ID: "b203c27e8f4b41d0a5b6c7d8e9f00112", Payload: []byte("charge-recorded"), AddedAt: added.Add(time.Second)}
	if err := store.Add(ctx, first); err != nil {
		fmt.Println("add failed:", err)
		return
	}
	if err := store.Add(ctx, second); err != nil {
		fmt.Println("add failed:", err)
		return
	}
	if err := store.RecordFailures(ctx, []string{first.ID}); err != nil {
		fmt.Println("record failures failed:", err)
		return
	}

	live, err := store.Pending(ctx, 1, -1)
	if err != nil {
		fmt.Println("pending failed:", err)
		return
	}
	spent, err := store.Exhausted(ctx, 1)
	if err != nil {
		fmt.Println("exhausted failed:", err)
		return
	}
	fmt.Println(len(live), len(spent), string(spent[0].Payload), spent[0].Failures)
	// Output:
	// 1 1 render-finished 1
}
