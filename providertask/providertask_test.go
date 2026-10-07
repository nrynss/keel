package providertask_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/providertask/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// errTransient is a Status fault a wait can clear, and errPermanent is one
// it cannot.
var (
	errTransient = errors.New("providertask test: transient fault")
	errPermanent = errors.New("providertask test: permanent fault")
)

// classify is the Retryable the retry tests share.
func classify(err error) bool {
	return errors.Is(err, errTransient)
}

// rendered is the result type every fake task reports.
type rendered struct {
	Link string
}

// step is one scripted answer the fake provider gives to Status.
type step struct {
	st  providertask.Status
	err error
}

// fakeProvider is a provider whose create, status, result and keep calls
// are counted, and whose status answers follow a script. A script that
// runs out of steps repeats its last one, so a single running step polls
// forever.
type fakeProvider struct {
	mu        sync.Mutex
	creates   int
	statuses  int
	results   int
	keeps     int
	createErr error
	keepErr   error
	value     rendered
	steps     []step
	stepNo    int
	price     func() (cost.Price, error)
}

// newFakeProvider returns a provider that reports value and follows steps.
func newFakeProvider(value rendered, steps ...step) *fakeProvider {
	return &fakeProvider{value: value, steps: steps}
}

// counts returns the call counters observed so far.
func (f *fakeProvider) counts() (creates, statuses, results, keeps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.statuses, f.results, f.keeps
}

// Create counts the call and returns the scripted fault or a fresh id.
func (f *fakeProvider) Create(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createErr != nil {
		return "", f.createErr
	}
	return "task-" + time.Now().Format("150405.000000000"), nil
}

// Status counts the call and plays the next scripted step.
func (f *fakeProvider) Status(ctx context.Context, taskID string) (providertask.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses++
	if f.stepNo < len(f.steps) {
		s := f.steps[f.stepNo]
		f.stepNo++
		return s.st, s.err
	}
	if len(f.steps) == 0 {
		return providertask.Status{}, errPermanent
	}
	return f.steps[len(f.steps)-1].st, f.steps[len(f.steps)-1].err
}

// Result counts the call and reports the canned value.
func (f *fakeProvider) Result(ctx context.Context, taskID string) (rendered, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results++
	return f.value, nil
}

// Keep counts the call and reports the scripted fault.
func (f *fakeProvider) Keep(ctx context.Context, value rendered) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keeps++
	return f.keepErr
}

// Price measures the canned price through the scripted hook.
func (f *fakeProvider) Price(ctx context.Context, value rendered) (cost.Price, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.price == nil {
		return 0, nil
	}
	return f.price()
}

// spec builds the Spec a Run drives, with the keep hook wired in. The
// price hook joins only when the fake has one, so a nil price means the
// Run settles unmeasured.
func (f *fakeProvider) spec(keep func(ctx context.Context, v rendered) error) providertask.Spec[rendered] {
	sp := providertask.Spec[rendered]{
		Create: f.Create,
		Status: f.Status,
		Result: f.Result,
		Keep:   keep,
	}
	if f.price != nil {
		sp.Price = f.Price
	}
	return sp
}

// progressOf builds one provider progress report with counters attached.
func progressOf(stage string, current, total int64) job.Progress {
	return job.Progress{Stage: stage, Current: &current, Total: &total}
}

// harness is one test's database, store and config. The clock is frozen at
// base so the window arithmetic reads the same in every test.
type harness struct {
	t     *testing.T
	base  time.Time
	path  string
	db    *sqlite.DB
	store *sqlitestore.Store
	log   *slog.Logger
}

// openAt opens the database and store at path, the way a restarted process
// would.
func openAt(t *testing.T, path string) (*sqlite.DB, *sqlitestore.Store) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, Logger: log})
	if err != nil {
		t.Fatalf("open sqlite at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("open store at %s: %v", path, err)
	}
	return db, store
}

// newHarness opens a fresh database and store for one test.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		base: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		path: dbPath(t),
		log:  slog.New(slog.DiscardHandler),
	}
	h.db, h.store = openAt(t, h.path)
	return h
}

// dbPath returns the database path inside the test's temp directory.
func dbPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/tasks.db"
}

// config returns a fast Config whose clock reads the harness base. Every
// test overrides the waits it cares about.
func (h *harness) config() providertask.Config {
	return providertask.Config{
		PollBase:    time.Millisecond,
		PollCeiling: 2 * time.Millisecond,
		Log:         h.log,
		Now:         func() time.Time { return h.base },
	}
}

