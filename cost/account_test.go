package cost

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ctxKey keys the value the recording account looks for, so a test can tell
// that a detached context still carries the caller's values.
type ctxKey struct{}

// accountStep is one method call a recordingAccount observed.
type accountStep struct {
	op string
	// hold is the hold the method received or, for reserve, returned.
	hold Hold
	// actual is the price a settle booked.
	actual Price
	// done reports whether the context the method received was already
	// done when it ran.
	done bool
	// value is the ctxKey value the method's context carried.
	value any
}

// recordingAccount is a ContextAccount that hands out numbered holds and
// records every call with the state of the context it received. It frees
// and books nothing real, so a test reads the sequence of calls alone.
type recordingAccount struct {
	mu         sync.Mutex
	steps      []accountStep
	next       int
	reserveErr error
	settleErr  error
	releaseErr error
	settled    map[[2]string]bool
	// onSettle, when not nil, runs inside Settle and SettleOnce before the
	// booking, so a test can cancel the caller's context at that point.
	onSettle func()
}

func (a *recordingAccount) record(ctx context.Context, step accountStep) {
	step.done = ctx.Err() != nil
	step.value = ctx.Value(ctxKey{})
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steps = append(a.steps, step)
}

func (a *recordingAccount) calls() []accountStep {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]accountStep(nil), a.steps...)
}

func (a *recordingAccount) Denomination() Denomination { return Denomination{} }

func (a *recordingAccount) Reserve(ctx context.Context, estimate Price) (Hold, error) {
	if a.reserveErr != nil {
		a.record(ctx, accountStep{op: "reserve"})
		return Hold{}, a.reserveErr
	}
	a.mu.Lock()
	a.next++
	h := Hold{ID: "hold-" + strconv.Itoa(a.next), Amount: estimate, ExpiresAt: time.Unix(1_700_000_000, 0)}
	a.mu.Unlock()
	a.record(ctx, accountStep{op: "reserve", hold: h})
	return h, nil
}

func (a *recordingAccount) Settle(ctx context.Context, h Hold, actual Price) error {
	if a.onSettle != nil {
		a.onSettle()
	}
	a.record(ctx, accountStep{op: "settle", hold: h, actual: actual})
	return a.settleErr
}

