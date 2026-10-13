package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// terminal carries, and a wrapped Failure is still found. A blank code falls
// back to job_failed while its detail still travels. A chain with no Failure
// carries job_failed and no detail, which is what every error carried before
// Failure existed.
func TestFailureNamesTerminalCode(t *testing.T) {
	cause := errors.New("reserve: over budget")
	cases := []struct {
		name       string
		err        error
		wantCode   string
		wantDetail string
	}{
		{
			name:       "coded-failure",
			err:        Failure{Code: "budget_exhausted", Detail: map[string]int{"retry_after": 30}, Err: cause},
			wantCode:   "budget_exhausted",
			wantDetail: `{"retry_after":30}`,
		},
		{
			name:       "wrapped-failure",
			err:        fmt.Errorf("plan: %w", Failure{Code: "input_rejected", Detail: map[string]any{"field": "length"}, Err: cause}),
			wantCode:   "input_rejected",
			wantDetail: `{"field":"length"}`,
		},
		{
			name:     "failure-without-detail",
			err:      Failure{Code: "provider_fault", Err: cause},
			wantCode: "provider_fault",
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
		},
		{
			name:     "plain-error-falls-back",
			err:      cause,
			wantCode: "job_failed",
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
			if body.Message != tc.err.Error() {
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
			if !errors.Is(res.Err, cause) {
				t.Errorf("Result.Err = %v, want the cause still in the chain", res.Err)
			}
		})
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
