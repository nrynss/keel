package sqlitestore_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// mustStoreMeter builds a meter over the store's own budget that records
// into the store, or ends the test.
func mustStoreMeter(t *testing.T, store *sqlitestore.Store) *cost.Meter {
	t.Helper()
	meter, err := cost.NewContextMeter(store.Account(), store)
	if err != nil {
		t.Fatalf("NewContextMeter: %v", err)
	}
	return meter
}

// mustReserved reads the store's outstanding holds or ends the test.
func mustReserved(t *testing.T, store *sqlitestore.Store) cost.Price {
	t.Helper()
	reserved, err := store.Reserved(t.Context())
	if err != nil {
		t.Fatalf("reserved: %v", err)
	}
	return reserved
}

// mustCharges reads the store's recorded charges or ends the test.
func mustCharges(t *testing.T, store *sqlitestore.Store) []cost.Charge {
	t.Helper()
	charges, err := store.Charges(t.Context())
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	return charges
}

// TestStoreBacksAMeter pins the issue's reproduction the other way round: a
// durable store backs a meter, which books the measured price, frees the
// hold and records the charge in the same file.
func TestStoreBacksAMeter(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar))
	meter := mustStoreMeter(t, store)
	usage, err := meter.Call(t.Context(), 30*cost.Cent, "tts", "job-1",
		func(context.Context) (cost.Usage, error) {
			return cost.Usage{Price: 12 * cost.Cent, Measured: true}, nil
		})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if want := (cost.Usage{Price: 12 * cost.Cent, Measured: true}); usage != want {
		t.Fatalf("Call usage = %+v, want %+v", usage, want)
	}
	if got := mustSpent(t, store); got != 12*cost.Cent {
		t.Errorf("spent = %d, want %d", got, 12*cost.Cent)
	}
	if got := mustReserved(t, store); got != 0 {
		t.Errorf("reserved = %d, want 0", got)
	}
	want := cost.Charge{Kind: "tts", Units: 1, UnitPrice: 12 * cost.Cent, Ref: "job-1"}
	if charges := mustCharges(t, store); len(charges) != 1 || charges[0] != want {
		t.Errorf("charges = %+v, want [%+v]", charges, want)
	}
}

// TestStoreMeterBooksAfterALateCancel pins the rule the hand-written
// sequence kept getting wrong. A caller that cancels after the work ran
// still gets the booking and the charge on the durable store, because the
// meter settles and records on a context detached from the cancel.
func TestStoreMeterBooksAfterALateCancel(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar))
	meter := mustStoreMeter(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := meter.Call(ctx, 30*cost.Cent, "tts", "job-1",
		func(context.Context) (cost.Usage, error) {
			cancel()
			return cost.Usage{Price: 20 * cost.Cent, Measured: true}, nil
		}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := mustSpent(t, store); got != 20*cost.Cent {
		t.Errorf("spent = %d, want %d", got, 20*cost.Cent)
	}
	if got := mustReserved(t, store); got != 0 {
		t.Errorf("reserved = %d, want 0", got)
	}
	if charges := mustCharges(t, store); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1", len(charges))
	}
}

// TestStoreMeterReleasesOnEveryFailure pins that a failed, cancelled or
// panicking call frees its durable hold at once and books nothing, so no
// hold waits out its expiry.
func TestStoreMeterReleasesOnEveryFailure(t *testing.T) {
	workErr := errors.New("provider said no")
	tests := []struct {
		name string
		call func(t *testing.T, meter *cost.Meter)
	}{
		{
			name: "work fails",
			call: func(t *testing.T, meter *cost.Meter) {
				if _, err := meter.Call(t.Context(), 30*cost.Cent, "tts", "job-1",
					func(context.Context) (cost.Usage, error) { return cost.Usage{}, workErr }); !errors.Is(err, workErr) {
					t.Fatalf("Call error = %v, want the work error", err)
				}
			},
		},
		{
			name: "context cancelled mid-work",
			call: func(t *testing.T, meter *cost.Meter) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if _, err := meter.Call(ctx, 30*cost.Cent, "tts", "job-1",
					func(ctx context.Context) (cost.Usage, error) {
						cancel()
						return cost.Usage{}, ctx.Err()
					}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Call error = %v, want context.Canceled", err)
				}
			},
		},
		{
			name: "work panics",
			call: func(t *testing.T, meter *cost.Meter) {
				defer func() {
					if got := recover(); got != "boom" {
						t.Errorf("recovered %v, want boom", got)
					}
				}()
				_, _ = meter.Call(t.Context(), 30*cost.Cent, "tts", "job-1",
					func(context.Context) (cost.Usage, error) { panic("boom") })
			},
		},
		{
			name: "once without a reference",
			call: func(t *testing.T, meter *cost.Meter) {
				if _, err := meter.Call(t.Context(), 30*cost.Cent, "tts", "",
					func(context.Context) (cost.Usage, error) { return cost.Usage{}, nil },
					cost.Once()); !errors.Is(err, cost.ErrEmptyReference) {
					t.Fatalf("Call error = %v, want ErrEmptyReference", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar))
			tt.call(t, mustStoreMeter(t, store))
			if got := mustReserved(t, store); got != 0 {
				t.Errorf("reserved = %d, want 0, the hold must go back at once", got)
			}
			if got := mustSpent(t, store); got != 0 {
				t.Errorf("spent = %d, want 0", got)
			}
			if charges := mustCharges(t, store); len(charges) != 0 {
				t.Errorf("charges = %+v, want none", charges)
			}
		})
	}
}