func (a *recordingAccount) SettleOnce(ctx context.Context, h Hold, actual Price, kind, ref string) (bool, error) {
	if a.onSettle != nil {
		a.onSettle()
	}
	a.record(ctx, accountStep{op: "settle once", hold: h, actual: actual})
	if a.settleErr != nil {
		return false, a.settleErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settled == nil {
		a.settled = make(map[[2]string]bool)
	}
	pair := [2]string{kind, ref}
	if a.settled[pair] {
		return false, nil
	}
	a.settled[pair] = true
	return true, nil
}

func (a *recordingAccount) Release(ctx context.Context, h Hold) error {
	a.record(ctx, accountStep{op: "release", hold: h})
	return a.releaseErr
}

// contextSink records the charges it receives with the state of the context
// each Add saw.
type contextSink struct {
	mu      sync.Mutex
	charges []Charge
	done    []bool
	values  []any
}

func (s *contextSink) Add(ctx context.Context, c Charge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.charges = append(s.charges, c)
	s.done = append(s.done, ctx.Err() != nil)
	s.values = append(s.values, ctx.Value(ctxKey{}))
	return nil
}

// mustContextMeter builds a meter over account and sink or ends the test.
func mustContextMeter(t *testing.T, account ContextAccount, sink ChargeSink) *Meter {
	t.Helper()
	meter, err := NewContextMeter(account, sink)
	if err != nil {
		t.Fatalf("NewContextMeter: %v", err)
	}
	return meter
}

// TestContextMeterPassesTheReservedHoldBack pins that the meter hands the
// exact hold the account returned to the settle, ID and expiry included, so
// a durable account can find the row its reserve wrote.
func TestContextMeterPassesTheReservedHoldBack(t *testing.T) {
	account := &recordingAccount{}
	sink := &contextSink{}
	meter := mustContextMeter(t, account, sink)
	usage, err := meter.Call(context.Background(), 30*Cent, "tts", "job-1",
		func(context.Context) (Usage, error) {
			return Usage{Price: 12 * Cent, Measured: true}, nil
		})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if want := (Usage{Price: 12 * Cent, Measured: true}); usage != want {
		t.Fatalf("Call usage = %+v, want %+v", usage, want)
	}
	steps := account.calls()
	if len(steps) != 2 || steps[0].op != "reserve" || steps[1].op != "settle" {
		t.Fatalf("account calls = %+v, want reserve then settle", steps)
	}
	if steps[1].hold != steps[0].hold {
		t.Errorf("settle hold = %+v, want the reserved hold %+v", steps[1].hold, steps[0].hold)
	}
	if steps[1].hold.Amount != 30*Cent || steps[1].actual != 12*Cent {
		t.Errorf("settle = hold amount %d, actual %d, want %d and %d",
			steps[1].hold.Amount, steps[1].actual, 30*Cent, 12*Cent)
	}
	if want := (Charge{Kind: "tts", Units: 1, UnitPrice: 12 * Cent, Ref: "job-1"}); len(sink.charges) != 1 || sink.charges[0] != want {
		t.Errorf("sink charges = %+v, want [%+v]", sink.charges, want)
	}
}

// TestContextMeterBooksAfterALateCancel pins the rule a durable account
// depends on. A caller that cancels after the work returned still gets its
// settle and its charge, because the settle and the sink run on a context
// detached from the call's cancellation that still carries its values.
func TestContextMeterBooksAfterALateCancel(t *testing.T) {
	for _, once := range []bool{false, true} {
		name := "settle"
		if once {
			name = "settle once"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "trace-7"))
			defer cancel()
			account := &recordingAccount{onSettle: cancel}
			sink := &contextSink{}
			meter := mustContextMeter(t, account, sink)
			var opts []CallOption
			if once {
				opts = append(opts, Once())
			}
			usage, err := meter.Call(ctx, 30*Cent, "tts", "job-1",
				func(context.Context) (Usage, error) {
					return Usage{Price: 20 * Cent, Measured: true}, nil
				}, opts...)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if usage.Price != 20*Cent {
				t.Errorf("Call usage = %+v, want price %d", usage, 20*Cent)
			}
			if ctx.Err() == nil {
				t.Fatal("the caller's context was never cancelled, the test proves nothing")
			}
			steps := account.calls()
			if len(steps) != 2 || steps[1].op != name {
				t.Fatalf("account calls = %+v, want reserve then %s", steps, name)
			}
			if steps[1].done {
				t.Errorf("%s ran on a done context, want one detached from the cancel", name)
			}
			if steps[1].value != "trace-7" {
				t.Errorf("%s context value = %v, want the caller's trace-7", name, steps[1].value)
			}
			if len(sink.charges) != 1 || sink.done[0] || sink.values[0] != "trace-7" {
				t.Errorf("sink = %d charges, done %v, values %v, want one charge on a live context carrying trace-7",
					len(sink.charges), sink.done, sink.values)
			}
		})
	}
}

