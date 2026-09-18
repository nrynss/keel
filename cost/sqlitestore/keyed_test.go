package sqlitestore_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// openKeyed opens a cost store and a keyed budget over the file at path.
func openKeyed(t *testing.T, path string, opts ...func(*sqlitestore.Config)) (*sqlitestore.KeyedBudget, *sqlitestore.Store) {
	t.Helper()
	store := openStore(t, path, opts...)
	return sqlitestore.NewKeyedBudget(store), store
}

// mustOwnerCeiling gives an owner its ceiling or ends the test.
func mustOwnerCeiling(t *testing.T, keyed *sqlitestore.KeyedBudget, owner string, limit cost.Price) {
	t.Helper()
	if err := keyed.SetLimit(t.Context(), owner, limit); err != nil {
		t.Fatalf("set owner limit %q = %d: %v", owner, limit, err)
	}
}

// mustKeyedHold reserves an owner's hold or ends the test.
func mustKeyedHold(t *testing.T, keyed *sqlitestore.KeyedBudget, owner string, estimate cost.Price) sqlitestore.Reservation {
	t.Helper()
	reservation, err := keyed.Reserve(t.Context(), owner, estimate)
	if err != nil {
		t.Fatalf("reserve %d for %q: %v", estimate, owner, err)
	}
	return reservation
}

// mustOwnerRemaining reads an owner's headroom or ends the test.
func mustOwnerRemaining(t *testing.T, keyed *sqlitestore.KeyedBudget, owner string) cost.Price {
	t.Helper()
	remaining, err := keyed.Remaining(t.Context(), owner)
	if err != nil {
		t.Fatalf("remaining for %q: %v", owner, err)
	}
	return remaining
}

// TestKeyedBudgetDividesOneStoreBetweenOwners drives two owners through one
// keyed budget over one store and pins the three bounds a consumer relies
// on. Exhausting one owner leaves the other's headroom untouched, and the
// global ceiling refuses when the owners' sum passes it. A released hold
// returns headroom to the owner that released it and to nobody else.
func TestKeyedBudgetDividesOneStoreBetweenOwners(t *testing.T) {
	tests := []struct {
		name  string
		run   func(t *testing.T, keyed *sqlitestore.KeyedBudget, store *sqlitestore.Store)
		wantA cost.Price
		wantB cost.Price
	}{
		{
			name: "exhausting one owner leaves the other untouched",
			run: func(t *testing.T, keyed *sqlitestore.KeyedBudget, store *sqlitestore.Store) {
				mustOwnerCeiling(t, keyed, "a", 300)
				mustOwnerCeiling(t, keyed, "b", 1000)
				mustKeyedHold(t, keyed, "a", 300)
				mustKeyedHold(t, keyed, "b", 400)
				if _, err := keyed.Reserve(t.Context(), "a", 1); !errors.Is(err, cost.ErrOverBudget) {
					t.Fatalf("reserve past a's ceiling error = %v, want ErrOverBudget", err)
				}
				// The refusal committed nothing, so b can still draw its
				// share of the shared pool even though a is exhausted.
				mustKeyedHold(t, keyed, "b", 300)
			},
			wantA: 0,
			wantB: 300,
		},
		{
			name: "the global ceiling refuses when the sum passes it",
			run: func(t *testing.T, keyed *sqlitestore.KeyedBudget, store *sqlitestore.Store) {
				mustOwnerCeiling(t, keyed, "a", 1000)
				mustOwnerCeiling(t, keyed, "b", 1000)
				mustKeyedHold(t, keyed, "a", 600)
				mustKeyedHold(t, keyed, "b", 400)
				// Both owners sit far under their own ceilings, but the
				// shared pool is full, so the next reserve anywhere fails.
				if _, err := keyed.Reserve(t.Context(), "b", 1); !errors.Is(err, cost.ErrOverBudget) {
					t.Fatalf("reserve past the global ceiling error = %v, want ErrOverBudget", err)
				}
			},
			wantA: 400,
			wantB: 600,
		},
		{
			name: "a released hold returns headroom to the right owner",
			run: func(t *testing.T, keyed *sqlitestore.KeyedBudget, store *sqlitestore.Store) {
				mustOwnerCeiling(t, keyed, "a", 500)
				mustOwnerCeiling(t, keyed, "b", 500)
				hold := mustKeyedHold(t, keyed, "a", 300)
				mustKeyedHold(t, keyed, "b", 300)
				if err := keyed.Release(t.Context(), "a", hold); err != nil {
					t.Fatalf("release a: %v", err)
				}
				// a is back at its full ceiling and b kept exactly the
				// headroom it had before the release.
				mustKeyedHold(t, keyed, "a", 500)
				mustKeyedHold(t, keyed, "b", 100)
			},
			wantA: 0,
			wantB: 100,
		},
		{
			name: "a settle books the actual price on the owner's own account",
			run: func(t *testing.T, keyed *sqlitestore.KeyedBudget, store *sqlitestore.Store) {
				mustOwnerCeiling(t, keyed, "a", 500)
				mustOwnerCeiling(t, keyed, "b", 500)
				hold := mustKeyedHold(t, keyed, "a", 300)
				// The call cost less than the estimate, so only the actual
				// price stays booked, and it lands on the global pool too.
				if err := keyed.Settle(t.Context(), "a", hold, 250); err != nil {
					t.Fatalf("settle a: %v", err)
				}
				if spent, _ := store.Spent(t.Context()); spent != 250 {
					t.Errorf("global spent = %d, want 250", spent)
				}
				mustKeyedHold(t, keyed, "b", 300)
			},
			wantA: 250,
			wantB: 200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyed, store := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
			tt.run(t, keyed, store)
			if got := mustOwnerRemaining(t, keyed, "a"); got != tt.wantA {
				t.Errorf("remaining for a = %d, want %d", got, tt.wantA)
			}
			if got := mustOwnerRemaining(t, keyed, "b"); got != tt.wantB {
				t.Errorf("remaining for b = %d, want %d", got, tt.wantB)
			}
		})
	}
}

