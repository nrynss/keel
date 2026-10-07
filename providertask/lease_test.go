package providertask_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/providertask"
)

// leaseClock is a time source the test moves by hand, so lease expiry and
// takeover need no wall clock wait. The two processes share one clock,
// which is what the package promise asks of processes that share a store.
type leaseClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *leaseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *leaseClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newLeaseClock returns a clock at the harness base with a config whose
// waits are fast and whose clock is the shared one. The lease length stays
// at its thirty second floor, and the tests move the clock past it instead
// of waiting.
func newLeaseClock(h *harness) (*leaseClock, providertask.Config) {
	clk := &leaseClock{at: h.base}
	cfg := h.config()
	cfg.Now = clk.now
	return clk, cfg
}

// countingGetStore counts Get calls, so a test can see that a waiter sits
// in its wait loop before the clock moves.
type countingGetStore struct {
	providertask.Store
	gets atomic.Int64
}

func (s *countingGetStore) Get(ctx context.Context, key string) (providertask.Claim, error) {
	s.gets.Add(1)
	return s.Store.Get(ctx, key)
}

// finishCaptureStore records every terminal record fault, so a test can
// pin a refused Finish the Run itself only logs.
type finishCaptureStore struct {
	providertask.Store
	mu   sync.Mutex
	errs []error
}

func (s *finishCaptureStore) Finish(ctx context.Context, key string, token int64, state providertask.State, code string) error {
	err := s.Store.Finish(ctx, key, token, state, code)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
	return err
}

// errs returns the recorded Finish faults.
func (s *finishCaptureStore) finishErrs() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errs...)
}

// gatedAccount parks the first SettleOnce inside the gate, so a test can
// hold a driver between the settle and the booking while the world moves
// around it. Later settles pass straight through.
type gatedAccount struct {
	cost.Account
	entered chan struct{}
	gate    chan struct{}
	armed   atomic.Bool
}

func newGatedAccount(a cost.Account) *gatedAccount {
	g := &gatedAccount{Account: a, entered: make(chan struct{}), gate: make(chan struct{})}
	g.armed.Store(true)
	return g
}

// plantLeader writes the row a leader that has just died leaves behind: a
// recorded task under a foreign lease that nothing will renew. The owner is
// a name no process here holds, so a same process takeover cannot win
// before the lease expires.
func plantLeader(t *testing.T, store providertask.Store, key string, now time.Time) {
	t.Helper()
	if _, created, err := store.Claim(t.Context(), key, "dead-leader", now, 30*time.Second); err != nil || !created {
		t.Fatalf("plant Claim = %v, %v, want created", created, err)
	}
	if err := store.Record(t.Context(), key, "task-"+key); err != nil {
		t.Fatalf("plant Record: %v", err)
	}
}

func (a *gatedAccount) SettleOnce(reserved, actual cost.Price, kind, ref string) (bool, error) {
	if a.armed.CompareAndSwap(true, false) {
		close(a.entered)
		<-a.gate
	}
	return a.Account.SettleOnce(reserved, actual, kind, ref)
}

