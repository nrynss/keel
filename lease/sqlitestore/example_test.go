package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/lease"
	leasesql "github.com/nrynss/keel/lease/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen writes one open lease, reads it back, and prints the stored
// owner and kind.
func ExampleOpen() {
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

	store, err := leasesql.Open(ctx, leasesql.Config{DB: db})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}

	stamp := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	row := lease.Lease{
		ID:        "row-1",
		State:     lease.StateOpen,
		OpenedAt:  stamp,
		ExpiresAt: stamp.Add(time.Hour),
		Estimate:  100,
		Kind:      "session",
		Owner:     "team-a",
	}
	if err := store.Create(ctx, row); err != nil {
		fmt.Println("create failed:", err)
		return
	}
	got, err := store.Get(ctx, "row-1")
	if err != nil {
		fmt.Println("get failed:", err)
		return
	}
	fmt.Println(got.Owner, got.Kind, got.State)
	// Output:
	// team-a session open
}