// TestContextMeterReleasesOnADetachedContext pins that a call whose context
// is cancelled mid-work frees its hold on a live context, so a durable
// account deletes the row at once rather than leaving it to expire.
func TestContextMeterReleasesOnADetachedContext(t *testing.T) {
	account := &recordingAccount{}
	meter := mustContextMeter(t, account, &contextSink{})
	ctx, cancel := context.WithCancel(context.Background())
	_, err := meter.Call(ctx, 30*Cent, "tts", "job-1",
		func(ctx context.Context) (Usage, error) {
			cancel()
			return Usage{}, ctx.Err()
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
	steps := account.calls()
	if len(steps) != 2 || steps[1].op != "release" {
		t.Fatalf("account calls = %+v, want reserve then release", steps)
	}
	if steps[1].done {
		t.Error("release ran on a done context, want one detached from the cancel")
	}
	if steps[1].hold != steps[0].hold {
		t.Errorf("release hold = %+v, want the reserved hold %+v", steps[1].hold, steps[0].hold)
	}
}

// TestContextMeterReleaseOnEveryFailure walks every failure before the
// settle commits and pins that each one releases the reserved hold exactly
// once and reports the failure that caused it.
func TestContextMeterReleaseOnEveryFailure(t *testing.T) {
	workErr := errors.New("provider said no")
	settleErr := errors.New("disk full")
	tests := []struct {
		name      string
		settleErr error
		work      Work
		opts      []CallOption
		ref       string
		want      error
	}{
		{
			name: "work fails",
			work: func(context.Context) (Usage, error) { return Usage{}, workErr },
			ref:  "job-1",
			want: workErr,
		},
		{
			name: "negative measured price",
			work: func(context.Context) (Usage, error) { return Usage{Price: -1, Measured: true}, nil },
			ref:  "job-1",
			want: ErrNegativePrice,
		},
		{
			name:      "settle refused",
			settleErr: settleErr,
			work:      func(context.Context) (Usage, error) { return Usage{}, nil },
			ref:       "job-1",
			want:      settleErr,
		},
		{
			name:      "settle once refused",
			settleErr: settleErr,
			work:      func(context.Context) (Usage, error) { return Usage{}, nil },
			opts:      []CallOption{Once()},
			ref:       "job-1",
			want:      settleErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &recordingAccount{settleErr: tt.settleErr}
			sink := &contextSink{}
			meter := mustContextMeter(t, account, sink)
			usage, err := meter.Call(context.Background(), 30*Cent, "tts", tt.ref, tt.work, tt.opts...)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Call error = %v, want %v", err, tt.want)
			}
			if usage != (Usage{}) {
				t.Errorf("Call usage = %+v, want the zero usage", usage)
			}
			steps := account.calls()
			last := steps[len(steps)-1]
			if last.op != "release" || last.hold != steps[0].hold {
				t.Errorf("account calls = %+v, want the reserved hold released last", steps)
			}
			releases := 0
			for _, s := range steps {
				if s.op == "release" {
					releases++
				}
			}
			if releases != 1 {
				t.Errorf("releases = %d, want 1", releases)
			}
			if len(sink.charges) != 0 {
				t.Errorf("sink charges = %+v, want none", sink.charges)
			}
		})
	}
}

// TestContextMeterJoinsAFailedRelease pins that a release the account fails
// is reported beside the failure that caused it, so a hold that may still
// stand until its expiry is never silent, and the cause still matches.
func TestContextMeterJoinsAFailedRelease(t *testing.T) {
	workErr := errors.New("provider said no")
	releaseErr := errors.New("database is closed")
	account := &recordingAccount{releaseErr: releaseErr}
	meter := mustContextMeter(t, account, &contextSink{})
	_, err := meter.Call(context.Background(), 30*Cent, "tts", "job-1",
		func(context.Context) (Usage, error) { return Usage{}, workErr })
	if !errors.Is(err, workErr) {
		t.Errorf("Call error = %v, want it to match the work error", err)
	}
	if !errors.Is(err, releaseErr) {
		t.Errorf("Call error = %v, want it to carry the release error", err)
	}
}

// TestContextMeterReleasesOnPanic pins that a panic in work releases the
// hold while it unwinds and continues with its own value.
func TestContextMeterReleasesOnPanic(t *testing.T) {
	account := &recordingAccount{}
	meter := mustContextMeter(t, account, &contextSink{})
	func() {
		defer func() {
			if got := recover(); got != "boom" {
				t.Errorf("recovered %v, want boom", got)
			}
		}()
		_, _ = meter.Call(context.Background(), 30*Cent, "tts", "job-1",
			func(context.Context) (Usage, error) { panic("boom") })
	}()
	steps := account.calls()
	if len(steps) != 2 || steps[1].op != "release" || steps[1].hold != steps[0].hold {
		t.Fatalf("account calls = %+v, want reserve then the release of that hold", steps)
	}
}

// TestContextMeterRefusedReserveRunsNothing pins that a reserve the account
// refuses runs no work, releases nothing and reports the refusal.
func TestContextMeterRefusedReserveRunsNothing(t *testing.T) {
	account := &recordingAccount{reserveErr: ErrOverBudget}
	meter := mustContextMeter(t, account, &contextSink{})
	_, err := meter.Call(context.Background(), 30*Cent, "tts", "job-1",
		func(context.Context) (Usage, error) {
			t.Error("work ran after a refused reserve")
			return Usage{}, nil
		})
	if !errors.Is(err, ErrOverBudget) {
		t.Fatalf("Call error = %v, want ErrOverBudget", err)
	}
	if steps := account.calls(); len(steps) != 1 {
		t.Errorf("account calls = %+v, want the reserve alone", steps)
	}
}

