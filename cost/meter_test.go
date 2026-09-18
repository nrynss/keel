package cost

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// mustMeter builds a meter over a fresh budget and ledger or ends the test.
func mustMeter(t *testing.T, limit Price) (*Meter, *Budget, *Ledger) {
	t.Helper()
	budget, err := NewBudget(limit)
	if err != nil {
		t.Fatalf("NewBudget(%d): %v", limit, err)
	}
	ledger := NewLedger()
	meter, err := NewMeter(budget, ledger)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	return meter, budget, ledger
}

// mustOwnerMeter builds a meter over one owner's account or ends the test.
func mustOwnerMeter(t *testing.T, k *KeyedBudget, owner string, ledger *Ledger) *Meter {
	t.Helper()
	account, err := k.Owner(owner)
	if err != nil {
		t.Fatalf("Owner(%q): %v", owner, err)
	}
	meter, err := NewMeter(account, ledger)
	if err != nil {
		t.Fatalf("NewMeter for owner %q: %v", owner, err)
	}
	return meter
}

// TestMeterSettlesTheMeasuredPrice runs a call that reports its own usage
// and pins the exact booked spend, headroom and ledger row after it. The
// ledger row carries the call's kind and reference, so both totals stay
// attributable.
func TestMeterSettlesTheMeasuredPrice(t *testing.T) {
	meter, budget, ledger := mustMeter(t, 100*Cent)
	usage, err := meter.Call(context.Background(), 30*Cent, "transcribe", "job-9",
		func(context.Context) (Usage, error) {
			return Usage{Price: 12 * Cent, Measured: true}, nil
		})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if want := (Usage{Price: 12 * Cent, Measured: true}); usage != want {
		t.Fatalf("Call usage = %+v, want %+v", usage, want)
	}
	if got := budget.Spent(); got != 12*Cent {
		t.Errorf("Spent() = %d, want %d", got, 12*Cent)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 88*Cent {
		t.Errorf("Remaining() = %d, %v, want %d", got, err, 88*Cent)
	}
	charges := ledger.Charges()
	if len(charges) != 1 {
		t.Fatalf("Charges() = %d rows, want 1", len(charges))
	}
	if want := (Charge{Kind: "transcribe", Units: 1, UnitPrice: 12 * Cent, Ref: "job-9"}); charges[0] != want {
		t.Errorf("charge = %+v, want %+v", charges[0], want)
	}
	if got, err := ledger.TotalByKind("transcribe"); err != nil || got != 12*Cent {
		t.Errorf("TotalByKind(transcribe) = %d, %v, want %d", got, err, 12*Cent)
	}
	if got, err := ledger.TotalForRef("job-9"); err != nil || got != 12*Cent {
		t.Errorf("TotalForRef(job-9) = %d, %v, want %d", got, err, 12*Cent)
	}
}

// TestMeterReleasesTheReservationWhenTheWorkFails pins that a returned error
// frees the whole reservation and books nothing, so the failed call leaves
// the budget exactly as it found it.
func TestMeterReleasesTheReservationWhenTheWorkFails(t *testing.T) {
	workFailed := errors.New("work failed")
	meter, budget, ledger := mustMeter(t, 100*Cent)
	usage, err := meter.Call(context.Background(), 30*Cent, "transcribe", "job-9",
		func(context.Context) (Usage, error) {
			return Usage{Price: 12 * Cent, Measured: true}, workFailed
		})
	if !errors.Is(err, workFailed) {
		t.Fatalf("Call error = %v, want %v", err, workFailed)
	}
	if usage != (Usage{}) {
		t.Errorf("Call usage = %+v, want the zero usage", usage)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("Spent() = %d, want 0", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 100*Cent {
		t.Errorf("Remaining() = %d, %v, want %d", got, err, 100*Cent)
	}
	if charges := ledger.Charges(); len(charges) != 0 {
		t.Errorf("Charges() = %d rows, want 0", len(charges))
	}
	// The freed reservation must be spendable again, not just invisible.
	if err := budget.Reserve(100 * Cent); err != nil {
		t.Errorf("Reserve after the failed call: %v", err)
	}
}

// TestMeterReleasesTheReservationWhenTheWorkPanics pins that a panic in work
// frees the whole reservation while it unwinds and keeps its own value, so
// the caller still sees the panic it caused.
func TestMeterReleasesTheReservationWhenTheWorkPanics(t *testing.T) {
	meter, budget, ledger := mustMeter(t, 100*Cent)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		meter.Call(context.Background(), 30*Cent, "transcribe", "job-9",
			func(context.Context) (Usage, error) {
				panic("kaboom")
			})
	}()
	if recovered != "kaboom" {
		t.Fatalf("recover() = %v, want the work's own panic", recovered)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("Spent() = %d, want 0", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 100*Cent {
		t.Errorf("Remaining() = %d, %v, want %d", got, err, 100*Cent)
	}
	if charges := ledger.Charges(); len(charges) != 0 {
		t.Errorf("Charges() = %d rows, want 0", len(charges))
	}
}

// TestMeterReleasesTheReservationWhenTheContextIsCancelled pins that a
// context cancelled mid-call frees the whole reservation and returns the
// context error the work stopped with.
func TestMeterReleasesTheReservationWhenTheContextIsCancelled(t *testing.T) {
	meter, budget, ledger := mustMeter(t, 100*Cent)
	ctx, cancel := context.WithCancel(context.Background())
	usage, err := meter.Call(ctx, 30*Cent, "transcribe", "job-9",
		func(ctx context.Context) (Usage, error) {
			cancel()
			<-ctx.Done()
			return Usage{}, ctx.Err()
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
	if usage != (Usage{}) {
		t.Errorf("Call usage = %+v, want the zero usage", usage)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("Spent() = %d, want 0", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 100*Cent {
		t.Errorf("Remaining() = %d, %v, want %d", got, err, 100*Cent)
	}
	if charges := ledger.Charges(); len(charges) != 0 {
		t.Errorf("Charges() = %d rows, want 0", len(charges))
	}
}

// TestMeterRefusesCallWhenContextAlreadyDone pins that a context that is
// done before the call never reserves at all, so the refusal takes no
// headroom even for a moment.
func TestMeterRefusesCallWhenContextAlreadyDone(t *testing.T) {
	meter, budget, _ := mustMeter(t, 100*Cent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	usage, err := meter.Call(ctx, 30*Cent, "transcribe", "job-9",
		func(context.Context) (Usage, error) {
			t.Error("work ran on a done context")
			return Usage{}, nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
	if usage != (Usage{}) {
		t.Errorf("Call usage = %+v, want the zero usage", usage)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
}

// TestMeterSettlesTheEstimateWhenUsageIsUnmeasured pins that a call which
// cannot report usage settles at its estimate, and that the returned usage
// says so through Measured.
func TestMeterSettlesTheEstimateWhenUsageIsUnmeasured(t *testing.T) {
	meter, budget, ledger := mustMeter(t, 100*Cent)
	usage, err := meter.Call(context.Background(), 30*Cent, "transcribe", "job-9",
		func(context.Context) (Usage, error) {
			return Usage{}, nil
		})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if want := (Usage{Price: 30 * Cent}); usage != want {
		t.Fatalf("Call usage = %+v, want %+v", usage, want)
	}
	if got := budget.Spent(); got != 30*Cent {
		t.Errorf("Spent() = %d, want %d", got, 30*Cent)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 70*Cent {
		t.Errorf("Remaining() = %d, %v, want %d", got, err, 70*Cent)
	}
	charges := ledger.Charges()
	if len(charges) != 1 {
		t.Fatalf("Charges() = %d rows, want 1", len(charges))
	}
	if want := (Charge{Kind: "transcribe", Units: 1, UnitPrice: 30 * Cent, Ref: "job-9"}); charges[0] != want {
		t.Errorf("charge = %+v, want %+v", charges[0], want)
	}
}

// TestMeterRefusesNilAccountAndNilLedger pins that neither half of a meter
// may be nil, because a meter that cannot bound or cannot record a call
// would only pretend to.
func TestMeterRefusesNilAccountAndNilLedger(t *testing.T) {
	if _, err := NewMeter(nil, NewLedger()); !errors.Is(err, ErrNilAccount) {
		t.Errorf("NewMeter(nil, ledger) error = %v, want ErrNilAccount", err)
	}
	budget, err := NewBudget(100 * Cent)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	if _, err := NewMeter(budget, nil); !errors.Is(err, ErrNilLedger) {
		t.Errorf("NewMeter(budget, nil) error = %v, want ErrNilLedger", err)
	}
}

// TestMeterRefusesANegativeMeasuredPrice pins that a call reporting a price
// below zero books nothing and holds nothing. The refusal arrives before
// any booking, so a later call sees the same headroom it would have seen
func TestMeterRefusesANegativeMeasuredPrice(t *testing.T) {
	meter, budget, ledger := mustMeter(t, 100*Cent)
	usage, err := meter.Call(context.Background(), 30*Cent, "transcribe", "job-9",
		func(context.Context) (Usage, error) {
			return Usage{Price: -2 * Cent, Measured: true}, nil
		})
	if !errors.Is(err, ErrNegativePrice) {
		t.Fatalf("Call error = %v, want ErrNegativePrice", err)
	}
	if usage != (Usage{}) {
		t.Errorf("Call usage = %+v, want the zero usage", usage)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("Spent() = %d, want 0", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 100*Cent {
		t.Errorf("Remaining() = %d, %v, want %d", got, err, 100*Cent)
	}
	if charges := ledger.Charges(); len(charges) != 0 {
		t.Errorf("Charges() = %d rows, want 0", len(charges))
	}
	if err := budget.Reserve(100 * Cent); err != nil {
		t.Errorf("Reserve after the refused call: %v", err)
	}
}

// TestMeterDrivesAnOwnerAccount pins that a meter over one owner's account
// bounds itself by the owner ceiling and by the global one. The owner's
// headroom comes out exact, and the global ceiling refuses at the exact
// cent the settled call left.
func TestMeterDrivesAnOwnerAccount(t *testing.T) {
	k := mustKeyed(t, 100*Cent)
	mustOwnerLimit(t, k, "a", 40*Cent)
	mustOwnerLimit(t, k, "b", 100*Cent)
	meter := mustOwnerMeter(t, k, "a", NewLedger())

	usage, err := meter.Call(context.Background(), 30*Cent, "transcribe", "job-9",
		func(context.Context) (Usage, error) {
			return Usage{Price: 12 * Cent, Measured: true}, nil
		})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if want := (Usage{Price: 12 * Cent, Measured: true}); usage != want {
		t.Fatalf("Call usage = %+v, want %+v", usage, want)
	}
	if got := mustKeyedRemaining(t, k, "a"); got != 28*Cent {
		t.Errorf("owner a headroom = %d, want %d", got, 28*Cent)
	}
	// The global pool holds the same 12 booked cents, so 88 cents is all
	// that any owner can still draw, owner b's bigger ceiling notwithstanding.
	if err := k.Reserve("b", 89*Cent); !errors.Is(err, ErrOverBudget) {
		t.Errorf("reserve 89c past the global pool error = %v, want ErrOverBudget", err)
	}
	if err := k.Reserve("b", 88*Cent); err != nil {
		t.Errorf("reserve 88c against the global pool: %v", err)
	}
}

// TestKeyedBudgetOwnerRefusesUnknownOwner pins that an account is refused
// for an owner no ceiling was set for, so a mistyped owner stops before any
// call runs.
func TestKeyedBudgetOwnerRefusesUnknownOwner(t *testing.T) {
	k := mustKeyed(t, 100*Cent)
	if _, err := k.Owner("ghost"); !errors.Is(err, ErrUnknownOwner) {
		t.Errorf("Owner(ghost) error = %v, want ErrUnknownOwner", err)
	}
}

// TestMeterConcurrentCallsSettleExactly runs concurrent calls against one
// meter and pins that every settle lands. The estimate and the measured
// price are one cent, so the full limit must come out booked, with nothing
// left reserved and one ledger row per call.
func TestMeterConcurrentCallsSettleExactly(t *testing.T) {
	const unit = Cent
	const calls = 400
	meter, budget, ledger := mustMeter(t, calls*unit)

	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			usage, err := meter.Call(context.Background(), unit, "transcribe", "job-9",
				func(context.Context) (Usage, error) {
					return Usage{Price: unit, Measured: true}, nil
				})
			if err != nil {
				t.Errorf("Call: %v", err)
				return
			}
			if usage != (Usage{Price: unit, Measured: true}) {
				t.Errorf("Call usage = %+v, want %d measured", usage, unit)
			}
		}()
	}
	wg.Wait()

	if got := budget.Spent(); got != calls*unit {
		t.Errorf("Spent() = %d, want %d", got, calls*unit)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 0 {
		t.Errorf("Remaining() = %d, %v, want 0", got, err)
	}
	if got, err := ledger.Total(); err != nil || got != calls*unit {
		t.Errorf("ledger total = %d, %v, want %d", got, err, calls*unit)
	}
	if charges := ledger.Charges(); len(charges) != calls {
		t.Errorf("Charges() = %d rows, want %d", len(charges), calls)
	}
}

// TestOwnerAccountsConcurrentCallsSettleExactly runs concurrent calls
// through two owners' accounts of one keyed budget and pins that neither an
// owner ceiling nor the global one loses an update. Both owners must come
// out booked to the exact cent their calls settled.
func TestOwnerAccountsConcurrentCallsSettleExactly(t *testing.T) {
	const unit = Cent
	const perOwner = 200
	k := mustKeyed(t, 2*perOwner*unit)
	mustOwnerLimit(t, k, "a", perOwner*unit)
	mustOwnerLimit(t, k, "b", perOwner*unit)
	ledger := NewLedger()
	owners := map[string]*Meter{
		"a": mustOwnerMeter(t, k, "a", ledger),
		"b": mustOwnerMeter(t, k, "b", ledger),
	}

	var wg sync.WaitGroup
	for i := 0; i < 2*perOwner; i++ {
		owner := "a"
		if i%2 == 1 {
			owner = "b"
		}
		meter := owners[owner]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := meter.Call(context.Background(), unit, "transcribe", "job-9",
				func(context.Context) (Usage, error) {
					return Usage{Price: unit, Measured: true}, nil
				}); err != nil {
				t.Errorf("Call for owner %s: %v", owner, err)
			}
		}()
	}
	wg.Wait()

	for owner := range owners {
		if got := mustKeyedRemaining(t, k, owner); got != 0 {
			t.Errorf("owner %s headroom = %d, want 0", owner, got)
		}
	}
	if got, err := ledger.Total(); err != nil || got != 2*perOwner*unit {
		t.Errorf("ledger total = %d, %v, want %d", got, err, 2*perOwner*unit)
	}
	if charges := ledger.Charges(); len(charges) != 2*perOwner {
		t.Errorf("Charges() = %d rows, want %d", len(charges), 2*perOwner)
	}
}