// waitTicks polls until the probe returns true, and fails the test at the
// deadline.
func waitTicks(t *testing.T, probe func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !probe() {
		if time.Now().After(deadline) {
			t.Fatal("the probe never turned true")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHealthyLeaderDrivesAndTheSecondProcessCollects: the leader creates
// once, polls to its verdict and settles once. The second process takes
// the released lease, collects the recorded outcome, and never calls
// Status or the meter.
func TestHealthyLeaderDrivesAndTheSecondProcessCollects(t *testing.T) {
	h := newHarness(t)
	_, cfg := newLeaseClock(h)
	_, storeB := openAt(t, h.path)
	coordinatorA := providertask.NewCoordinator()
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)

	p := newFakeProvider(rendered{Link: "healthy"},
		step{st: providertask.Status{State: providertask.StateRunning}},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfgA := cfg
	cfgA.Meter = meter
	cfgA.Estimate = 10
	cfgA.Coordinator = coordinatorA
	outcome, err := providertask.Run(t.Context(), cfgA, h.store, "healthy", p.spec(p.Keep))
	if err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	if outcome.Value.Link != "healthy" || outcome.Usage.Price != 10 {
		t.Fatalf("leader outcome = %+v, want the result and the settled estimate", outcome)
	}

	// The leader's Finish cleared the lease, so the second process takes
	// over without the clock moving at all.
	cfgB := cfg
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	outcomeB, err := providertask.Run(t.Context(), cfgB, storeB, "healthy", p.spec(p.Keep))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if outcomeB.Value.Link != "healthy" {
		t.Errorf("second outcome = %+v, want the fetched result", outcomeB)
	}
	if (outcomeB.Usage != cost.Usage{}) {
		t.Errorf("second usage = %+v, want the zero usage, the leader settled", outcomeB.Usage)
	}
	creates, statuses, results, keeps := p.counts()
	if creates != 1 {
		t.Errorf("creates = %d, want 1", creates)
	}
	if statuses != 2 {
		t.Errorf("statuses = %d, want the leader's two polls only", statuses)
	}
	if results != 2 || keeps != 2 {
		t.Errorf("results=%d keeps=%d, want one fetch and one keep per Run", results, keeps)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() = %d, want the single leader booking", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1", len(charges))
	}
	// The takeover bumped the token once, and the collecting process
	// leases the row it drives, verdict already recorded.
	claim, err := storeB.Get(t.Context(), "healthy")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.State != providertask.StateSucceeded || claim.Token != 2 {
		t.Errorf("claim = %+v, want the verdict recorded under token 2", claim)
	}
}

// TestLeaderThatStopsRenewingIsTakenOver: a leader has recorded its task
// under a lease nothing renews, which is what a frozen or dead process
// leaves behind. Once the lease length passes, the second process takes
// over in its own resume path, settles once, and records the verdict.
func TestLeaderThatStopsRenewingIsTakenOver(t *testing.T) {
	h := newHarness(t)
	clk, cfg := newLeaseClock(h)
	_, storeB := openAt(t, h.path)
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)

	plantLeader(t, storeB, "stalled", clk.now())
	claim, err := storeB.Get(t.Context(), "stalled")
	if err != nil {
		t.Fatalf("Get while the leader holds: %v", err)
	}
	if claim.TaskID == "" || claim.Owner != "dead-leader" {
		t.Fatalf("claim = %+v, want a recorded task under the dead leader's lease", claim)
	}

	// The lease length passes with no renewal behind it, and the second
	// process drives the recorded task.
	clk.advance(31 * time.Second)
	p := newFakeProvider(rendered{Link: "taken"},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfgB := cfg
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	outcome, err := providertask.Run(t.Context(), cfgB, storeB, "stalled", p.spec(nil))
	if err != nil {
		t.Fatalf("takeover Run: %v", err)
	}
	if outcome.Value.Link != "taken" || outcome.Usage.Price != 10 {
		t.Fatalf("takeover outcome = %+v, want the result and the settled estimate", outcome)
	}
	creates, statuses, _, _ := p.counts()
	if creates != 0 {
		t.Errorf("creates = %d, want none, the recorded task is resumed", creates)
	}
	if statuses != 1 {
		t.Errorf("statuses = %d, want the takeover's single poll", statuses)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() = %d, want the one takeover booking", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1", len(charges))
	}
	claim, err = storeB.Get(t.Context(), "stalled")
	if err != nil {
		t.Fatalf("Get after the takeover: %v", err)
	}
	if claim.State != providertask.StateSucceeded {
		t.Errorf("state = %s, want the takeover's verdict recorded", claim.State)
	}
}

// TestDeadlineWithoutVerdictReleasesTheLease: the leader runs out its
// deadline on a task that never finishes and releases the lease on the way
// out. The second process takes over at once without the clock moving.
func TestDeadlineWithoutVerdictReleasesTheLease(t *testing.T) {
	h := newHarness(t)
	_, cfg := newLeaseClock(h)
	_, storeB := openAt(t, h.path)
	coordinatorA := providertask.NewCoordinator()
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)

	pA := newFakeProvider(rendered{Link: "never"},
		step{st: providertask.Status{State: providertask.StateRunning}},
	)
	cfgA := cfg
	cfgA.Meter = meter
	cfgA.Estimate = 10
	cfgA.Coordinator = coordinatorA
	cfgA.Deadline = 60 * time.Millisecond
	_, errA := providertask.Run(t.Context(), cfgA, h.store, "release", pA.spec(nil))
	if !errors.Is(errA, providertask.ErrDeadline) {
		t.Fatalf("leader error = %v, want ErrDeadline", errA)
	}
	claim, err := storeB.Get(t.Context(), "release")
	if err != nil {
		t.Fatalf("Get after the deadline: %v", err)
	}
	if claim.Owner != "" || !claim.LeaseUntil.IsZero() {
		t.Fatalf("claim = %+v, want the lease released", claim)
	}
	if claim.State != providertask.StateRunning || claim.TaskID == "" {
		t.Fatalf("claim = %+v, want the recorded task still running", claim)
	}

	// The second process takes the released lease at once, though the
	// original lease would run thirty seconds more, and settles the task
	// the leader left.
	pB := newFakeProvider(rendered{Link: "resumed"},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfgB := cfg
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	outcome, err := providertask.Run(t.Context(), cfgB, storeB, "release", pB.spec(nil))
	if err != nil {
		t.Fatalf("takeover Run: %v", err)
	}
	if outcome.Value.Link != "resumed" || outcome.Usage.Price != 10 {
		t.Fatalf("takeover outcome = %+v, want the result and the settled estimate", outcome)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() = %d, want the single takeover booking", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1", len(charges))
	}
}