// scalar runs one single value query and returns it as an integer. The
// read runs through the database's own reader pool, and the store tests
// pin the same rows again through a connection this module never holds.
func (h *harness) scalar(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.Reader().QueryRowContext(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// meterOf returns a budget of limit and the ledger its charges land in.
func meterOf(t *testing.T, limit cost.Price) (*cost.Budget, *cost.Ledger) {
	t.Helper()
	budget, err := cost.NewBudget(limit)
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	return budget, cost.NewLedger()
}

// mustMeter returns a meter over the budget and ledger.
func mustMeter(t *testing.T, budget *cost.Budget, ledger *cost.Ledger) *cost.Meter {
	t.Helper()
	meter, err := cost.NewMeter(budget, ledger)
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	return meter
}

// TestRunCreatesRecordsPollsAndKeeps: a plain Run creates once, publishes
// the created stage and the provider progress, fetches the result, keeps
// it, and lands a succeeded row.
func TestRunCreatesRecordsPollsAndKeeps(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var stages []string
	p := newFakeProvider(rendered{Link: "s3://result"},
		step{st: providertask.Status{
			State:    providertask.StateRunning,
			Progress: progressOf("render", 3, 10),
		}},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfg := h.config()
	cfg.Progress = func(p job.Progress) {
		mu.Lock()
		defer mu.Unlock()
		stages = append(stages, p.Stage)
	}
	keeps := 0
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "key-one", p.spec(func(ctx context.Context, v rendered) error {
		mu.Lock()
		keeps++
		mu.Unlock()
		if v.Link != "s3://result" {
			t.Errorf("keep value = %+v, want the fetched result", v)
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.TaskID == "" {
		t.Fatal("outcome carries no task id")
	}
	if outcome.Value.Link != "s3://result" {
		t.Errorf("value = %+v, want the fetched result", outcome.Value)
	}
	if outcome.Usage != (cost.Usage{}) {
		t.Errorf("usage = %+v, want zero for an unmetered run", outcome.Usage)
	}
	creates, statuses, results, _ := p.counts()
	if creates != 1 || results != 1 {
		t.Errorf("creates=%d results=%d, want one of each", creates, results)
	}
	mu.Lock()
	if keeps != 1 {
		t.Errorf("keeps = %d, want one", keeps)
	}
	if statuses != 2 {
		t.Errorf("statuses = %d, want one running poll and one verdict", statuses)
	}
	defer mu.Unlock()
	if len(stages) != 2 || stages[0] != providertask.StageCreated || stages[1] != "render" {
		t.Errorf("stages = %v, want created then render", stages)
	}
}

// TestRunLandsTheVerdictInTheStore pins the row a finished Run leaves, so
// a restart can read the outcome back.
func TestRunLandsTheVerdictInTheStore(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{Link: "done"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	if _, err := providertask.Run(t.Context(), h.config(), h.store, "key-row", p.spec(nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	claim, err := h.store.Get(t.Context(), "key-row")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.State != providertask.StateSucceeded || claim.TaskID == "" {
		t.Errorf("claim = %+v, want a recorded succeeded task", claim)
	}
}

// TestWindowRefusalNeverRecreates: a recorded task past the query window
// refuses with the stable sentinel, and nothing creates or polls it.
func TestWindowRefusalNeverRecreates(t *testing.T) {
	h := newHarness(t)
	old := h.base.Add(-25 * time.Hour)
	if _, _, err := h.store.Claim(t.Context(), "stale", "test", old, time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.store.Record(t.Context(), "stale", "task-old"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p := newFakeProvider(rendered{}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	cfg := providertask.Config{
		Window: 24 * time.Hour,
		Log:    h.log,
		Now:    func() time.Time { return h.base },
	}
	_, err := providertask.Run(t.Context(), cfg, h.store, "stale", p.spec(nil))
	if !errors.Is(err, providertask.ErrWindowExceeded) {
		t.Fatalf("err = %v, want ErrWindowExceeded", err)
	}
	creates, statuses, _, _ := p.counts()
	if creates != 0 || statuses != 0 {
		t.Errorf("creates=%d statuses=%d, want none: the refusal must not touch the provider", creates, statuses)
	}
	claim, err := h.store.Get(t.Context(), "stale")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.TaskID != "task-old" || claim.State != providertask.StateRunning {
		t.Errorf("claim = %+v, want the old task untouched", claim)
	}
}

// TestRecordedSuccessIsCollectedWithoutPollingOrSettling: a row that
// already records success is fetched and kept again, with no poll and no
// second charge.
func TestRecordedSuccessIsCollectedWithoutPollingOrSettling(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.store.Claim(t.Context(), "done", "test", h.base.Add(-time.Hour), time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.store.Record(t.Context(), "done", "task-done"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := h.store.Finish(t.Context(), "done", 1, providertask.StateSucceeded, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 10
	p := newFakeProvider(rendered{Link: "again"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "done", p.spec(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Value.Link != "again" || outcome.TaskID != "task-done" {
		t.Errorf("outcome = %+v, want the recorded task fetched again", outcome)
	}
	creates, statuses, results, _ := p.counts()
	if creates != 0 || statuses != 0 {
		t.Errorf("creates=%d statuses=%d, want none: the verdict is already recorded", creates, statuses)
	}
	if results != 1 {
		t.Errorf("results = %d, want one fetch of the expiring link", results)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("spent = %d, want nothing: the Run that recorded the verdict settled", got)
	}
	if outcome.Usage != (cost.Usage{}) {
		t.Errorf("usage = %+v, want zero on a collecting run", outcome.Usage)
	}
}

// TestRecordedFailureReturnsTheVerdict: a recorded failed verdict comes
// back as a TaskFailure with the stored provider code, without touching
// the provider.
func TestRecordedFailureReturnsTheVerdict(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.store.Claim(t.Context(), "lost", "test", h.base.Add(-time.Hour), time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.store.Record(t.Context(), "lost", "task-lost"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := h.store.Finish(t.Context(), "lost", 1, providertask.StateFailed, "content_broken"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	p := newFakeProvider(rendered{}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	_, err := providertask.Run(t.Context(), h.config(), h.store, "lost", p.spec(nil))
	var fail *providertask.TaskFailure
	if !errors.As(err, &fail) {
		t.Fatalf("err = %v, want a TaskFailure", err)
	}
	if fail.Code != "content_broken" || fail.TaskID != "task-lost" {
		t.Errorf("failure = %+v, want the recorded verdict", fail)
	}
	creates, statuses, _, _ := p.counts()
	if creates != 0 || statuses != 0 {
		t.Errorf("creates=%d statuses=%d, want none", creates, statuses)
	}
}

// TestDeadlineRefusalKeepsTheTaskResumable: a Run that runs out of time
// refuses with ErrDeadline, leaves the recorded task running, and a later
// Run resumes it without creating again.
func TestDeadlineRefusalKeepsTheTaskResumable(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{Link: "late"}, step{st: providertask.Status{State: providertask.StateRunning}})
	cfg := h.config()
	cfg.Deadline = 120 * time.Millisecond
	cfg.PollBase = 20 * time.Millisecond
	cfg.PollCeiling = 40 * time.Millisecond
	_, err := providertask.Run(t.Context(), cfg, h.store, "slow", p.spec(nil))
	if !errors.Is(err, providertask.ErrDeadline) {
		t.Fatalf("err = %v, want ErrDeadline", err)
	}
	claim, err := h.store.Get(t.Context(), "slow")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.TaskID == "" || claim.State != providertask.StateRunning {
		t.Errorf("claim = %+v, want the recorded task left running", claim)
	}

	// A later Run resumes the recorded task and never creates again.
	resume := newFakeProvider(rendered{Link: "late"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	outcome, err := providertask.Run(t.Context(), h.config(), h.store, "slow", resume.spec(nil))
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if outcome.TaskID != claim.TaskID || outcome.Value.Link != "late" {
		t.Errorf("outcome = %+v, want the same task resumed", outcome)
	}
	creates, _, _, _ := resume.counts()
	if creates != 0 {
		t.Errorf("resume created %d tasks, want none", creates)
	}
}

// TestTransientStatusErrorsAreRetriedThroughTheClassifier: a fault the
// classifier calls transient is waited out, and the Run finishes.
func TestTransientStatusErrorsAreRetriedThroughTheClassifier(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{Link: "ok"},
		step{err: errTransient},
		step{err: errTransient},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfg := h.config()
	cfg.Retryable = classify
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "flaky", p.spec(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Value.Link != "ok" {
		t.Errorf("value = %+v, want the fetched result", outcome.Value)
	}
	_, statuses, _, _ := p.counts()
	if statuses != 3 {
		t.Errorf("statuses = %d, want two retried faults and one verdict", statuses)
	}
}

// TestPermanentStatusErrorStopsTheRun: a fault the classifier rejects
// comes back at once and is not retried.
func TestPermanentStatusErrorStopsTheRun(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{}, step{err: errPermanent})
	cfg := h.config()
	cfg.Retryable = classify
	_, err := providertask.Run(t.Context(), cfg, h.store, "broken", p.spec(nil))
	if !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v, want the provider fault", err)
	}
	_, statuses, _, _ := p.counts()
	if statuses != 1 {
		t.Errorf("statuses = %d, want one call with no retry", statuses)
	}
}

// TestNilClassifierDoesNotRetry: a forgotten classifier never spins on a
// fault, so the first error ends the Run.
func TestNilClassifierDoesNotRetry(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{}, step{err: errTransient})
	_, err := providertask.Run(t.Context(), h.config(), h.store, "noclass", p.spec(nil))
	if !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want the provider fault", err)
	}
	_, statuses, _, _ := p.counts()
	if statuses != 1 {
		t.Errorf("statuses = %d, want one call with no retry", statuses)
	}
}

// TestProviderFailureIsTerminalAndNeverRetried: a failed verdict ends the
// Run with the provider's code, records the verdict on the row, and runs
// no second poll.
func TestProviderFailureIsTerminalAndNeverRetried(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{}, step{st: providertask.Status{
		State: providertask.StateFailed,
		Code:  "unit_limit",
	}})
	cfg := h.config()
	cfg.Retryable = classify
	_, err := providertask.Run(t.Context(), cfg, h.store, "denied", p.spec(nil))
	var fail *providertask.TaskFailure
	if !errors.As(err, &fail) {
		t.Fatalf("err = %v, want a TaskFailure", err)
	}
	if !errors.Is(err, providertask.ErrTaskFailed) {
		t.Errorf("err = %v, want ErrTaskFailed in the chain", err)
	}
	if fail.Code != "unit_limit" {
		t.Errorf("code = %q, want the provider code unchanged", fail.Code)
	}
	_, statuses, _, keeps := p.counts()
	if statuses != 1 {
		t.Errorf("statuses = %d, want exactly one poll", statuses)
	}
	if keeps != 0 {
		t.Errorf("keeps = %d, want none for a failed task", keeps)
	}
	claim, err := h.store.Get(t.Context(), "denied")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.State != providertask.StateFailed || claim.Code != "unit_limit" {
		t.Errorf("claim = %+v, want the failed verdict recorded", claim)
	}
}

// TestUnknownStateStopsTheRun: a state outside the vocabulary is a fault
// the caller sees, not a poll the loop spins on.
func TestUnknownStateStopsTheRun(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{}, step{st: providertask.Status{State: providertask.State("arrived")}})
	_, err := providertask.Run(t.Context(), h.config(), h.store, "weird", p.spec(nil))
	if err == nil || errors.Is(err, providertask.ErrDeadline) {
		t.Fatalf("err = %v, want the unknown state refused", err)
	}
	_, statuses, _, _ := p.counts()
	if statuses != 1 {
		t.Errorf("statuses = %d, want one call with no retry", statuses)
	}
}

// TestKeepFailureKeepsTheVerdictAndTheCharge: a Keep that fails fails the
// Run, and the recorded success plus the settled charge stay. A later Run
// fetches and keeps again without creating or settling twice.
func TestKeepFailureKeepsTheVerdictAndTheCharge(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 10
	p := newFakeProvider(rendered{Link: "expiring"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	p.keepErr = errors.New("providertask test: copy failed")
	_, err := providertask.Run(t.Context(), cfg, h.store, "copyfail", p.spec(p.Keep))
	if err == nil || !errors.Is(err, p.keepErr) {
		t.Fatalf("err = %v, want the keep fault", err)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("spent = %d, want the estimate: the provider succeeded", got)
	}
	claim, err := h.store.Get(t.Context(), "copyfail")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.State != providertask.StateSucceeded {
		t.Errorf("state = %s, want the recorded success", claim.State)
	}

	// A later Run collects the recorded outcome and keeps again.
	p.keepErr = nil
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "copyfail", p.spec(p.Keep))
	if err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if outcome.Value.Link != "expiring" {
		t.Errorf("value = %+v, want the fetched result", outcome.Value)
	}
	// The counters are cumulative, and the collecting run added nothing:
	// one create and one poll in total, both from the Run that reached the
	// verdict.
	creates, statuses, results, keeps := p.counts()
	if creates != 1 || statuses != 1 {
		t.Errorf("creates=%d statuses=%d, want the originals untouched", creates, statuses)
	}
	if results != 2 || keeps != 2 {
		t.Errorf("results=%d keeps=%d, want one fetch and one keep per Run", results, keeps)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("spent = %d, want the single original charge", got)
	}
}

// TestBackoffStaysInsideTheConfiguredBand: the waits between polls rise on
// the jitter curve from the base to the ceiling, and no wait falls below
// half the current step, because the jitter spans the upper half.
func TestBackoffStaysInsideTheConfiguredBand(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{Link: "done"},
		step{st: providertask.Status{State: providertask.StateRunning}},
		step{st: providertask.Status{State: providertask.StateRunning}},
		step{st: providertask.Status{State: providertask.StateRunning}},
		step{st: providertask.Status{State: providertask.StateRunning}},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfg := h.config()
	cfg.PollBase = 30 * time.Millisecond
	cfg.PollCeiling = 120 * time.Millisecond
	var (
		mu   sync.Mutex
		last time.Time
		gaps []time.Duration
	)
	inner := p.Status
	spec := p.spec(nil)
	spec.Status = func(ctx context.Context, taskID string) (providertask.Status, error) {
		now := time.Now()
		mu.Lock()
		if !last.IsZero() {
			gaps = append(gaps, now.Sub(last))
		}
		last = now
		mu.Unlock()
		return inner(ctx, taskID)
	}
	if _, err := providertask.Run(t.Context(), cfg, h.store, "paced", spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The band for wait n is the upper half of the doubled base, capped at
	// the ceiling. Timers never fire early, so the lower bounds hold with
	// a small slack, and the upper bounds allow scheduler delay.
	bands := []struct{ low, high time.Duration }{
		{10 * time.Millisecond, 200 * time.Millisecond},
		{20 * time.Millisecond, 250 * time.Millisecond},
		{50 * time.Millisecond, 400 * time.Millisecond},
		{50 * time.Millisecond, 400 * time.Millisecond},
	}
	if len(gaps) != len(bands) {
		t.Fatalf("waits = %d, want %d", len(gaps), len(bands))
	}
	for i, gap := range gaps {
		if gap < bands[i].low || gap > bands[i].high {
			t.Errorf("wait %d = %s, want inside [%s, %s]", i+1, gap, bands[i].low, bands[i].high)
		}
	}
}

// TestRunRejectsInvalidInput: a nil store, an empty key and a half built
// spec refuse before anything is claimed or created.
func TestRunRejectsInvalidInput(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	ctx := t.Context()
	if _, err := providertask.Run(ctx, h.config(), nil, "k", p.spec(nil)); !errors.Is(err, providertask.ErrInvalid) {
		t.Errorf("nil store: err = %v, want ErrInvalid", err)
	}
	if _, err := providertask.Run(ctx, h.config(), (*sqlitestore.Store)(nil), "k", p.spec(nil)); !errors.Is(err, providertask.ErrInvalid) {
		t.Errorf("typed nil store: err = %v, want ErrInvalid", err)
	}
	if _, err := providertask.Run(ctx, h.config(), h.store, "", p.spec(nil)); !errors.Is(err, providertask.ErrInvalid) {
		t.Errorf("empty key: err = %v, want ErrInvalid", err)
	}
	broken := p.spec(nil)
	broken.Status = nil
	if _, err := providertask.Run(ctx, h.config(), h.store, "k", broken); !errors.Is(err, providertask.ErrInvalid) {
		t.Errorf("missing status: err = %v, want ErrInvalid", err)
	}
	if got := h.scalar(`SELECT COUNT(*) FROM providertask_task`); got != 0 {
		t.Errorf("store holds %d rows, want none: nothing was claimed", got)
	}
}

// TestCreateErrorLeavesTheKeyUnresolved: a create that failed mid call
// leaves the key claimed, and a later Run refuses it rather than create
// again.
func TestCreateErrorLeavesTheKeyUnresolved(t *testing.T) {
	h := newHarness(t)
	createErr := errors.New("providertask test: provider unreachable")
	p := newFakeProvider(rendered{})
	p.createErr = createErr
	_, err := providertask.Run(t.Context(), h.config(), h.store, "lostcreate", p.spec(nil))
	if !errors.Is(err, createErr) {
		t.Fatalf("err = %v, want the create fault", err)
	}
	again := newFakeProvider(rendered{Link: "x"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	cfg := h.config()
	cfg.Deadline = 50 * time.Millisecond
	_, err = providertask.Run(t.Context(), cfg, h.store, "lostcreate", again.spec(nil))
	if !errors.Is(err, providertask.ErrTaskPending) {
		t.Fatalf("err = %v, want ErrTaskPending", err)
	}
	creates, _, _, _ := again.counts()
	if creates != 0 {
		t.Errorf("later run created %d tasks, want none", creates)
	}
}

// TestCreateWithoutAnIDIsRefused: a create that names no task cannot be
// polled or resumed, so the Run refuses it.
func TestCreateWithoutAnIDIsRefused(t *testing.T) {
	h := newHarness(t)
	p := newFakeProvider(rendered{})
	empty := p.spec(nil)
	empty.Create = func(ctx context.Context) (string, error) { return "", nil }
	_, err := providertask.Run(t.Context(), h.config(), h.store, "noid", empty)
	if !errors.Is(err, providertask.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestRefusedReservationReleasesTheClaimAndRunsNothing: a meter that
// refuses the estimate releases the still empty claim, so a later Run with
// headroom runs the key from the start.
func TestRefusedReservationReleasesTheClaimAndRunsNothing(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 5)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 10
	p := newFakeProvider(rendered{Link: "x"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	_, err := providertask.Run(t.Context(), cfg, h.store, "nopool", p.spec(nil))
	if !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	creates, _, _, _ := p.counts()
	if creates != 0 {
		t.Errorf("creates = %d, want none: the refusal must come first", creates)
	}
	if got := h.scalar(`SELECT COUNT(*) FROM providertask_task WHERE key = 'nopool'`); got != 0 {
		t.Errorf("claim rows = %d, want none: the empty claim was released", got)
	}

	// The key is usable again once the account has headroom.
	bigBudget, bigLedger := meterOf(t, 100)
	cfg.Meter = mustMeter(t, bigBudget, bigLedger)
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "nopool", p.spec(nil))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if outcome.Value.Link != "x" {
		t.Errorf("value = %+v, want the fetched result", outcome.Value)
	}
	creates, _, _, _ = p.counts()
	if creates != 1 {
		t.Errorf("creates = %d, want the one create of the second run", creates)
	}
}

// TestMeterSettlesOnSuccessAndNamesTheCharge: a succeeded task settles at
// the estimate, frees the reservation, and the charge carries the kind,
// the key as its reference and the account's denomination.
func TestMeterSettlesOnSuccessAndNamesTheCharge(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 25
	cfg.Kind = "upscale"
	p := newFakeProvider(rendered{Link: "x"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "charged", p.spec(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := budget.Spent(); got != 25 {
		t.Errorf("spent = %d, want the estimate", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("reserved = %d, want nothing held", got)
	}
	if outcome.Usage != (cost.Usage{Price: 25, Measured: false}) {
		t.Errorf("usage = %+v, want the settled estimate marked unmeasured", outcome.Usage)
	}
	charges := ledger.Charges()
	if len(charges) != 1 {
		t.Fatalf("charges = %d, want one", len(charges))
	}
	c := charges[0]
	if c.Kind != "upscale" || c.Ref != "charged" || c.UnitPrice != 25 || c.Denomination != budget.Denomination() {
		t.Errorf("charge = %+v, want kind upscale, ref charged, price 25, the budget unit", c)
	}
}

// TestMeterSettlesTheMeasuredPrice: a price the spec measures from the
// result wins over the estimate, and the usage says it was measured.
func TestMeterSettlesTheMeasuredPrice(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 10
	p := newFakeProvider(rendered{Link: "x"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	p.price = func() (cost.Price, error) { return 34, nil }
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "measured", p.spec(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := budget.Spent(); got != 34 {
		t.Errorf("spent = %d, want the measured price", got)
	}
	if outcome.Usage != (cost.Usage{Price: 34, Measured: true}) {
		t.Errorf("usage = %+v, want the measured price marked measured", outcome.Usage)
	}
}

// TestMeterReleasesOnProviderFailure: a failed task the provider does not
// charge for leaves no spend and no hold.
func TestMeterReleasesOnProviderFailure(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 20
	p := newFakeProvider(rendered{}, step{st: providertask.Status{
		State: providertask.StateFailed,
		Code:  "unit_limit",
	}})
	_, err := providertask.Run(t.Context(), cfg, h.store, "freeride", p.spec(nil))
	var fail *providertask.TaskFailure
	if !errors.As(err, &fail) {
		t.Fatalf("err = %v, want a TaskFailure", err)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("spent = %d, want nothing booked", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("reserved = %d, want the hold freed", got)
	}
	if len(ledger.Charges()) != 0 {
		t.Errorf("charges = %d, want none", len(ledger.Charges()))
	}
}

// TestChargesOnFailureSettlesTheEstimate: a provider that charges for a
// failed task books the estimate and still returns the verdict.
func TestChargesOnFailureSettlesTheEstimate(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 20
	cfg.ChargesOnFailure = true
	p := newFakeProvider(rendered{}, step{st: providertask.Status{
		State: providertask.StateFailed,
		Code:  "content_broken",
	}})
	_, err := providertask.Run(t.Context(), cfg, h.store, "chargedfail", p.spec(nil))
	var fail *providertask.TaskFailure
	if !errors.As(err, &fail) || fail.Code != "content_broken" {
		t.Fatalf("err = %v, want the verdict", err)
	}
	if got := budget.Spent(); got != 20 {
		t.Errorf("spent = %d, want the estimate booked for the failed task", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("reserved = %d, want nothing held", got)
	}
	if len(ledger.Charges()) != 1 {
		t.Errorf("charges = %d, want the failure charge recorded", len(ledger.Charges()))
	}
}

// TestPriceFaultFailsTheRunAndKeepsTheRowOpen: a price the spec cannot
// measure frees the reservation and fails the Run, and the open row lets a
// later Run fetch and price again.
func TestPriceFaultFailsTheRunAndKeepsTheRowOpen(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 10
	p := newFakeProvider(rendered{Link: "x"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	priceErr := errors.New("providertask test: no usage in the result")
	p.price = func() (cost.Price, error) { return 0, priceErr }
	_, err := providertask.Run(t.Context(), cfg, h.store, "noprice", p.spec(nil))
	if !errors.Is(err, priceErr) {
		t.Fatalf("err = %v, want the price fault", err)
	}
	if got := budget.Spent(); got != 0 {
		t.Errorf("spent = %d, want nothing booked", got)
	}
	if got := budget.Reserved(); got != 0 {
		t.Errorf("reserved = %d, want the hold freed", got)
	}
	claim, err := h.store.Get(t.Context(), "noprice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.State != providertask.StateRunning || claim.TaskID == "" {
		t.Errorf("claim = %+v, want the recorded task left open", claim)
	}
}

// TestConcurrentRunsCreateAndSettleOnce: eight Runs of one key share one
// flight. The leader creates and settles once, the joiners wait for the
// recorded outcome and collect it, and every Run returns the result.
func TestConcurrentRunsCreateAndSettleOnce(t *testing.T) {
	h := newHarness(t)
	budget, ledger := meterOf(t, 1000)
	cfg := h.config()
	cfg.Coordinator = providertask.NewCoordinator()
	cfg.Meter = mustMeter(t, budget, ledger)
	cfg.Estimate = 10
	cfg.PollBase = 2 * time.Millisecond
	cfg.PollCeiling = 4 * time.Millisecond

	p := newFakeProvider(rendered{Link: "shared"})
	p.steps = []step{
		{st: providertask.Status{State: providertask.StateRunning}},
		{st: providertask.Status{State: providertask.StateSucceeded}},
	}
	leaderSpec := p.spec(p.Keep)
	innerCreate := p.Create
	leaderSpec.Create = func(ctx context.Context) (string, error) {
		time.Sleep(25 * time.Millisecond)
		return innerCreate(ctx)
	}

	const runs = 8
	type answer struct {
		taskID string
		link   string
	}
	answers := make([]answer, runs)
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcome, err := providertask.Run(t.Context(), cfg, h.store, "hot", leaderSpec)
			answers[i] = answer{taskID: outcome.TaskID, link: outcome.Value.Link}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for i := 1; i < runs; i++ {
		if answers[i] != answers[0] {
			t.Errorf("answer %d = %+v, want the shared %+v", i, answers[i], answers[0])
		}
	}
	creates, statuses, results, keeps := p.counts()
	if creates != 1 {
		t.Errorf("creates = %d, want exactly one across every Run", creates)
	}
	if statuses != 2 {
		t.Errorf("statuses = %d, want only the leader's two polls", statuses)
	}
	if results != runs || keeps != runs {
		t.Errorf("results=%d keeps=%d, want one of each per Run", results, keeps)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("spent = %d, want the single charge of the leader", got)
	}
	if len(ledger.Charges()) != 1 {
		t.Errorf("charges = %d, want the leader's one charge", len(ledger.Charges()))
	}
}

// TestJoinerTimesOutWithTaskPending: a joiner whose leader is still
// creating refuses with ErrTaskPending when its own deadline passes,
// without creating or polling anything.
func TestJoinerTimesOutWithTaskPending(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	cfg.Coordinator = providertask.NewCoordinator()

	release := make(chan struct{})
	p := newFakeProvider(rendered{Link: "x"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	leaderSpec := p.spec(nil)
	innerCreate := p.Create
	leaderSpec.Create = func(ctx context.Context) (string, error) {
		<-release
		return innerCreate(ctx)
	}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := providertask.Run(t.Context(), cfg, h.store, "joined", leaderSpec)
		leaderDone <- err
	}()

	// The leader holds the flight once its claim is on disk, so waiting
	// for the row makes the joiner's join lose deterministically.
	for i := 0; i < 500; i++ {
		if _, err := h.store.Get(t.Context(), "joined"); err == nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := h.store.Get(t.Context(), "joined"); err != nil {
		t.Fatalf("leader never claimed: %v", err)
	}

	joinerCfg := cfg
	joinerCfg.Deadline = 80 * time.Millisecond
	_, err := providertask.Run(t.Context(), joinerCfg, h.store, "joined", p.spec(nil))
	if !errors.Is(err, providertask.ErrTaskPending) {
		t.Errorf("err = %v, want ErrTaskPending", err)
	}
	_, statuses, _, _ := p.counts()
	if statuses != 0 {
		t.Errorf("statuses = %d, want none: a joiner never polls", statuses)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader Run: %v", err)
	}
}

// gatedStore holds a Run at its Claim, so a second Run can read the same
// row while the first still holds the flight. That is the two caller shape
// the stale verdict pin needs.
type gatedStore struct {
	providertask.Store
	gate chan struct{}
}

func (g *gatedStore) Claim(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (providertask.Claim, bool, error) {
	<-g.gate
	return g.Store.Claim(ctx, key, owner, now, ttl)
}

// TestStaleVerdictsAreHonouredAtAnyAge: a recorded verdict older than the
// query window is answered from the row, never refused as stale. Two
// callers of one stale failed row get the identical stored code, whatever
// path each takes to the row.
func TestStaleVerdictsAreHonouredAtAnyAge(t *testing.T) {
	h := newHarness(t)
	old := h.base.Add(-25 * time.Hour)
	cfg := providertask.Config{
		Window: 24 * time.Hour,
		Log:    h.log,
		Now:    func() time.Time { return h.base },
	}
	plant := func(key, taskID string, state providertask.State, code string) {
		t.Helper()
		if _, _, err := h.store.Claim(t.Context(), key, "test", old, time.Minute); err != nil {
			t.Fatalf("Claim %s: %v", key, err)
		}
		if err := h.store.Record(t.Context(), key, taskID); err != nil {
			t.Fatalf("Record %s: %v", key, err)
		}
		if err := h.store.Finish(t.Context(), key, 1, state, code); err != nil {
			t.Fatalf("Finish %s: %v", key, err)
		}
	}
	plant("stale-lost", "task-lost", providertask.StateFailed, "unit_limit")

	// A plain Run answers from the row: the stored verdict, not the window.
	p := newFakeProvider(rendered{}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	_, err := providertask.Run(t.Context(), cfg, h.store, "stale-lost", p.spec(nil))
	var fail *providertask.TaskFailure
	if !errors.As(err, &fail) || fail.Code != "unit_limit" || fail.TaskID != "task-lost" {
		t.Fatalf("err = %v, want the stored verdict", err)
	}
	creates, statuses, _, _ := p.counts()
	if creates != 0 || statuses != 0 {
		t.Errorf("creates=%d statuses=%d, want none: the verdict is already recorded", creates, statuses)
	}

	// A recorded success past the window is collected, not refused.
	plant("stale-won", "task-won", providertask.StateSucceeded, "")
	pw := newFakeProvider(rendered{Link: "kept"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	outcome, err := providertask.Run(t.Context(), cfg, h.store, "stale-won", pw.spec(nil))
	if err != nil {
		t.Fatalf("stale success Run: %v", err)
	}
	if outcome.TaskID != "task-won" || outcome.Value.Link != "kept" {
		t.Errorf("outcome = %+v, want the recorded task fetched again", outcome)
	}
	if creates, statuses, results, _ := pw.counts(); creates != 0 || statuses != 0 || results != 1 {
		t.Errorf("creates=%d statuses=%d results=%d, want only the fetch", creates, statuses, results)
	}

	// Two overlapping callers of the stale failed row read the same row and
	// answer with the identical stored code, whichever one leads.
	coord := providertask.NewCoordinator()
	cfg.Coordinator = coord
	joinerCfg := cfg
	gate := make(chan struct{})
	gated := &gatedStore{Store: h.store, gate: gate}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := providertask.Run(t.Context(), cfg, gated, "stale-lost", p.spec(nil))
		leaderDone <- err
	}()
	joinerDone := make(chan error, 1)
	go func() {
		_, err := providertask.Run(t.Context(), joinerCfg, h.store, "stale-lost", p.spec(nil))
		joinerDone <- err
	}()
	var joinFail *providertask.TaskFailure
	select {
	case err := <-joinerDone:
		if !errors.As(err, &joinFail) || joinFail.Code != "unit_limit" || joinFail.TaskID != "task-lost" {
			t.Fatalf("joiner err = %v, want the stored verdict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("joiner never resolved")
	}
	close(gate)
	leadErr := <-leaderDone
	var leadFail *providertask.TaskFailure
	if !errors.As(leadErr, &leadFail) || leadFail.Code != "unit_limit" || leadFail.TaskID != "task-lost" {
		t.Fatalf("leader err = %v, want the stored verdict", leadErr)
	}
	if joinFail.Code != fail.Code || joinFail.TaskID != fail.TaskID {
		t.Errorf("joiner answered %+v, want the first caller's %+v", joinFail, fail)
	}
}