// TestStoreMeterOnceSurvivesAReopen pins that Once dedupes through the
// durable pair table, so a second meter over a reopened file books a
// repeated reference nothing and records nothing.
func TestStoreMeterOnceSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	first, db := openAt(t, path, withLimit(cost.Dollar))
	work := func(context.Context) (cost.Usage, error) {
		return cost.Usage{Price: 10 * cost.Cent, Measured: true}, nil
	}
	if _, err := mustStoreMeter(t, first).Call(t.Context(), 30*cost.Cent, "tts", "job-1", work, cost.Once()); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second := openStore(t, path, withLimit(cost.Dollar))
	usage, err := mustStoreMeter(t, second).Call(t.Context(), 30*cost.Cent, "tts", "job-1", work, cost.Once())
	if err != nil {
		t.Fatalf("repeat Call: %v", err)
	}
	if usage != (cost.Usage{}) {
		t.Errorf("repeat usage = %+v, want the zero usage", usage)
	}
	if got := mustSpent(t, second); got != 10*cost.Cent {
		t.Errorf("spent = %d, want %d", got, 10*cost.Cent)
	}
	if got := mustReserved(t, second); got != 0 {
		t.Errorf("reserved = %d, want 0", got)
	}
	if charges := mustCharges(t, second); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1", len(charges))
	}
}

// TestKeyedOwnerAccountRefusesBadOwners pins that Owner refuses the empty
// key and an owner with no ceiling before any call runs.
func TestKeyedOwnerAccountRefusesBadOwners(t *testing.T) {
	keyed, _ := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"))
	if _, err := keyed.Owner(t.Context(), ""); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("Owner(empty) error = %v, want ErrInvalid", err)
	}
	if _, err := keyed.Owner(t.Context(), "ghost"); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("Owner(ghost) error = %v, want ErrUnknownOwner", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	mustOwnerCeiling(t, keyed, "a", 100)
	if _, err := keyed.Owner(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Owner on a cancelled context error = %v, want context.Canceled", err)
	}
}

// TestKeyedOwnerAccountBindsTheOwner pins that a meter over one owner's
// durable account books against that owner and the global pool only, and
// that a forgotten owner's account refuses its next call.
func TestKeyedOwnerAccountBindsTheOwner(t *testing.T) {
	keyed, store := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar))
	mustOwnerCeiling(t, keyed, "a", 40*cost.Cent)
	mustOwnerCeiling(t, keyed, "b", 40*cost.Cent)
	account, err := keyed.Owner(t.Context(), "a")
	if err != nil {
		t.Fatalf("Owner(a): %v", err)
	}
	meter, err := cost.NewContextMeter(account, store)
	if err != nil {
		t.Fatalf("NewContextMeter: %v", err)
	}
	if _, err := meter.Call(t.Context(), 30*cost.Cent, "tts", "job-1",
		func(context.Context) (cost.Usage, error) {
			return cost.Usage{Price: 25 * cost.Cent, Measured: true}, nil
		}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := mustOwnerRemaining(t, keyed, "a"); got != 15*cost.Cent {
		t.Errorf("remaining for a = %d, want %d", got, 15*cost.Cent)
	}
	if got := mustOwnerRemaining(t, keyed, "b"); got != 40*cost.Cent {
		t.Errorf("remaining for b = %d, want %d", got, 40*cost.Cent)
	}
	if got := mustSpent(t, store); got != 25*cost.Cent {
		t.Errorf("global spent = %d, want %d", got, 25*cost.Cent)
	}
	if _, err := meter.Call(t.Context(), 20*cost.Cent, "tts", "job-2",
		func(context.Context) (cost.Usage, error) {
			t.Error("work ran past the owner's ceiling")
			return cost.Usage{}, nil
		}); !errors.Is(err, cost.ErrOverBudget) {
		t.Errorf("Call past a's ceiling error = %v, want ErrOverBudget", err)
	}
	if err := keyed.ForgetOwner(t.Context(), "a"); err != nil {
		t.Fatalf("ForgetOwner(a): %v", err)
	}
	if _, err := meter.Call(t.Context(), cost.Cent, "tts", "job-3",
		func(context.Context) (cost.Usage, error) { return cost.Usage{}, nil }); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Errorf("Call for a forgotten owner error = %v, want ErrUnknownOwner", err)
	}
}