// TestLeaseExpiryDuringAwaitHandsTheDriveToTheWaiter: the second process
// sits in the wait loop while the dead leader's lease is still live. When
// the lease expires it takes the drive inside that same Run, settles, and
// returns the outcome. No fresh Run starts.
func TestLeaseExpiryDuringAwaitHandsTheDriveToTheWaiter(t *testing.T) {
	h := newHarness(t)
	clk, cfg := newLeaseClock(h)
	_, storeB := openAt(t, h.path)
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)

	plantLeader(t, storeB, "waited", clk.now())

	// The waiter counts its reads, so the test knows it sits in the wait
	// loop while the lease is still live.
	getsB := &countingGetStore{Store: storeB}
	p := newFakeProvider(rendered{Link: "waited"},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfgB := cfg
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	doneB := make(chan struct{})
	var outcomeB providertask.Outcome[rendered]
	var errB error
	go func() {
		defer close(doneB)
		outcomeB, errB = providertask.Run(t.Context(), cfgB, getsB, "waited", p.spec(nil))
	}()
	waitTicks(t, func() bool { return getsB.gets.Load() >= 2 })
	live, err := storeB.Get(t.Context(), "waited")
	if err != nil {
		t.Fatalf("Get during the wait: %v", err)
	}
	if live.Owner != "dead-leader" || live.Token != 1 {
		t.Fatalf("claim during the wait = %+v, want the dead leader still holding token 1", live)
	}

	// The lease expires mid wait, and the waiter drives the task inside
	// its own Run.
	clk.advance(31 * time.Second)
	<-doneB
	if errB != nil {
		t.Fatalf("waiter Run: %v", errB)
	}
	if outcomeB.Value.Link != "waited" || outcomeB.Usage.Price != 10 {
		t.Fatalf("waiter outcome = %+v, want the result and the settled estimate", outcomeB)
	}
	creates, statuses, _, _ := p.counts()
	if creates != 0 {
		t.Errorf("creates = %d, want none, the waiter never creates", creates)
	}
	if statuses != 1 {
		t.Errorf("statuses = %d, want the waiter's single poll of the recorded task", statuses)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() = %d, want the waiter's single booking", got)
	}
	claim, err := storeB.Get(t.Context(), "waited")
	if err != nil {
		t.Fatalf("Get after the takeover: %v", err)
	}
	if claim.State != providertask.StateSucceeded || claim.Token != 2 {
		t.Errorf("claim = %+v, want the verdict recorded under token 2", claim)
	}
}

