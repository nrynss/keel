package gate_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/nrynss/keel/gate"
)

// ExampleGate_Protect wraps a handler with a one-request burst, sends two
// requests from one client, and prints each status and the refusal code.
func ExampleGate_Protect() {
	g, err := gate.New(gate.Config{})
	if err != nil {
		fmt.Println("new gate failed:", err)
		return
	}

	handler, err := g.Protect(gate.Rule{
		Name:      "thumb",
		PerClient: gate.Limit{Burst: 1, Every: time.Hour},
		Global:    gate.Limit{Burst: 100, Every: time.Hour},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if err != nil {
		fmt.Println("protect failed:", err)
		return
	}

	for range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/thumb", nil)
		req.RemoteAddr = "203.0.113.7:34567"
		handler.ServeHTTP(rec, req)
		fmt.Println(rec.Code)
		if rec.Code == http.StatusTooManyRequests {
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				fmt.Println("decode failed:", err)
				return
			}
			fmt.Println(envelope.Error.Code)
		}
	}
	// Output:
	// 200
	// 429
	// rate_limited
}
