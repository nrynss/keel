package erase_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/job"
	jobsql "github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
)

// target is one erase target the tests count calls on. A blocking target
// holds its delete until its context is done. fail decides per call, and a
// nil fail confirms at once.
type target struct {
	name  string
	block bool
	mu    sync.Mutex
	calls int
	fail  func(call int) error
}

// Name identifies the target.
func (t *target) Name() string { return t.name }

// Delete counts one call, then blocks or applies the fail decision.
func (t *target) Delete(ctx context.Context) error {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if t.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if t.fail == nil {
		return nil
	}
	return t.fail(call)
}

// count returns the calls so far.
func (t *target) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// harness wires one Runner over a fresh database with the erase kind
// registered. Its Source rebuilds nothing, because these tests never
// restart.
type harness struct {
	runner *job.Runner
	er     *erase.Eraser
	dir    string
}

// newHarness opens the harness with cfg.
func newHarness(t *testing.T, cfg erase.Config) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := jobsql.Open(ctx, jobsql.Config{DB: db})
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	er, err := erase.New(func(ctx context.Context, ref string) ([]erase.Target, error) {
		return nil, fmt.Errorf("harness rebuilds nothing")
	}, cfg)
	if err != nil {
		t.Fatalf("erase new: %v", err)
	}
	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{
		Broker: broker,
		Store:  store,
		Kinds:  map[string]job.Kind{erase.KindName: er.Kind()},
	})
	if err != nil {
		t.Fatalf("job open: %v", err)
	}
	return &harness{runner: runner, er: er, dir: dir}
}

// waitTerminal polls the lineage of id until its last attempt reaches a
// terminal state and returns that attempt as a Result.
func waitTerminal(t *testing.T, ctx context.Context, r *job.Runner, id string) job.Result {
	t.Helper()
	attempts := waitAttempts(t, ctx, r, id, 1)
	last := attempts[len(attempts)-1]
	return job.Result{ID: last.ID, Status: last.Status, Data: last.Data, Err: last.Err}
}