// TestStalledLeaderWithStaleTokenBooksNothing: a leader parks between the
// token check and the booking, its lease passes, and the second process
// takes over, settles and records. The parked leader then wakes, its
// SettleOnce books nothing and frees its reservation, and its stale Finish
// refuses.
func TestStalledLeaderWithStaleTokenBooksNothing(t *testing.T) {
	h := newHarness(t)
	clk, cfg := newLeaseClock(h)
	_, storeB := openAt(t, h.path)
	coordinatorA := providertask.NewCoordinator()
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	gated := newGatedAccount(budget)
	meter, err := cost.NewMeter(gated, ledger)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}

	p := newFakeProvider(rendered{Link: "fenced"},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	finishA := &finishCaptureStore{Store: h.store}
	cfgA := cfg
	cfgA.Meter = meter
	cfgA.Estimate = 10
	cfgA.Coordinator = coordinatorA
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		_, _ = providertask.Run(t.Context(), cfgA, finishA, "fenced", p.spec(nil))
	}()
	// The leader parks inside its settle with its token still fresh.
	<-gated.entered

	// The lease passes, and the second process takes over, books the only
	// charge, and records the verdict under its own token.
	clk.advance(31 * time.Second)
	cfgB := cfg
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	outcomeB, err := providertask.Run(t.Context(), cfgB, storeB, "fenced", p.spec(nil))
	if err != nil {
		t.Fatalf("takeover Run: %v", err)
	}
	if outcomeB.Value.Link != "fenced" || outcomeB.Usage.Price != 10 {
		t.Fatalf("takeover outcome = %+v, want the result and the settled estimate", outcomeB)
	}
	if got := budget.Spent(); got != 10 {
		t.Fatalf("Spent() before the wake = %d, want the takeover booking only", got)
	}

	// The leader wakes. Its settle books nothing and frees its hold, and
	// its stale Finish refuses.
	close(gated.gate)
	<-doneA
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() after the wake = %d, want the single booking", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0, the stale settle freed its hold", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1, the sink never sees the stale settle", len(charges))
	}
	stale := false
	for _, ferr := range finishA.finishErrs() {
		if errors.Is(ferr, providertask.ErrStaleToken) {
			stale = true
		}
	}
	if !stale {
		t.Errorf("leader Finish faults = %v, want one matching ErrStaleToken", finishA.finishErrs())
	}
	claim, err := storeB.Get(t.Context(), "fenced")
	if err != nil {
		t.Fatalf("Get after the wake: %v", err)
	}
	if claim.State != providertask.StateSucceeded {
		t.Errorf("state = %s, want the takeover's verdict intact", claim.State)
	}
	creates, _, _, _ := p.counts()
	if creates != 1 {
		t.Errorf("creates = %d, want the leader's single create", creates)
	}
}

// renewRefuseStore refuses every Renew outright, so the driver that holds
// it loses its lease at the first renewal tick no matter what the clock
// reads.
type renewRefuseStore struct {
	providertask.Store
}

func (s *renewRefuseStore) Renew(ctx context.Context, key, owner string, token int64, until time.Time) (bool, error) {
	return false, nil
}

