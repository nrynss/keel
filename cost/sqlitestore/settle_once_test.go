package sqlitestore_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// freshScalar reads one integer column over a fresh connection to the file,
// so the pin reports what the database holds rather than what the store
// reports about itself.
func freshScalar(t *testing.T, path, query string) int {
	t.Helper()
	var n int
	if err := openFresh(t, path).QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q over a fresh connection: %v", query, err)
	}
	return n
}

// TestStoreSettleOnceBooksAReferenceOnce pins the settle-once contract on
// the durable store. The first settle of a kind and reference pair books
// exactly as Settle does. A repeat frees the hold it was handed, books
// nothing, and reports booked false. The pins read a fresh connection, so
// the pair table and the booked spend are what the file holds, not what the
// store's own handles report.
func TestStoreSettleOnceBooksAReferenceOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withLimit(1000))
	ctx := t.Context()

	hold, err := store.Reserve(ctx, 30)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	booked, err := store.SettleOnce(ctx, hold, 12, "gen", "job-9")
	if err != nil {
		t.Fatalf("SettleOnce: %v", err)
	}
	if !booked {
		t.Fatalf("first SettleOnce booked = false, want true")
	}
	if got, err := store.Spent(ctx); err != nil || got != 12 {
		t.Errorf("Spent() = %d, %v, want 12", got, err)
	}

	// The repeat books nothing, and the hold it frees is spendable again.
	repeat, err := store.Reserve(ctx, 30)
	if err != nil {
		t.Fatalf("Reserve for the repeat: %v", err)
	}
	booked, err = store.SettleOnce(ctx, repeat, 12, "gen", "job-9")
	if err != nil {
		t.Fatalf("repeat SettleOnce: %v", err)
	}
	if booked {
		t.Errorf("repeat SettleOnce booked = true, want false")
	}
	if got, err := store.Spent(ctx); err != nil || got != 12 {
		t.Errorf("Spent() after the repeat = %d, %v, want 12", got, err)
	}
	if got, err := store.Reserved(ctx); err != nil || got != 0 {
		t.Errorf("Reserved() after the repeat = %d, %v, want 0, the repeat frees its hold", got, err)
	}
	if got := freshScalar(t, path, `SELECT spent_nd FROM cost_budget WHERE id = 1`); got != 12 {
		t.Errorf("file spent_nd = %d, want 12", got)
	}
	if got := freshCount(t, path, "cost_settle_once"); got != 1 {
		t.Errorf("file settle-once rows = %d, want 1", got)
	}
	if got := freshCount(t, path, "cost_reservation"); got != 0 {
		t.Errorf("file reservation rows = %d, want 0, both holds went back", got)
	}
	if got := freshCount(t, path, "cost_settle"); got != 1 {
		t.Errorf("file settle history rows = %d, want 1, the repeat writes no history row", got)
	}
}

// TestStoreSettleOnceAnswersOneTrueAcrossHandles drives two stores over one
// file, the way two processes share one database, and pins that the pair
// settles exactly once between them. The loser keeps no hold, because its
// reservation went back with its refusal.
func TestStoreSettleOnceAnswersOneTrueAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	first := openStore(t, path, withLimit(1000))
	second := openStore(t, path, withLimit(1000))
	ctx := t.Context()

	firstHold, err := first.Reserve(ctx, 30)
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	secondHold, err := second.Reserve(ctx, 30)
	if err != nil {
		t.Fatalf("second Reserve: %v", err)
	}
	booked, err := first.SettleOnce(ctx, firstHold, 12, "gen", "job-9")
	if err != nil || !booked {
		t.Fatalf("first store SettleOnce booked = %v, %v, want true", booked, err)
	}
	booked, err = second.SettleOnce(ctx, secondHold, 12, "gen", "job-9")
	if err != nil {
		t.Fatalf("second store SettleOnce: %v", err)
	}
	if booked {
		t.Fatalf("second store SettleOnce booked = true, want false")
	}
	for name, store := range map[string]*sqlitestore.Store{"first": first, "second": second} {
		if got, err := store.Spent(ctx); err != nil || got != 12 {
			t.Errorf("%s store Spent() = %d, %v, want 12", name, got, err)
		}
		if got, err := store.Reserved(ctx); err != nil || got != 0 {
			t.Errorf("%s store Reserved() = %d, %v, want 0", name, got, err)
		}
	}
	if got := freshScalar(t, path, `SELECT spent_nd FROM cost_budget WHERE id = 1`); got != 12 {
		t.Errorf("file spent_nd = %d, want 12, one booking across both handles", got)
	}
}

