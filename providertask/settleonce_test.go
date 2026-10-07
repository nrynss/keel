package providertask_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/providertask"
)

// finishFaultStore wraps a store and refuses one terminal record. The row it
// leaves is the row a crash between the settle and the record leaves: the
// task id recorded, the charge booked, the verdict unrecorded.
type finishFaultStore struct {
	providertask.Store
	mu    sync.Mutex
	fails int
}

// Finish passes through until its single fault is spent, then behaves as the
// wrapped store does.
func (s *finishFaultStore) Finish(ctx context.Context, key string, state providertask.State, code string) error {
	s.mu.Lock()
	pending := s.fails > 0
	if pending {
		s.fails--
	}
	s.mu.Unlock()
	if pending {
		return errors.New("providertask test: terminal record refused")
	}
	return s.Store.Finish(ctx, key, state, code)
}

// TestResumeAfterCrashBetweenSettleAndFinishBooksOnce pins the settle-once
// promise end to end. The first Run settles the task and dies before the
// verdict lands, which the fault store stages. The resume runs on a second
// store and a second Coordinator over the same file, polls the recorded task
// again, and its settle books nothing. The ledger and the budget each show
// the one charge the first settle booked, and the verdict the resume records
// closes the key.
func TestResumeAfterCrashBetweenSettleAndFinishBooksOnce(t *testing.T) {
	h := newHarness(t)
	_, storeB := openAt(t, h.path)
	coordinatorA := providertask.NewCoordinator()
	coordinatorB := providertask.NewCoordinator()
	budget, ledger := meterOf(t, 1000)
	meter := mustMeter(t, budget, ledger)
	storeA := &finishFaultStore{Store: h.store, fails: 1}

	p := newFakeProvider(rendered{Link: "settled"},
		step{st: providertask.Status{State: providertask.StateRunning}},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)
	cfgA := h.config()
	cfgA.Meter = meter
	cfgA.Estimate = 10
	cfgA.Coordinator = coordinatorA
	outcome, err := providertask.Run(t.Context(), cfgA, storeA, "crash-settle", p.spec(p.Keep))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if outcome.Value.Link != "settled" {
		t.Errorf("first outcome = %+v, want the fetched result", outcome)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() after the crash = %d, want the one settled estimate", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Fatalf("charges after the crash = %d rows, want 1", len(charges))
	}
	// The verdict never landed, so the row stands where a crash left it.
	claim, err := storeB.Get(t.Context(), "crash-settle")
	if err != nil {
		t.Fatalf("Get after the crash: %v", err)
	}
	if claim.State != providertask.StateRunning || claim.TaskID == "" {
		t.Fatalf("row after the crash = %+v, want a recorded running task", claim)
	}

	cfgB := h.config()
	cfgB.Meter = meter
	cfgB.Estimate = 10
	cfgB.Coordinator = coordinatorB
	outcomeB, err := providertask.Run(t.Context(), cfgB, storeB, "crash-settle", p.spec(p.Keep))
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if outcomeB.Value.Link != "settled" {
		t.Errorf("resume outcome = %+v, want the fetched result", outcomeB)
	}
	if outcomeB.Usage != (cost.Usage{}) {
		t.Errorf("resume usage = %+v, want the zero usage, the resume booked nothing", outcomeB.Usage)
	}
	// One create from the first process, one poll of the recorded task per
	// process, and one fetch and keep per Run.
	creates, statuses, results, keeps := p.counts()
	if creates != 1 {
		t.Errorf("creates = %d, want 1, the resume never creates", creates)
	}
	if statuses != 3 {
		t.Errorf("statuses = %d, want 3, two polls before the crash and one on the resume", statuses)
	}
	if results != 2 || keeps != 2 {
		t.Errorf("results=%d keeps=%d, want one fetch and one keep per Run", results, keeps)
	}
	if got := budget.Spent(); got != 10 {
		t.Errorf("Spent() after the resume = %d, want the single first booking", got)
	}
	if charges := ledger.Charges(); len(charges) != 1 {
		t.Errorf("charges after the resume = %d rows, want 1, the sink never sees the repeat", len(charges))
	}
	claim, err = storeB.Get(t.Context(), "crash-settle")
	if err != nil {
		t.Fatalf("Get after the resume: %v", err)
	}
	if claim.State != providertask.StateSucceeded {
		t.Errorf("state after the resume = %s, want the recorded verdict", claim.State)
	}
}