// TestRenewalFailureFallsBackToAwait: a driver whose renewal is refused
// stops polling and waits like a joiner, and the Run that takes the key
// settles it. The stalled driver collects the recorded outcome with no
// usage of its own.
func TestRenewalFailureFallsBackToAwait(t *testing.T) {
	h := newHarness(t)
	_, cfg := newLeaseClock(h)
	_, storeB := openAt(t, h.path)
	coordinatorA := providertask.NewCoordinator()
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)

	p := newFakeProvider(rendered{Link: "renewed"})
	aGate := make(chan struct{})
	var closeGate sync.Once
	specA := p.spec(nil)
	var aParks atomic.Bool
	specA.Status = func(ctx context.Context, taskID string) (providertask.Status, error) {
		aParks.Store(true)
		// The poll observes its context, the way a real provider client
		// does, so a renewal that cancels the drive ends the call.
		select {
		case <-aGate:
			return providertask.Status{}, errors.New("providertask test: leader stopped")
		case <-ctx.Done():
			return providertask.Status{}, ctx.Err()
		}
	}
	t.Cleanup(func() { closeGate.Do(func() { close(aGate) }) })

	getsA := &countingGetStore{Store: h.store}
	storeA := &renewRefuseStore{Store: getsA}
	cfgA := cfg
	cfgA.Meter = meter
	cfgA.Estimate = 10
	cfgA.Coordinator = coordinatorA
	outcomeA := make(chan providertask.Outcome[rendered], 1)
	errA := make(chan error, 1)
	go func() {
		o, e := providertask.Run(t.Context(), cfgA, storeA, "renewed", specA)
		outcomeA <- o
		errA <- e
	}()
	waitTicks(t, func() bool { return aParks.Load() })

	// The first renewal tick refuses, so the driver stops polling and
	// falls back to the wait loop, which reads the row again.
	waitTicks(t, func() bool { return getsA.gets.Load() >= 2 })

	pB := newFakeProvider(rendered{Link: "settled"},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfgB := cfg
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	outcomeB, err := providertask.Run(t.Context(), cfgB, storeB, "renewed", pB.spec(nil))
	if err != nil {
		t.Fatalf("takeover Run: %v", err)
	}
	if outcomeB.Value.Link != "settled" || outcomeB.Usage.Price != 10 {
		t.Fatalf("takeover outcome = %+v, want the result and the settled estimate", outcomeB)
	}

	// The leader's parked poll ends, the lost drive falls back to the
	// wait, and the recorded outcome resolves it with nothing settled.
	closeGate.Do(func() { close(aGate) })
	if err := <-errA; err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	if got := <-outcomeA; (got.Usage != cost.Usage{}) {
		t.Errorf("leader usage = %+v, want the zero usage, the takeover settled", got.Usage)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() = %d, want the single takeover booking", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Errorf("charges = %d rows, want 1", len(charges))
	}
}

// foreignRowStore rewrites every read row, so the driver's token check
// sees a row that moved on underneath it. That is how a row looks after
// another process took the lease between the claim and the check.
type foreignRowStore struct {
	providertask.Store
}

func (s *foreignRowStore) Get(ctx context.Context, key string) (providertask.Claim, error) {
	claim, err := s.Store.Get(ctx, key)
	if err != nil {
		return claim, err
	}
	claim.Owner = "someone-else"
	claim.Token = claim.Token + 7
	return claim, nil
}

// TestStaleTokenCheckSkipsTheSettle pins the fencing check the driver runs
// immediately before it settles. A row that no longer names this driver
// and token stops the settle, so nothing books and the Run falls back to
// awaiting the outcome the real owner will produce.
func TestStaleTokenCheckSkipsTheSettle(t *testing.T) {
	h := newHarness(t)
	_, cfg := newLeaseClock(h)
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)

	rogue := &foreignRowStore{Store: h.store}
	cfgA := cfg
	cfgA.Meter = meter
	cfgA.Estimate = 10
	cfgA.Coordinator = providertask.NewCoordinator()
	// The fenced drive falls back to the wait loop, and a short deadline
	// ends that wait with the pending refusal.
	cfgA.Deadline = 50 * time.Millisecond
	p := newFakeProvider(rendered{Link: "done"},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	_, err := providertask.Run(t.Context(), cfgA, rogue, "fenced-check", p.spec(nil))
	if !errors.Is(err, providertask.ErrTaskPending) {
		t.Fatalf("Run error = %v, want ErrTaskPending, the fenced drive waits", err)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("Spent() = %d, want 0, the check stops the settle", got)
	}
	if charges := ledger.Charges(); len(charges) != 0 {
		t.Errorf("charges = %d rows, want 0, the fenced settle never reaches the sink", len(charges))
	}
}

