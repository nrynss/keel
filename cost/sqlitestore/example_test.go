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

// ExampleOpen opens a budgeted ledger and books one paid call through a
// meter the durable budget backs. The meter reserves, runs the work, settles
// the measured price and records the charge in the same file, so no
// application writes the reserve, settle and release by hand.
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
	meter, err := cost.NewContextMeter(store.Account(), store)
	if err != nil {
		fmt.Println("meter failed:", err)
		return
	}

	if _, err := meter.Call(ctx, cost.USD(0.30), "tts", "job-1",
		func(context.Context) (cost.Usage, error) {
			return cost.Usage{Price: cost.USD(0.25), Measured: true}, nil
		}); err != nil {
		fmt.Println("call failed:", err)
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
	total, err := store.TotalForRef(ctx, "job-1")
	if err != nil {
		fmt.Println("total failed:", err)
		return
	}

	fmt.Println(spent)
	fmt.Println(remaining)
	fmt.Println(total)
	// Output:
	// $0.25
	// $0.75
	// $0.25
}

// ExampleKeyedBudget_Owner backs a meter with one owner's share of a durable
// keyed budget. The account is bound to the owner, so the call counts
// against that owner's ceiling and the global one, and a call past the
// owner's ceiling is refused before its work runs.
func ExampleKeyedBudget_Owner() {
	dir, err := os.MkdirTemp("", "keel-cost-owner-example-")
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

	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db, Limit: cost.USD(10.00)})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}
	keyed := sqlitestore.NewKeyedBudget(store)
	if err := keyed.SetLimit(ctx, "team-a", cost.USD(0.40)); err != nil {
		fmt.Println("owner limit failed:", err)
		return
	}
	account, err := keyed.Owner(ctx, "team-a")
	if err != nil {
		fmt.Println("owner account failed:", err)
		return
	}
	meter, err := cost.NewContextMeter(account, store)
	if err != nil {
		fmt.Println("meter failed:", err)
		return
	}

	work := func(context.Context) (cost.Usage, error) {
		return cost.Usage{Price: cost.USD(0.30), Measured: true}, nil
	}
	if _, err := meter.Call(ctx, cost.USD(0.30), "tts", "job-1", work); err != nil {
		fmt.Println("call failed:", err)
		return
	}
	_, err = meter.Call(ctx, cost.USD(0.30), "tts", "job-2", work)
	fmt.Println(errors.Is(err, cost.ErrOverBudget))

	remaining, err := keyed.Remaining(ctx, "team-a")
	if err != nil {
		fmt.Println("remaining failed:", err)
		return
	}
	fmt.Println(remaining)
	// Output:
	// true
	// $0.10
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