// TestKeyedBudgetConcurrentReservesNeverOverspend drives concurrent
// reservations for several owners at one durable keyed budget and pins that
// neither an owner ceiling nor the global one loses an update. The owners'
// ceilings sum past the global one, so the shared pool binds first, and the
// per-owner headroom must still come out exact.
func TestKeyedBudgetConcurrentReservesNeverOverspend(t *testing.T) {
	keyed, _ := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(500))
	ctx := t.Context()
	owners := []string{"a", "b", "c"}
	const ownerShare = 250
	for _, owner := range owners {
		mustOwnerCeiling(t, keyed, owner, ownerShare)
	}

	const attempts = 900
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted = map[string]int{}
	)
	for i := 0; i < attempts; i++ {
		owner := owners[i%len(owners)]
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			_, err := keyed.Reserve(ctx, owner, 1)
			switch {
			case err == nil:
				mu.Lock()
				granted[owner]++
				mu.Unlock()
			case !errors.Is(err, cost.ErrOverBudget):
				t.Errorf("reserve for %q error = %v, want nil or ErrOverBudget", owner, err)
			}
		}(owner)
	}
	wg.Wait()

	total := 0
	for _, owner := range owners {
		got := granted[owner]
		if got > ownerShare {
			t.Errorf("owner %q holds %d reservations, want at most %d", owner, got, ownerShare)
		}
		if want := cost.Price(ownerShare - got); mustOwnerRemaining(t, keyed, owner) != want {
			t.Errorf("owner %q headroom does not match its %d holds", owner, got)
		}
		total += got
	}
	if total != 500 {
		t.Errorf("granted %d holds in total, want the full global 500", total)
	}
	// The shared pool is exhausted, so a reserve for any owner fails no
	// matter how much of its own share it never used.
	for _, owner := range owners {
		if _, err := keyed.Reserve(t.Context(), owner, 1); !errors.Is(err, cost.ErrOverBudget) {
			t.Errorf("reserve for %q past the global pool error = %v, want ErrOverBudget", owner, err)
		}
	}
}

// TestKeyedExpiryReturnsTheOwnerHeadroom advances the injected clock past the
// TTL and shows an expired hold stops holding both its owner's ceiling and
// the global one.
func TestKeyedExpiryReturnsTheOwnerHeadroom(t *testing.T) {
	clock := &testClock{at: base}
	keyed, store := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(1000), withClock(clock.now), withTTL(time.Hour))
	mustOwnerCeiling(t, keyed, "a", 400)
	mustKeyedHold(t, keyed, "a", 300)
	if got := mustOwnerRemaining(t, keyed, "a"); got != 100 {
		t.Fatalf("remaining for a while held = %d, want 100", got)
	}

	clock.advance(2 * time.Hour)
	if got := mustOwnerRemaining(t, keyed, "a"); got != 400 {
		t.Errorf("remaining for a after the hold expired = %d, want 400", got)
	}
	if remaining, _ := store.Remaining(t.Context()); remaining != 1000 {
		t.Errorf("global remaining after the hold expired = %d, want 1000", remaining)
	}
	if _, err := keyed.Reserve(t.Context(), "a", 400); err != nil {
		t.Errorf("reserve the full owner ceiling after expiry: %v", err)
	}
}

