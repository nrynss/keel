package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// ExampleStore_Grant funds a pool of a provider's credits that lapse, and
// refills it once a month with a key that makes the refill idempotent. A
// restart or a double post cannot fund the same window twice, and spend
// draws from the grant that expires soonest first.
func ExampleStore_Grant() {
	dir, err := os.MkdirTemp("", "keel-cost-grant-example-")
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

	credit := cost.Denomination{Name: "provider.unit", Minor: 1}
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{
		DB:           db,
		Limit:        1 << 40,
		Denomination: credit,
	})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}

	// The current month's window. The key is the window's start, so a
	// restart or a double post of this month changes nothing, and next
	// month's key is new.
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := store.Grant(ctx, 1000, month.Add(32*24*time.Hour), month.Format("2006-01")); err != nil {
		fmt.Println("grant failed:", err)
		return
	}
	// A second post of the same window changes nothing.
	if err := store.Grant(ctx, 1000, month.Add(32*24*time.Hour), month.Format("2006-01")); err != nil {
		if !errors.Is(err, sqlitestore.ErrGrantRepeated) {
			fmt.Println("grant failed:", err)
			return
		}
	}

	reservation, err := store.Reserve(ctx, 250)
	if err != nil {
		fmt.Println("reserve failed:", err)
		return
	}
	if err := store.Settle(ctx, reservation, 250); err != nil {
		fmt.Println("settle failed:", err)
		return
	}
	balance, err := store.Balance(ctx)
	if err != nil {
		fmt.Println("balance failed:", err)
		return
	}

	fmt.Printf("%d %s left\n", balance, credit.Name)
	// Output:
	// 750 provider.unit left
}
