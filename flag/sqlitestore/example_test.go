package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/flag"
	"github.com/nrynss/keel/flag/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen opens a flag store, flips one flag, and reads it back with the
// change time it was stamped with.
func ExampleOpen() {
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

	model := flag.Text{Name: "model", Default: "standard", Help: "which model tier serves calls"}
	if _, err := store.SetText(ctx, model, "premium"); err != nil {
		fmt.Println("set failed:", err)
		return
	}
	value, _, err := store.Text(ctx, model)
	if err != nil {
		fmt.Println("read failed:", err)
		return
	}
	fmt.Println(value)
	// Output:
	// premium
}