// waitAttempts polls the lineage of id until it holds at least n attempts
// and the last one is terminal.
func waitAttempts(t *testing.T, ctx context.Context, r *job.Runner, id string, n int) []job.Record {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		attempts, err := r.Attempts(ctx, id)
		if err != nil {
			t.Fatalf("Attempts %s: %v", id, err)
		}
		if len(attempts) >= n {
			switch last := attempts[len(attempts)-1]; last.Status {
			case job.StatusDone, job.StatusError, job.StatusCancelled, job.StatusInterrupted:
				return attempts
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d terminal attempts of %s", n, id)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRetryHitsOnlyTheFailingTarget registers three targets where one fails
// twice and then confirms. The pin is the call count: the failing target
// spends its tries, and the two that confirmed are never called again.
func TestRetryHitsOnlyTheFailingTarget(t *testing.T) {
	h := newHarness(t, erase.Config{})
	alpha := &target{name: "alpha"}
	beta := &target{name: "beta", fail: func(call int) error {
		if call < 3 {
			return errors.New("provider unavailable")
		}
		return nil
	}}
	omega := &target{name: "omega"}

	ctx := context.Background()
	id, err := h.er.Start(ctx, h.runner, "account-9", []erase.Target{alpha, beta, omega})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	res := waitTerminal(t, ctx, h.runner, id)
	if res.Status != job.StatusDone {
		t.Fatalf("status = %q with error %v, want %q", res.Status, res.Err, job.StatusDone)
	}
	if got := alpha.count(); got != 1 {
		t.Errorf("alpha calls = %d, want 1", got)
	}
	if got := beta.count(); got != 3 {
		t.Errorf("beta calls = %d, want 3, because only the failing target is retried", got)
	}
	if got := omega.count(); got != 1 {
		t.Errorf("omega calls = %d, want 1", got)
	}

	rep, err := h.er.Inspect(ctx, h.runner, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !rep.Complete() {
		t.Errorf("Complete() = false, want true with stuck %v", rep.Stuck)
	}
	if got, want := rep.Confirmed, []string{"alpha", "beta", "omega"}; !equal(got, want) {
		t.Errorf("Confirmed = %v, want %v", got, want)
	}
	if len(rep.Stuck) != 0 {
		t.Errorf("Stuck = %v, want empty", rep.Stuck)
	}
	if rep.Ref != "account-9" {
		t.Errorf("Ref = %q, want account-9", rep.Ref)
	}
}

// TestStuckTargetLeavesTheErasureUnfinished pins the failure shape: a
// target that never confirms fails the job, and the ledger names it as
// stuck rather than reporting a success.
func TestStuckTargetLeavesTheErasureUnfinished(t *testing.T) {
	h := newHarness(t, erase.Config{})
	alpha := &target{name: "alpha"}
	beta := &target{name: "beta", fail: func(call int) error {
		return errors.New("provider refused")
	}}
	omega := &target{name: "omega"}

	ctx := context.Background()
	id, err := h.er.Start(ctx, h.runner, "account-4", []erase.Target{alpha, beta, omega})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	res := waitTerminal(t, ctx, h.runner, id)
	if res.Status != job.StatusError {
		t.Fatalf("status = %q, want %q, because a stuck erasure is never a success", res.Status, job.StatusError)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "beta") {
		t.Errorf("error = %v, want it to name beta", res.Err)
	}
	if got := beta.count(); got != 3 {
		t.Errorf("beta calls = %d, want the default of 3 tries", got)
	}

	rep, err := h.er.Inspect(ctx, h.runner, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if rep.Complete() {
		t.Error("Complete() = true, want false for a stuck erasure")
	}
	if got, want := rep.Stuck, []string{"beta"}; !equal(got, want) {
		t.Errorf("Stuck = %v, want %v", got, want)
	}
	if got, want := rep.Confirmed, []string{"alpha", "omega"}; !equal(got, want) {
		t.Errorf("Confirmed = %v, want %v", got, want)
	}
}

// TestAlreadyGoneCountsAsConfirmed pins that a target answering with
// ErrGone confirms on its first call.
func TestAlreadyGoneCountsAsConfirmed(t *testing.T) {
	h := newHarness(t, erase.Config{})
	gone := &target{name: "gone", fail: func(call int) error {
		return fmt.Errorf("remote says absent: %w", erase.ErrGone)
	}}
	live := &target{name: "live"}

	ctx := context.Background()
	id, err := h.er.Start(ctx, h.runner, "account-2", []erase.Target{gone, live})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	res := waitTerminal(t, ctx, h.runner, id)
	if res.Status != job.StatusDone {
		t.Fatalf("status = %q with error %v, want %q", res.Status, res.Err, job.StatusDone)
	}
	if got := gone.count(); got != 1 {
		t.Errorf("gone calls = %d, want 1, because already gone confirms at once", got)
	}
	rep, err := h.er.Inspect(ctx, h.runner, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got, want := rep.Confirmed, []string{"gone", "live"}; !equal(got, want) {
		t.Errorf("Confirmed = %v, want %v", got, want)
	}
}

// TestCancelStopsTheFanOut pins that a cancelled erasure lands the
// cancelled terminal instead of an error or a success.
func TestCancelStopsTheFanOut(t *testing.T) {
	h := newHarness(t, erase.Config{})
	blocked := &target{name: "blocked", block: true}
	quick := &target{name: "quick"}

	ctx := context.Background()
	id, err := h.er.Start(ctx, h.runner, "account-7", []erase.Target{blocked, quick})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for blocked.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the blocked target to be called")
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.runner.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	res := waitTerminal(t, ctx, h.runner, id)
	if res.Status != job.StatusCancelled {
		t.Errorf("status = %q, want %q", res.Status, job.StatusCancelled)
	}
}

// TestStartRefusesUnusableInput pins the input checks, so no erasure is
// ever recorded with a list the ledger cannot represent.
func TestStartRefusesUnusableInput(t *testing.T) {
	h := newHarness(t, erase.Config{})
	ctx := context.Background()

	if _, err := h.er.Start(ctx, nil, "ref", []erase.Target{&target{name: "a"}}); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("nil runner error = %v, want ErrInvalid", err)
	}
	if _, err := h.er.Start(ctx, h.runner, "ref", nil); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("empty list error = %v, want ErrInvalid", err)
	}
	if _, err := h.er.Start(ctx, h.runner, "ref", []erase.Target{&target{name: ""}}); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("empty name error = %v, want ErrInvalid", err)
	}
	if _, err := h.er.Start(ctx, h.runner, "ref", []erase.Target{&target{name: "a"}, nil}); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("nil target error = %v, want ErrInvalid", err)
	}
	if _, err := h.er.Start(ctx, h.runner, "ref", []erase.Target{&target{name: "a"}, &target{name: "a"}}); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("repeated name error = %v, want ErrInvalid", err)
	}
	if _, err := erase.New(nil, erase.Config{}); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("nil source error = %v, want ErrInvalid", err)
	}
}