// TestContextMeterOnceBooksARepeatOnce pins that a repeated reference under
// Once books and records once, and the repeat neither releases a second
// time nor reaches the sink.
func TestContextMeterOnceBooksARepeatOnce(t *testing.T) {
	account := &recordingAccount{}
	sink := &contextSink{}
	meter := mustContextMeter(t, account, sink)
	for i := range 2 {
		usage, err := meter.Call(context.Background(), 30*Cent, "tts", "job-1",
			func(context.Context) (Usage, error) { return Usage{Price: 10 * Cent, Measured: true}, nil }, Once())
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		want := Usage{Price: 10 * Cent, Measured: true}
		if i == 1 {
			want = Usage{}
		}
		if usage != want {
			t.Errorf("Call %d usage = %+v, want %+v", i, usage, want)
		}
	}
	for _, s := range account.calls() {
		if s.op == "release" {
			t.Errorf("account calls = %+v, want no release on a once path", account.calls())
			break
		}
	}
	if len(sink.charges) != 1 {
		t.Errorf("sink charges = %d, want 1", len(sink.charges))
	}
}

// TestNewContextMeterRefusesNilParts pins the same nil guard NewMeter has,
// for a plain nil and for an interface holding a nil pointer.
func TestNewContextMeterRefusesNilParts(t *testing.T) {
	if _, err := NewContextMeter(nil, NewLedger()); !errors.Is(err, ErrNilAccount) {
		t.Errorf("NewContextMeter(nil, sink) error = %v, want ErrNilAccount", err)
	}
	if _, err := NewContextMeter((*recordingAccount)(nil), NewLedger()); !errors.Is(err, ErrNilAccount) {
		t.Errorf("NewContextMeter(typed nil, sink) error = %v, want ErrNilAccount", err)
	}
	if _, err := NewContextMeter(&recordingAccount{}, nil); !errors.Is(err, ErrNilSink) {
		t.Errorf("NewContextMeter(account, nil) error = %v, want ErrNilSink", err)
	}
	if _, err := NewContextMeter(&recordingAccount{}, (*Ledger)(nil)); !errors.Is(err, ErrNilSink) {
		t.Errorf("NewContextMeter(account, typed nil) error = %v, want ErrNilSink", err)
	}
	if _, err := NewContextMeter((*Budget)(nil).Account(), NewLedger()); !errors.Is(err, ErrNilAccount) {
		t.Errorf("NewContextMeter(nil budget's account, sink) error = %v, want ErrNilAccount", err)
	}
}

// TestBudgetAccountDrivesTheBudget pins that the context shape of a Budget
// books, holds and frees against the budget itself, with an empty hold ID
// and no expiry.
func TestBudgetAccountDrivesTheBudget(t *testing.T) {
	budget, err := NewBudget(100 * Cent)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	account := budget.Account()
	ctx := context.Background()
	h, err := account.Reserve(ctx, 30*Cent)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if want := (Hold{Amount: 30 * Cent}); h != want {
		t.Errorf("hold = %+v, want %+v", h, want)
	}
	if got := budget.Reserved(); got != 30*Cent {
		t.Errorf("Reserved() = %d, want %d", got, 30*Cent)
	}
	if err := account.Settle(ctx, h, 20*Cent); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	h2, err := account.Reserve(ctx, 10*Cent)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := account.Release(ctx, h2); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := account.Reserve(ctx, 81*Cent); !errors.Is(err, ErrOverBudget) {
		t.Errorf("Reserve past the ceiling error = %v, want ErrOverBudget", err)
	}
	if budget.Spent() != 20*Cent || budget.Reserved() != 0 {
		t.Errorf("Spent, Reserved = %d, %d, want %d, 0", budget.Spent(), budget.Reserved(), 20*Cent)
	}
	if _, err := account.SettleOnce(ctx, Hold{}, 1, "tts", ""); !errors.Is(err, ErrEmptyReference) {
		t.Errorf("SettleOnce without a reference error = %v, want ErrEmptyReference", err)
	}
	if got := account.Denomination(); got != budget.Denomination() {
		t.Errorf("Denomination() = %+v, want %+v", got, budget.Denomination())
	}
}

