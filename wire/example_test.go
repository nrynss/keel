package wire_test

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/nrynss/keel/wire"
)

// ExampleWriteError writes a rate-limited refusal and prints the status, the
// retry hint and the envelope body.
func ExampleWriteError() {
	rec := httptest.NewRecorder()
	detail := struct {
		RetryAfterSeconds int `json:"retry_after_seconds"`
	}{RetryAfterSeconds: 3}

	if err := wire.WriteError(rec, http.StatusTooManyRequests, "rate_limited", "too many requests", detail); err != nil {
		fmt.Println("write failed:", err)
		return
	}

	fmt.Println(rec.Code)
	fmt.Println(rec.Header().Get("Retry-After"))
	fmt.Print(rec.Body.String())
	// Output:
	// 429
	// 3
	// {"error":{"code":"rate_limited","message":"too many requests","detail":{"retry_after_seconds":3}}}
}

// ExampleWriteEvent writes one progress frame and reads it back with ReadEvent.
func ExampleWriteEvent() {
	stream := httptest.NewRecorder()
	payload := wire.ProgressEvent{JobID: "abc", Stage: "encode"}
	if err := wire.WriteEvent(stream, wire.EventProgress, 7, payload); err != nil {
		fmt.Println("write failed:", err)
		return
	}

	frame, err := wire.ReadEvent(bufio.NewReader(stream.Body))
	if err != nil {
		fmt.Println("read failed:", err)
		return
	}

	fmt.Println(frame.Event, frame.ID)
	fmt.Println(string(frame.Data))
	// Output:
	// progress 7
	// {"job_id":"abc","stage":"encode"}
}