// TestStoreSettleOnceRefusesAnEmptyReference pins the one refusal the pair
// key needs. An empty reference would collapse every once settle of a kind
// onto one durable row forever, so the store refuses it before anything is
// written, and the hold and the ceiling stay as they were. The refusal
// matches the same sentinel the in-memory accounts refuse with, beside the
// store's own.
func TestStoreSettleOnceRefusesAnEmptyReference(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	hold, err := store.Reserve(ctx, 30)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	_, err = store.SettleOnce(ctx, hold, 12, "gen", "")
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("SettleOnce with an empty reference error = %v, want ErrInvalid", err)
	}
	if !errors.Is(err, cost.ErrEmptyReference) {
		t.Fatalf("SettleOnce with an empty reference error = %v, want ErrEmptyReference", err)
	}
	if got, err := store.Spent(ctx); err != nil || got != 0 {
		t.Errorf("Spent() = %d, %v, want 0, the refusal commits nothing", got, err)
	}
	if err := store.Release(ctx, hold); err != nil {
		t.Errorf("Release after the refusal: %v", err)
	}
}

// TestKeyedSettleOnceBooksAReferenceOnce pins the settle-once contract on
// the keyed budget over the durable store. The first settle books on the
// owner ceiling and on the global one. A repeat frees the hold, books
// nothing on either ceiling, and the pair table holds one row.
func TestKeyedSettleOnceBooksAReferenceOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withLimit(1000))
	keyed := sqlitestore.NewKeyedBudget(store)
	ctx := t.Context()
	if err := keyed.SetLimit(ctx, "alice", 500); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}

	hold, err := keyed.Reserve(ctx, "alice", 30)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	booked, err := keyed.SettleOnce(ctx, "alice", hold, 12, "gen", "job-9")
	if err != nil {
		t.Fatalf("SettleOnce: %v", err)
	}
	if !booked {
		t.Fatalf("first SettleOnce booked = false, want true")
	}
	if got, err := keyed.Remaining(ctx, "alice"); err != nil || got != 488 {
		t.Errorf("owner remaining = %d, %v, want 488", got, err)
	}

	repeat, err := keyed.Reserve(ctx, "alice", 30)
	if err != nil {
		t.Fatalf("Reserve for the repeat: %v", err)
	}
	booked, err = keyed.SettleOnce(ctx, "alice", repeat, 12, "gen", "job-9")
	if err != nil {
		t.Fatalf("repeat SettleOnce: %v", err)
	}
	if booked {
		t.Errorf("repeat SettleOnce booked = true, want false")
	}
	if got, err := keyed.Remaining(ctx, "alice"); err != nil || got != 488 {
		t.Errorf("owner remaining after the repeat = %d, %v, want 488", got, err)
	}
	if got, err := store.Spent(ctx); err != nil || got != 12 {
		t.Errorf("global Spent() = %d, %v, want 12", got, err)
	}
	if got, err := store.Reserved(ctx); err != nil || got != 0 {
		t.Errorf("global Reserved() = %d, %v, want 0", got, err)
	}
	if got := freshScalar(t, path, `SELECT spent_nd FROM cost_owner_budget WHERE owner = 'alice'`); got != 12 {
		t.Errorf("file owner spent_nd = %d, want 12", got)
	}
	if got := freshCount(t, path, "cost_settle_once"); got != 1 {
		t.Errorf("file settle-once rows = %d, want 1", got)
	}
}

// TestKeyedSettleOnceRefusesUnknownOwnerAndEmptyRef pins the two refusals
// the keyed settle once carries. An unknown owner is named before anything
// is freed or booked, and an empty reference is refused like the store's
// own.
func TestKeyedSettleOnceRefusesUnknownOwnerAndEmptyRef(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	keyed := sqlitestore.NewKeyedBudget(store)
	ctx := t.Context()
	if err := keyed.SetLimit(ctx, "alice", 500); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	hold, err := keyed.Reserve(ctx, "alice", 30)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := keyed.SettleOnce(ctx, "ghost", hold, 12, "gen", "job-9"); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("SettleOnce for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	_, err = keyed.SettleOnce(ctx, "alice", hold, 12, "gen", "")
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("SettleOnce with an empty reference error = %v, want ErrInvalid", err)
	}
	if !errors.Is(err, cost.ErrEmptyReference) {
		t.Errorf("SettleOnce with an empty reference error = %v, want ErrEmptyReference", err)
	}
	if _, err := keyed.SettleOnce(ctx, "", hold, 12, "gen", "job-9"); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("SettleOnce with an empty owner error = %v, want ErrInvalid", err)
	}
	if got, err := keyed.Remaining(ctx, "alice"); err != nil || got != 470 {
		t.Errorf("owner remaining = %d, %v, want 470, no refusal freed or booked anything", got, err)
	}
}
