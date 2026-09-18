package sqlitestore_test

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/lease"
	leasesql "github.com/nrynss/keel/lease/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the test stamps leases with. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// openDB opens the database file at path and closes it at cleanup.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// openStore applies the lease schema at path and returns the store.
func openStore(t *testing.T, path string) *leasesql.Store {
	t.Helper()
	store, err := leasesql.Open(t.Context(), leasesql.Config{
		DB:     openDB(t, path),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open lease store: %v", err)
	}
	return store
}

// openFresh opens the database file at path over a new connection with no
// pragmas from any package. It is the independent observer the tests query.
func openFresh(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// sampleLease builds one open lease row with distinct field values.
func sampleLease(id string) lease.Lease {
	return lease.Lease{
		ID:        id,
		State:     lease.StateOpen,
		OpenedAt:  base,
		ExpiresAt: base.Add(time.Hour),
		Estimate:  100,
		Kind:      "session",
		Owner:     "team-a",
	}
}

// TestCreateAndGetRoundTrip pins that every field survives the write and
// the read unchanged, including the nanosecond stamps.
func TestCreateAndGetRoundTrip(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "lease.db"))
	ctx := t.Context()
	want := sampleLease("row-1")
	want.State = lease.StateClosed
	want.ClosedAt = base.Add(10 * time.Minute)
	want.Settled = 120
	want.Reported = 110
	want.Reconciled = true
	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, "row-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != want {
		t.Fatalf("lease = %+v, want %+v", got, want)
	}
}

// TestGetUnknownLease pins the sentinel for a missing row.
func TestGetUnknownLease(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "lease.db"))
	if _, err := store.Get(t.Context(), "missing"); !errors.Is(err, lease.ErrUnknownLease) {
		t.Fatalf("get missing error = %v, want ErrUnknownLease", err)
	}
}

// TestUpdateMovesTheRow pins that Update writes every field back, and that
// an unknown id reports ErrUnknownLease.
func TestUpdateMovesTheRow(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "lease.db"))
	ctx := t.Context()
	if err := store.Create(ctx, sampleLease("row-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := sampleLease("row-1")
	want.State = lease.StateExpired
	if err := store.Update(ctx, want); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.Get(ctx, "row-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != want {
		t.Fatalf("lease = %+v, want %+v", got, want)
	}
	if err := store.Update(ctx, sampleLease("missing")); !errors.Is(err, lease.ErrUnknownLease) {
		t.Fatalf("update missing error = %v, want ErrUnknownLease", err)
	}
}

// TestMigrationIsIdempotent pins that a second open over the same file
// keeps the rows and changes nothing.
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	first := openStore(t, path)
	if err := first.Create(t.Context(), sampleLease("row-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	second := openStore(t, path)
	got, err := second.Get(t.Context(), "row-1")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got != sampleLease("row-1") {
		t.Fatalf("lease = %+v, want %+v", got, sampleLease("row-1"))
	}
	var n int
	if err := openFresh(t, path).QueryRow("SELECT COUNT(*) FROM lease_entry").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestCreateRefusesAnEmptyID pins that a lease with no key never lands.
func TestCreateRefusesAnEmptyID(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "lease.db"))
	if err := store.Create(t.Context(), lease.Lease{}); !errors.Is(err, lease.ErrInvalid) {
		t.Fatalf("create without an id error = %v, want ErrInvalid", err)
	}
}

// TestClosedLeaseKeepsItsNumbers pins that the settled and reported prices
// land in their own columns, so a later read tells them apart.
func TestClosedLeaseKeepsItsNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	store := openStore(t, path)
	ctx := t.Context()
	row := sampleLease("row-1")
	row.State = lease.StateClosed
	row.ClosedAt = base.Add(10 * time.Minute)
	row.Settled = cost.Price(150)
	row.Reported = cost.Price(120)
	row.Reconciled = true
	if err := store.Create(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}
	var settled, reported, reconciled int64
	err := openFresh(t, path).QueryRow(
		"SELECT settled_nd, reported_nd, reconciled FROM lease_entry WHERE id = 'row-1'").Scan(&settled, &reported, &reconciled)
	if err != nil {
		t.Fatalf("read raw columns: %v", err)
	}
	if settled != 150 || reported != 120 || reconciled != 1 {
		t.Fatalf("columns = %d %d %d, want 150 120 1", settled, reported, reconciled)
	}
}