// TestKeyedCeilingsSurviveARestart writes an owner ceiling and a booked
// spend, closes the handle, and reopens the same file to read both back. An
// owner's ceiling is durable, and the reopened store's own ceiling rides
// with the new open as before.
func TestKeyedCeilingsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store, db := openAt(t, path)
	keyed := sqlitestore.NewKeyedBudget(store)
	mustOwnerCeiling(t, keyed, "a", 400)
	hold := mustKeyedHold(t, keyed, "a", 300)
	if err := keyed.Settle(t.Context(), "a", hold, 200); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}

	reopened := openStore(t, path, withLimit(2000))
	keyed = sqlitestore.NewKeyedBudget(reopened)
	if got := mustOwnerRemaining(t, keyed, "a"); got != 200 {
		t.Fatalf("remaining for a after the restart = %d, want 200", got)
	}
	if remaining, _ := reopened.Remaining(t.Context()); remaining != 1800 {
		t.Errorf("global remaining after the restart = %d, want 1800", remaining)
	}
}

// TestKeyedBudgetRefusals pins the refusals a caller must classify. An owner
// no ceiling was set for matches cost.ErrUnknownOwner, a ceiling below zero
// matches cost.ErrNegativeLimit, and the empty key matches ErrInvalid. The
// empty key refuses because it names the unkeyed budget the store keeps.
func TestKeyedBudgetRefusals(t *testing.T) {
	keyed, _ := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	if err := keyed.SetLimit(ctx, "a", -1); !errors.Is(err, cost.ErrNegativeLimit) {
		t.Errorf("set owner limit below zero error = %v, want ErrNegativeLimit", err)
	}
	if _, err := keyed.Reserve(ctx, "ghost", 1); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("reserve for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if err := keyed.Settle(ctx, "ghost", sqlitestore.Reservation{}, 1); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("settle for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if err := keyed.Release(ctx, "ghost", sqlitestore.Reservation{ID: "missing"}); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("release for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if _, err := keyed.Remaining(ctx, "ghost"); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("remaining for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if err := keyed.SetLimit(ctx, "", 1); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("set a ceiling on the empty key error = %v, want ErrInvalid", err)
	}
	if _, err := keyed.Reserve(ctx, "", 1); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("reserve on the empty key error = %v, want ErrInvalid", err)
	}
	if _, err := keyed.Remaining(ctx, ""); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("remaining on the empty key error = %v, want ErrInvalid", err)
	}
	// A zero reservation names no hold, so releasing it stays a no-op for
	// any key, as Store.Release does.
	if err := keyed.Release(ctx, "", sqlitestore.Reservation{}); err != nil {
		t.Errorf("release a zero reservation: %v", err)
	}
}

// TestKeyedCancelledContextStopsTheWrite pins that a cancelled context stops
// a keyed ceiling write and a keyed reserve, and that neither stores
// anything.
func TestKeyedCancelledContextStopsTheWrite(t *testing.T) {
	keyed, _ := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	mustOwnerCeiling(t, keyed, "a", 500)
	ctx := cancelledContext(t)
	if err := keyed.SetLimit(ctx, "a", 400); !errors.Is(err, context.Canceled) {
		t.Errorf("set owner limit on a cancelled context error = %v, want context.Canceled", err)
	}
	if _, err := keyed.Reserve(ctx, "a", 100); !errors.Is(err, context.Canceled) {
		t.Errorf("reserve on a cancelled context error = %v, want context.Canceled", err)
	}
	if got := mustOwnerRemaining(t, keyed, "a"); got != 500 {
		t.Errorf("remaining for a after the cancelled calls = %d, want 500", got)
	}
	if _, err := keyed.Reserve(t.Context(), "a", 100); err != nil {
		t.Errorf("reserve after the cancelled calls: %v", err)
	}
}
