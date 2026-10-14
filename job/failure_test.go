package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nrynss/keel/wire"
)

// envelopeBody mirrors the error envelope body the wire package writes inside
// an error terminal, so a test reads the code, message and detail the stream
// carried.
type envelopeBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail"`
}

// decodeEnvelope reads the envelope body out of an error terminal payload.
// The payload's Error member is the same envelope a failed response carries,
// one object holding the code, message and detail under the error key.
func decodeEnvelope(t *testing.T, raw json.RawMessage) envelopeBody {
	t.Helper()
	var frame struct {
		Error envelopeBody `json:"error"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode envelope %s: %v", raw, err)
	}
	return frame.Error
}

// TestFailureNamesTerminalCode pins the failure-code contract. A Failure in
// the work function's error chain names the code and detail the error
// terminal carries, as a value or as a pointer. A wrapped Failure is still
// found. A blank code falls back to job_failed while its detail still
// travels. A chain with no Failure carries job_failed and no detail, which is
// what every error carried before Failure existed. A chain the Runner cannot
// read, because a walk or a message render panics inside it, reaches the
// terminal as the ErrPanic data point instead of ending the process.
func TestFailureNamesTerminalCode(t *testing.T) {
	cause := errors.New("reserve: over budget")
	cases := []struct {
		name       string
		err        error
		wantCode   string
		wantDetail string
		// wantCause marks an error that carries cause, so the terminal keeps
		// it in the chain.
		wantCause bool
		// wantPanic marks an error whose chain cannot be read, so the
		// terminal is the ErrPanic data point with the fallback code. The
		// error's own Error method is never called by this test.
		wantPanic bool
	}{
		{
			name:       "coded-failure",
			err:        Failure{Code: "budget_exhausted", Detail: map[string]int{"retry_after": 30}, Err: cause},
			wantCode:   "budget_exhausted",
			wantDetail: `{"retry_after":30}`,
			wantCause:  true,
		},
		{
			name:       "wrapped-failure",
			err:        fmt.Errorf("plan: %w", Failure{Code: "input_rejected", Detail: map[string]any{"field": "length"}, Err: cause}),
			wantCode:   "input_rejected",
			wantDetail: `{"field":"length"}`,
			wantCause:  true,
		},
		{
			name:       "coded-pointer-failure",
			err:        &Failure{Code: "budget_spent", Detail: map[string]int{"retry_after": 30}, Err: cause},
			wantCode:   "budget_spent",
			wantDetail: `{"retry_after":30}`,
			wantCause:  true,
		},
		{
			// The conversion to error keeps go vet's %w check quiet, which
			// asks for the value form. The wrapped pointer is the shape this
			// row pins.
			name:       "wrapped-pointer-failure",
			err:        fmt.Errorf("plan: %w", error(&Failure{Code: "quota_spent", Detail: map[string]any{"field": "length"}, Err: cause})),
			wantCode:   "quota_spent",
			wantDetail: `{"field":"length"}`,
			wantCause:  true,
		},
		{
			name:      "failure-without-detail",
			err:       Failure{Code: "provider_fault", Err: cause},
			wantCode:  "provider_fault",
			wantCause: true,
		},
		{
			name: "blank-code-falls-back",
			err: Failure{
				Code:   "   ",
				Detail: map[string]any{"field": "length"},
				Err:    cause,
			},
			wantCode:   "job_failed",
			wantDetail: `{"field":"length"}`,
			wantCause:  true,
		},
		{
			name:      "plain-error-falls-back",
			err:       cause,
			wantCode:  "job_failed",
			wantCause: true,
		},
		{
			name:      "nil-pointer-failure-panics",
			err:       (*Failure)(nil),
			wantCode:  "job_failed",
			wantPanic: true,
		},
		{
			name:      "value-over-nil-panics",
			err:       fmt.Errorf("outer: %w", Failure{Code: "a", Err: (*Failure)(nil)}),
			wantCode:  "job_failed",
			wantPanic: true,
		},
		{
			name:      "pointer-over-nil-panics",
			err:       fmt.Errorf("outer: %w", error(&Failure{Code: "a", Err: (*Failure)(nil)})),
			wantCode:  "job_failed",
			wantPanic: true,
		},
		{
			name:      "pointer-under-value-over-nil-panics",
			err:       Failure{Code: "a", Err: &Failure{Code: "b", Err: (*Failure)(nil)}},
			wantCode:  "job_failed",
			wantPanic: true,
		},
		{
			// The cancellation walk matches the join's first element, so the
			// panic lands in the message render rather than the walk.
			name:      "cancelled-join-over-nil-panics",
			err:       errors.Join(context.Canceled, error(&Failure{Code: "a", Err: (*Failure)(nil)})),
			wantCode:  "job_failed",
			wantPanic: true,
		},
		{
			name:      "panicking-error-method-panics",
			err:       panicOnError{},
			wantCode:  "job_failed",
			wantPanic: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, r := newBrokerAndRunner(t)
			id, sub := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
				return nil, tc.err
			})

			events := drain(t, sub, 1)
			if events[0].Name != wire.EventError {
				t.Fatalf("terminal event = %+v, want %q", events[0], wire.EventError)
			}
			payload, ok := events[0].Data.(wire.ErrorEvent)
			if !ok {
				t.Fatalf("terminal payload = %T, want wire.ErrorEvent", events[0].Data)
			}
			body := decodeEnvelope(t, payload.Error)
			if body.Code != tc.wantCode {
				t.Errorf("envelope code = %q, want %q", body.Code, tc.wantCode)
			}
			if tc.wantPanic {
				if !strings.Contains(body.Message, "job: panicked") {
					t.Errorf("envelope message = %q, want the panic named", body.Message)
				}
			} else if body.Message != tc.err.Error() {
				t.Errorf("envelope message = %q, want the error's own text %q", body.Message, tc.err.Error())
			}
			if tc.wantDetail == "" {
				if body.Detail != nil {
					t.Errorf("envelope detail = %s, want none", body.Detail)
				}
			} else if string(body.Detail) != tc.wantDetail {
				t.Errorf("envelope detail = %s, want %s", body.Detail, tc.wantDetail)
			}

			// The catch-up result carries what the stream carried, and the
			// cause survives in the chain for errors.Is.
			res, err := r.Result(context.Background(), id)
			if err != nil {
				t.Fatalf("Result: %v", err)
			}
			if res.Code != tc.wantCode {
				t.Errorf("Result.Code = %q, want %q", res.Code, tc.wantCode)
			}
			if tc.wantDetail == "" {
				if res.Detail != nil {
					t.Errorf("Result.Detail = %s, want none", res.Detail)
				}
			} else if string(res.Detail) != tc.wantDetail {
				t.Errorf("Result.Detail = %s, want %s", res.Detail, tc.wantDetail)
			}
			if tc.wantCause && !errors.Is(res.Err, cause) {
				t.Errorf("Result.Err = %v, want the cause still in the chain", res.Err)
			}
			if tc.wantPanic {
				if res.Status != StatusError {
					t.Errorf("Result.Status = %q, want %q", res.Status, StatusError)
				}
				if !errors.Is(res.Err, ErrPanic) {
					t.Errorf("Result.Err = %v, want errors.Is(.., ErrPanic)", res.Err)
				}

				// The runner survives every shape: the next job runs
				// normally.
				_, sub2 := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
					return []byte("ok"), nil
				})
				if events := drain(t, sub2, 1); events[0].Name != wire.EventDone {
					t.Errorf("next job terminal = %q, want %q", events[0].Name, wire.EventDone)
				}
			}
		})
	}
}

// panicOnError is an error whose own Error method panics, with no pointer
// anywhere in its chain. A work function may return it, and the terminal path
// may not end the process over it.
type panicOnError struct{}

// Error panics whatever it is asked.
func (panicOnError) Error() string { panic("panicOnError: Error panicked") }

// selfPoison panics with itself whenever its message is read, so every
// further attempt to render it panics too. fmt tolerates one nested panic and
// no more, so a render of this value without its own boundary ends the
// process.
type selfPoison struct{}

// Error panics with the value itself.
func (selfPoison) Error() string { panic(selfPoison{}) }

// poisonLadder panics with its next rung when its message is read, and the
// last rung panics with a string. A render has to step past two panics before
// it can print anything.
type poisonLadder struct{ rung int }

// Error panics with the next rung, or with a string at the last one.
func (e poisonLadder) Error() string {
	if e.rung >= 2 {
		panic("poisonLadder: the last rung panicked with a string")
	}
	panic(poisonLadder{rung: e.rung + 1})
}

// TestPoisonedRenderBecomesErrorTerminal pins the render policy: a value whose
// own render poisons reaches an error terminal, whether a work function
// returned it or panicked with it. The process keeps serving, and the message
// names the panic. Without the boundary around the render, the second panic
// escapes, the goroutine dies, and this test ends the test process.
func TestPoisonedRenderBecomesErrorTerminal(t *testing.T) {
	cases := []struct {
		name string
		fn   Func
	}{
		{
			name: "returned-self-poison",
			fn: func(context.Context, func(Progress)) ([]byte, error) {
				return nil, selfPoison{}
			},
		},
		{
			name: "returned-poison-ladder",
			fn: func(context.Context, func(Progress)) ([]byte, error) {
				return nil, poisonLadder{}
			},
		},
		{
			name: "panicked-self-poison",
			fn: func(context.Context, func(Progress)) ([]byte, error) {
				panic(selfPoison{})
			},
		},
		{
			name: "panicked-poison-ladder",
			fn: func(context.Context, func(Progress)) ([]byte, error) {
				panic(poisonLadder{})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, r := newBrokerAndRunner(t)
			id, sub := startGated(t, b, r, tc.fn)

			events := drain(t, sub, 1)
			if events[0].Name != wire.EventError {
				t.Fatalf("terminal event = %+v, want %q", events[0], wire.EventError)
			}
			payload, ok := events[0].Data.(wire.ErrorEvent)
			if !ok {
				t.Fatalf("terminal payload = %T, want wire.ErrorEvent", events[0].Data)
			}
			body := decodeEnvelope(t, payload.Error)
			if body.Code != "job_failed" {
				t.Errorf("envelope code = %q, want the fallback", body.Code)
			}
			if !strings.Contains(body.Message, "job: panicked") {
				t.Errorf("envelope message = %q, want the panic named", body.Message)
			}

			res, err := r.Result(context.Background(), id)
			if err != nil {
				t.Fatalf("Result: %v", err)
			}
			if res.Status != StatusError {
				t.Errorf("Result.Status = %q, want %q", res.Status, StatusError)
			}
			if !errors.Is(res.Err, ErrPanic) {
				t.Errorf("Result.Err = %v, want errors.Is(.., ErrPanic)", res.Err)
			}

			// The runner survives every shape: the next job runs normally.
			_, sub2 := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
				return []byte("ok"), nil
			})
			if events := drain(t, sub2, 1); events[0].Name != wire.EventDone {
				t.Errorf("next job terminal = %q, want %q", events[0].Name, wire.EventDone)
			}
		})
	}
}

// onceError answers its first Error call and panics on every later one, so a
// second read of the same value ends the process. It counts the calls.
type onceError struct{ calls atomic.Int32 }

// Error returns the message on the first call and panics afterwards.
func (e *onceError) Error() string {
	if e.calls.Add(1) > 1 {
		panic("onceError: Error called twice")
	}
	return "read once"
}

// TestTerminalReadsTheWorkErrorOnce pins the read policy: the Runner reads the
// message of the error a work function returned inside its terminal boundary
// and records that message, so the store and the stream never call the
// caller's own Error method again. The harness error ends the process if they
// do, and the Result keeps the value itself for errors.Is.
func TestTerminalReadsTheWorkErrorOnce(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	once := &onceError{}

	id, sub := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		return nil, once
	})
	events := drain(t, sub, 1)
	if events[0].Name != wire.EventError {
		t.Fatalf("terminal event = %+v, want %q", events[0], wire.EventError)
	}
	payload, ok := events[0].Data.(wire.ErrorEvent)
	if !ok {
		t.Fatalf("terminal payload = %T, want wire.ErrorEvent", events[0].Data)
	}
	if got := decodeEnvelope(t, payload.Error).Message; got != "read once" {
		t.Errorf("envelope message = %q, want the message the Runner read", got)
	}

	res, err := r.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := once.calls.Load(); got != 1 {
		t.Errorf("Error calls = %d, want exactly the one read", got)
	}
	if res.Err == nil || res.Err.Error() != "read once" {
		t.Errorf("Result.Err = %v, want the message the Runner read", res.Err)
	}
	if !errors.Is(res.Err, once) {
		t.Errorf("Result.Err = %v, want the work function's own error in the chain", res.Err)
	}
}

// TestFailureMethods pins the error surface of Failure itself: Error returns
// the cause's message when there is one and the code otherwise, and Unwrap
// is nil-safe.
func TestFailureMethods(t *testing.T) {
	cause := errors.New("disk full")
	bare := Failure{Code: "provider_fault"}
	if got, want := bare.Error(), "job: provider_fault"; got != want {
		t.Errorf("bare Error = %q, want %q", got, want)
	}
	if got := bare.Unwrap(); got != nil {
		t.Errorf("bare Unwrap = %v, want nil", got)
	}
	wrapped := Failure{Code: "provider_fault", Err: cause}
	if got, want := wrapped.Error(), cause.Error(); got != want {
		t.Errorf("wrapped Error = %q, want %q", got, want)
	}
	if !errors.Is(wrapped, cause) {
		t.Errorf("wrapped Unwrap lost the cause")
	}
}
