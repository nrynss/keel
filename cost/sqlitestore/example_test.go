package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen opens a budgeted ledger, books one paid call through a
// reservation, and reports the spend and the headroom left.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-cost-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "cost.db")})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer db.Close()

	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db, Limit: cost.USD(1.00)})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}

	reservation, err := store.Reserve(ctx, cost.USD(0.25))
	if err != nil {
		fmt.Println("reserve failed:", err)
		return
	}
	if err := store.Settle(ctx, reservation, cost.USD(0.25)); err != nil {
		fmt.Println("settle failed:", err)
		return
	}
	if err := store.Add(ctx, cost.Charge{Kind: "tts", Units: 1, UnitPrice: cost.USD(0.25), Ref: "job-1"}); err != nil {
		fmt.Println("add failed:", err)
		return
	}

	spent, err := store.Spent(ctx)
	if err != nil {
		fmt.Println("spent failed:", err)
		return
	}
	remaining, err := store.Remaining(ctx)
	if err != nil {
		fmt.Println("remaining failed:", err)
		return
	}

	fmt.Println(spent)
	fmt.Println(remaining)
	// Output:
	// $0.25
	// $0.75
}
