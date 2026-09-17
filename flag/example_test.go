package flag_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/flag"
	"github.com/nrynss/keel/flag/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleStore declares a runtime flag, reads its default before anyone has
// set it, and then flips it and reads the new value back.
func ExampleStore() {
	dir, err := os.MkdirTemp("", "keel-flag-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "flag.db")})
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

	// A declaration carries the default an operator falls back to, so a
	// missing row is never a silent off.
	paid := flag.Bool{Name: "paid_calls", Default: true, Help: "switch paid calls on"}
	before, _, err := store.Bool(ctx, paid)
	if err != nil {
		fmt.Println("read failed:", err)
		return
	}

	if _, err := store.SetBool(ctx, paid, false); err != nil {
		fmt.Println("set failed:", err)
		return
	}
	after, _, err := store.Bool(ctx, paid)
	if err != nil {
		fmt.Println("read failed:", err)
		return
	}

	fmt.Println(before)
	fmt.Println(after)
	// Output:
	// true
	// false
}