// TestDeadlineOutsideThePollReleasesTheLease: the deadline passes while
// the Run fetches the result or prices it, after the task id is recorded.
// The fault surfaces as the context's own error, the lease is released on
// the way out, and the next Run takes over at once, the way the poll
// loop's deadline already hands the drive over.
func TestDeadlineOutsideThePollReleasesTheLease(t *testing.T) {
	for _, phase := range []string{"result", "price"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t)
			_, storeB := openAt(t, h.path)
			budget, ledger := meterOf(t, 1000)
			meter := mustMeter(t, budget, ledger)

			p := newFakeProvider(rendered{Link: "slow"},
				step{st: providertask.Status{State: providertask.StateSucceeded}},
			)
			spec := p.spec(nil)
			block := func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}
			if phase == "result" {
				spec.Result = func(ctx context.Context, taskID string) (rendered, error) {
					return rendered{}, block(ctx)
				}
			} else {
				spec.Price = func(ctx context.Context, value rendered) (cost.Price, error) {
					return 0, block(ctx)
				}
			}
			cfgA := h.config()
			cfgA.Meter = meter
			cfgA.Estimate = 10
			cfgA.Coordinator = providertask.NewCoordinator()
			cfgA.Deadline = 60 * time.Millisecond
			key := "outside-" + phase
			_, errA := providertask.Run(t.Context(), cfgA, h.store, key, spec)
			if !errors.Is(errA, context.DeadlineExceeded) {
				t.Fatalf("leader error = %v, want the context deadline", errA)
			}
			claim, err := storeB.Get(t.Context(), key)
			if err != nil {
				t.Fatalf("Get after the deadline: %v", err)
			}
			if claim.Owner != "" || !claim.LeaseUntil.IsZero() {
				t.Fatalf("claim = %+v, want the lease released", claim)
			}
			if claim.State != providertask.StateRunning || claim.TaskID == "" {
				t.Fatalf("claim = %+v, want the recorded task still running", claim)
			}

			// The second process takes the released lease at once, though
			// the original lease would run thirty seconds more, and settles
			// the task the leader left.
			q := newFakeProvider(rendered{Link: "resumed"},
				step{st: providertask.Status{State: providertask.StateSucceeded}},
			)
			cfgB := h.config()
			cfgB.Meter = meter
			cfgB.Estimate = 10
			cfgB.Coordinator = providertask.NewCoordinator()
			outcome, err := providertask.Run(t.Context(), cfgB, storeB, key, q.spec(nil))
			if err != nil {
				t.Fatalf("takeover Run: %v", err)
			}
			if outcome.Value.Link != "resumed" || outcome.Usage.Price != 10 {
				t.Fatalf("takeover outcome = %+v, want the result and the settled estimate", outcome)
			}
			creates, statuses, _, _ := q.counts()
			if creates != 0 {
				t.Errorf("creates = %d, want none, the recorded task is resumed", creates)
			}
			if statuses != 1 {
				t.Errorf("statuses = %d, want the takeover's single poll", statuses)
			}
			if got := budget.Spent(); got != 10 {
				t.Errorf("Spent() = %d, want the single takeover booking", got)
			}
			if charges := ledger.Charges(); len(charges) != 1 {
				t.Errorf("charges = %d rows, want 1", len(charges))
			}
		})
	}
}