// TestKeyedOwnerAccountBindsTheOwner pins that the context shape of one
// owner books against that owner and the global ceiling and nobody else,
// and that an unknown owner is refused before a call runs.
func TestKeyedOwnerAccountBindsTheOwner(t *testing.T) {
	k, err := NewKeyedBudget(100 * Cent)
	if err != nil {
		t.Fatalf("NewKeyedBudget: %v", err)
	}
	if err := k.SetLimit("a", 40*Cent); err != nil {
		t.Fatalf("SetLimit a: %v", err)
	}
	if err := k.SetLimit("b", 40*Cent); err != nil {
		t.Fatalf("SetLimit b: %v", err)
	}
	if _, err := k.OwnerAccount("ghost"); !errors.Is(err, ErrUnknownOwner) {
		t.Errorf("OwnerAccount(ghost) error = %v, want ErrUnknownOwner", err)
	}
	account, err := k.OwnerAccount("a")
	if err != nil {
		t.Fatalf("OwnerAccount(a): %v", err)
	}
	meter := mustContextMeter(t, account, NewLedger())
	if _, err := meter.Call(context.Background(), 30*Cent, "tts", "job-1",
		func(context.Context) (Usage, error) { return Usage{Price: 25 * Cent, Measured: true}, nil }); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, err := k.Remaining("a"); err != nil || got != 15*Cent {
		t.Errorf("Remaining(a) = %d, %v, want %d", got, err, 15*Cent)
	}
	if got, err := k.Remaining("b"); err != nil || got != 40*Cent {
		t.Errorf("Remaining(b) = %d, %v, want %d", got, err, 40*Cent)
	}
	if _, err := meter.Call(context.Background(), 20*Cent, "tts", "job-2",
		func(context.Context) (Usage, error) { return Usage{}, nil }); !errors.Is(err, ErrOverBudget) {
		t.Errorf("Call past a's ceiling error = %v, want ErrOverBudget", err)
	}
}

// TestContextMeterConcurrentCallsSettleExactly drives one durable-shaped
// meter from many goroutines under the race detector and pins the booked
// total, so the shared account sees every settle exactly once.
func TestContextMeterConcurrentCallsSettleExactly(t *testing.T) {
	budget, err := NewBudget(1000 * Cent)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	ledger := NewLedger()
	meter := mustContextMeter(t, budget.Account(), ledger)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := meter.Call(context.Background(), 5*Cent, "tts", "job",
				func(context.Context) (Usage, error) { return Usage{Price: 3 * Cent, Measured: true}, nil }); err != nil {
				t.Errorf("Call: %v", err)
			}
		})
	}
	wg.Wait()
	if budget.Spent() != 96*Cent || budget.Reserved() != 0 {
		t.Errorf("Spent, Reserved = %d, %d, want %d, 0", budget.Spent(), budget.Reserved(), 96*Cent)
	}
	if got, err := ledger.Total(); err != nil || got != 96*Cent {
		t.Errorf("ledger total = %d, %v, want %d", got, err, 96*Cent)
	}
}

// TestMeterRecordsAfterALateCancel pins that the in-memory meter shares the
// detached settle: a cancel that lands after the work returned still
// records the charge in a sink that honours its context.
func TestMeterRecordsAfterALateCancel(t *testing.T) {
	budget, err := NewBudget(100 * Cent)
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	sink := &contextSink{}
	meter, err := NewMeter(budget, sink)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := meter.Call(ctx, 30*Cent, "tts", "job-1",
		func(context.Context) (Usage, error) {
			cancel()
			return Usage{Price: 10 * Cent, Measured: true}, nil
		}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(sink.done) != 1 || sink.done[0] {
		t.Errorf("sink saw done contexts %v, want one live context", sink.done)
	}
	if budget.Spent() != 10*Cent {
		t.Errorf("Spent() = %d, want %d", budget.Spent(), 10*Cent)
	}
}
