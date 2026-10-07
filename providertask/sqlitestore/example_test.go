package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/providertask/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// Example walks one claim through its life: the key is claimed, the task
// id is recorded before any polling would start, and the verdict lands
// where a restart reads it back.
func Example() {
	dir, err := os.MkdirTemp("", "keel-providertask-sqlitestore-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "tasks.db")})
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

	claim, created, err := store.Claim(ctx, "order-1", time.Now())
	fmt.Println(created, claim.TaskID, claim.State, err)

	// The provider call would sit between these two lines, and its task id
	// is durable before anything polls it.
	if err := store.Record(ctx, "order-1", "task-1"); err != nil {
		fmt.Println("record failed:", err)
		return
	}
	claim, _ = store.Get(ctx, "order-1")
	fmt.Println(claim.TaskID)

	if err := store.Finish(ctx, "order-1", providertask.StateSucceeded, ""); err != nil {
		fmt.Println("finish failed:", err)
		return
	}
	claim, _ = store.Get(ctx, "order-1")
	fmt.Println(claim.State)

	// The key is one row, so a second claim of the same key creates
	// nothing.
	_, created, _ = store.Claim(ctx, "order-1", time.Now())
	fmt.Println(created)
	// Output:
	// true  running <nil>
	// task-1
	// succeeded
	// false
}
