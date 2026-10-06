package outbox_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/outbox"
	outboxsql "github.com/nrynss/keel/outbox/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// printSink is an outbox.Sink an application writes when the transport is
// its own. A strict sink records the ids it serves, so a replayed entry can
// be dropped by id.
type printSink struct{}

// Deliver prints one line per entry, which stands in for the remote call.
func (printSink) Deliver(ctx context.Context, batch []outbox.Entry) error {
	for _, e := range batch {
		fmt.Printf("delivered %q with %d recorded failures\n", e.Payload, e.Failures)
	}
	return nil
}

// ExampleOpen writes two events durably, replays them to the sink in one
// pass, and shows the queue empty afterwards.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-outbox-example-")
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

	store, err := outboxsql.Open(ctx, outboxsql.Config{DB: db})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}
	box, err := outbox.Open(ctx, outbox.Config{Store: store, Sink: printSink{}})
	if err != nil {
		fmt.Println("open outbox failed:", err)
		return
	}

	if _, err := box.Add(ctx, []byte("render-finished")); err != nil {
		fmt.Println("add failed:", err)
		return
	}
	if _, err := box.Add(ctx, []byte("charge-recorded")); err != nil {
		fmt.Println("add failed:", err)
		return
	}

	sum, err := box.Flush(ctx)
	if err != nil {
		fmt.Println("flush failed:", err)
		return
	}
	pending, err := box.Pending(ctx)
	if err != nil {
		fmt.Println("pending failed:", err)
		return
	}
	fmt.Println(sum.Delivered, len(pending))
	// Output:
	// delivered "render-finished" with 0 recorded failures
	// delivered "charge-recorded" with 0 recorded failures
	// 2 0
}
