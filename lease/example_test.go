package lease_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/lease"
	leasesql "github.com/nrynss/keel/lease/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleManager opens one lease, closes it at the reported price, and
// reconciles the provider number, printing the three settled prices.
func ExampleManager() {
	dir, err := os.MkdirTemp("", "keel-lease-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "lease.db")})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer db.Close()
	store, err := leasesql.Open(ctx, leasesql.Config{DB: db, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}
	budget, err := cost.NewBudget(cost.USD(1.00))
	if err != nil {
		fmt.Println("budget failed:", err)
		return
	}
	meter, err := cost.NewMeter(budget, cost.NewLedger())
	if err != nil {
		fmt.Println("meter failed:", err)
		return
	}
	quota, err := lease.NewQuota(4)
	if err != nil {
		fmt.Println("quota failed:", err)
		return
	}
	manager, err := lease.New(lease.Config{
		Quota: quota,
		Meter: meter,
		Store: store,
		Cap:   time.Hour,
		Kind:  "session",
		Log:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		fmt.Println("manager failed:", err)
		return
	}
	opened, err := manager.Open(ctx, "", cost.USD(0.10), "")
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	closed, err := manager.Close(ctx, opened.ID, cost.USD(0.12))
	if err != nil {
		fmt.Println("close failed:", err)
		return
	}
	reconciled, err := manager.Reconcile(ctx, opened.ID, cost.USD(0.15))
	if err != nil {
		fmt.Println("reconcile failed:", err)
		return
	}
	fmt.Println(opened.Estimate)
	fmt.Println(closed.Settled)
	fmt.Println(reconciled.Settled, reconciled.Reported)
	// Output:
	// $0.10
	// $0.12
	// $0.15 $0.12
}
