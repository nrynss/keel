package providertask_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/providertask"
)

// backstopDeadline is the Config.Deadline a test with a manual deadline
// sets. It only bounds a test that never reaches its firing point, so a
// regression fails within a minute instead of hanging the suite. It never
// decides a passing run.
const backstopDeadline = time.Minute

// manualDeadline is a parent context whose deadline passes when the test
// calls pass, never on the wall clock. A Run's deadline context is derived
// from its parent, so passing this one ends the Run's provider calls with
// context.DeadlineExceeded exactly as an expired Config.Deadline does, but
// at a point the test chose. A wall-clock deadline of tens of milliseconds
// also covers the claim, the reservation, the create and the record, and a
// loaded machine spends that budget before the phase under test starts.
type manualDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

// newManualDeadline returns a deadline that has not passed, carrying the
// test context's values.
func newManualDeadline(t *testing.T) *manualDeadline {
	t.Helper()
	return &manualDeadline{Context: t.Context(), done: make(chan struct{})}
}

// Done closes when the deadline passes.
func (d *manualDeadline) Done() <-chan struct{} { return d.done }

// Err reports context.DeadlineExceeded once the deadline passed.
func (d *manualDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// pass makes the deadline pass. Calling it again changes nothing.
func (d *manualDeadline) pass() { d.once.Do(func() { close(d.done) }) }

// passAfterStatus wraps a spec's Status so the deadline passes after the
// first poll. A poll runs only once the task id is recorded, so the
// deadline lands inside the poll loop and nowhere earlier.
func passAfterStatus(d *manualDeadline, spec providertask.Spec[rendered]) providertask.Spec[rendered] {
	inner := spec.Status
	spec.Status = func(ctx context.Context, taskID string) (providertask.Status, error) {
		st, err := inner(ctx, taskID)
		d.pass()
		return st, err
	}
	return spec
}