// TestInspectClassifiesUnknownAndForeignJobs pins what Inspect says for an
// id that holds no erasure.
func TestInspectClassifiesUnknownAndForeignJobs(t *testing.T) {
	h := newHarness(t, erase.Config{})
	ctx := context.Background()

	if _, err := h.er.Inspect(ctx, h.runner, "missing"); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("unknown id error = %v, want job.ErrUnknownJob", err)
	}

	other, err := h.runner.Start(ctx, func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("start other: %v", err)
	}
	res := waitTerminal(t, ctx, h.runner, other)
	if res.Status != job.StatusDone {
		t.Fatalf("other status = %q, want %q", res.Status, job.StatusDone)
	}
	if _, err := h.er.Inspect(ctx, h.runner, other); !errors.Is(err, erase.ErrInvalid) {
		t.Errorf("foreign kind error = %v, want ErrInvalid", err)
	}
}

// TestResumedRunKeepsDroppedTargetsStuck pins that a target the rebuilt
// list drops while unconfirmed fails the erasure, because the ledger still
// owes its delete. A run that confirms nothing may still be stuck, and the
// ledger, not the attempt, decides.
func TestResumedRunKeepsDroppedTargetsStuck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := jobsql.Open(ctx, jobsql.Config{DB: db})
	if err != nil {
		t.Fatalf("store open: %v", err)
	}

	alpha := &target{name: "alpha"}
	slow := &target{name: "slow", block: true}
	list := []erase.Target{alpha, slow}
	er, err := erase.New(func(ctx context.Context, ref string) ([]erase.Target, error) {
		return list, nil
	}, erase.Config{})
	if err != nil {
		t.Fatalf("erase new: %v", err)
	}
	kinds := func() map[string]job.Kind {
		return map[string]job.Kind{erase.KindName: er.Kind()}
	}
	first, err := job.Open(ctx, job.Config{Broker: stream.New(stream.Config{Heartbeat: time.Hour}), Store: store, Kinds: kinds()})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	id, err := er.Start(ctx, first, "account-3", list)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Cancel(id) // cleanup: frees the blocked delete once the pins are read
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		attempts, err := first.Attempts(ctx, id)
		if err != nil {
			t.Fatalf("attempts: %v", err)
		}
		if len(attempts) == 1 && strings.Contains(string(attempts[0].Progress.Detail), "alpha") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the durable confirmation")
		}
		time.Sleep(time.Millisecond)
	}

	// The restart carries a list that lost the unconfirmed target.
	list = []erase.Target{alpha}
	second, err := job.Open(ctx, job.Config{Broker: stream.New(stream.Config{Heartbeat: time.Hour}), Store: store, Kinds: kinds()})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}

	attempts := waitAttempts(t, ctx, second, id, 2)
	if attempts[0].Status != job.StatusInterrupted {
		t.Errorf("first attempt status = %q, want %q", attempts[0].Status, job.StatusInterrupted)
	}
	if attempts[1].Status != job.StatusError {
		t.Fatalf("resumed attempt status = %q, want %q, because slow is still owed a delete", attempts[1].Status, job.StatusError)
	}
	if attempts[1].Err == nil || !strings.Contains(attempts[1].Err.Error(), "slow") {
		t.Errorf("resumed error = %v, want it to name slow", attempts[1].Err)
	}

	rep, err := er.Inspect(ctx, second, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if rep.Complete() {
		t.Error("Complete() = true, want false")
	}
	if got, want := rep.Stuck, []string{"slow"}; !equal(got, want) {
		t.Errorf("Stuck = %v, want %v", got, want)
	}
	if got, want := rep.Confirmed, []string{"alpha"}; !equal(got, want) {
		t.Errorf("Confirmed = %v, want %v", got, want)
	}
}

// equal reports whether two name lists hold the same names in the same
// order.
func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