// TestKeyedOwnerMeterNeverOverspendsUnderConcurrency races many calls for
// one owner whose ceiling admits exactly five of them. Every admitted call
// parks inside its work until every caller has either entered work or been
// refused, so the count of admissions is decided by the ceiling alone and
// not by how fast the winners settle.
func TestKeyedOwnerMeterNeverOverspendsUnderConcurrency(t *testing.T) {
	keyed, store := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar))
	mustOwnerCeiling(t, keyed, "a", 50*cost.Cent)
	account, err := keyed.Owner(t.Context(), "a")
	if err != nil {
		t.Fatalf("Owner(a): %v", err)
	}
	meter, err := cost.NewContextMeter(account, store)
	if err != nil {
		t.Fatalf("NewContextMeter: %v", err)
	}
	const callers = 16
	var decided sync.WaitGroup
	decided.Add(callers)
	gate := make(chan struct{})
	var admitted, refused atomic.Int64
	var done sync.WaitGroup
	for range callers {
		done.Go(func() {
			entered := false
			_, err := meter.Call(t.Context(), 10*cost.Cent, "tts", "job",
				func(context.Context) (cost.Usage, error) {
					entered = true
					admitted.Add(1)
					decided.Done()
					<-gate
					return cost.Usage{Price: 10 * cost.Cent, Measured: true}, nil
				})
			if !entered {
				if !errors.Is(err, cost.ErrOverBudget) {
					t.Errorf("refused Call error = %v, want ErrOverBudget", err)
				}
				refused.Add(1)
				decided.Done()
				return
			}
			if err != nil {
				t.Errorf("admitted Call: %v", err)
			}
		})
	}
	decided.Wait()
	close(gate)
	done.Wait()
	if admitted.Load() != 5 || refused.Load() != callers-5 {
		t.Errorf("admitted, refused = %d, %d, want 5, %d", admitted.Load(), refused.Load(), callers-5)
	}
	if got := mustOwnerRemaining(t, keyed, "a"); got != 0 {
		t.Errorf("remaining for a = %d, want 0", got)
	}
	if got := mustReserved(t, store); got != 0 {
		t.Errorf("reserved = %d, want 0", got)
	}
	if charges := mustCharges(t, store); len(charges) != 5 {
		t.Errorf("charges = %d rows, want 5", len(charges))
	}
}

// TestStoreAccountHoldMatchesTheReservation pins that the hold the account
// hands out carries the reservation's ID, amount and expiry, so the meter
// passes back exactly the row the reserve wrote.
func TestStoreAccountHoldMatchesTheReservation(t *testing.T) {
	clock := &testClock{at: base}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar), withClock(clock.now))
	account := store.Account()
	if got := account.Denomination(); got != store.Denomination() {
		t.Errorf("Denomination() = %+v, want %+v", got, store.Denomination())
	}
	h, err := account.Reserve(t.Context(), 30*cost.Cent)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if h.ID == "" || h.Amount != 30*cost.Cent || !h.ExpiresAt.After(base) {
		t.Errorf("hold = %+v, want an ID, the amount and an expiry after %v", h, base)
	}
	if got := mustReserved(t, store); got != 30*cost.Cent {
		t.Errorf("reserved = %d, want %d", got, 30*cost.Cent)
	}
	if err := account.Release(t.Context(), h); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := mustReserved(t, store); got != 0 {
		t.Errorf("reserved after release = %d, want 0", got)
	}
}

// TestNilStoreAccountIsRefused pins that a nil store hands out a nil
// account, so the meter constructor refuses it instead of a later call
// failing on it.
func TestNilStoreAccountIsRefused(t *testing.T) {
	var store *sqlitestore.Store
	if _, err := cost.NewContextMeter(store.Account(), cost.NewLedger()); !errors.Is(err, cost.ErrNilAccount) {
		t.Errorf("NewContextMeter(nil store's account) error = %v, want ErrNilAccount", err)
	}
}
