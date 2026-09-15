package stream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/wire"
)

// readUntil reads from body until pred holds of the accumulated bytes,
// returning what was read. It fails the test if the stream ends or the
// timeout fires first. Predicates look at content, which event in what
// order, never at event counts.
func readUntil(t *testing.T, body io.Reader, pred func(string) bool, timeout time.Duration) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 256)
		for !pred(sb.String()) {
			k, err := body.Read(buf)
			sb.Write(buf[:k])
			if err != nil {
				got <- sb.String()
				return
			}
		}
		got <- sb.String()
	}()
	select {
	case s := <-got:
		if !pred(s) {
			t.Fatalf("stream ended before the expected traffic: %q", s)
		}
		return s
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for the expected traffic")
		return ""
	}
}

// readN reads until n blank-line-terminated frames have been seen.
func readN(t *testing.T, body io.Reader, n int, timeout time.Duration) string {
	t.Helper()
	return readUntil(t, body, func(s string) bool {
		return strings.Count(s, "\n\n") >= n
	}, timeout)
}

// serve spins up a real HTTP server streaming topic from b and returns the
// server and the response. The caller owns the server's Close and the
// response body's Close.
func serve(t *testing.T, b *Broker, topic string, clientTimeout time.Duration) (*httptest.Server, *http.Response) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.ServeTopic(w, r, topic)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	client := &http.Client{Timeout: clientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return srv, resp
}

// readGolden returns the bytes of one frame file under testdata/wire.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "wire", name))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	return raw
}

// TestServeTopicHeadersAndFraming pins the SSE surface: the three stream
// headers, a 200, and the exact wire framing, an event field, an id that
// increases per topic, one compact JSON data line, and a blank-line
// terminator.
func TestServeTopicHeadersAndFraming(t *testing.T) {
	b := New(Config{Heartbeat: time.Hour}) // no heartbeat noise in the framing assertions
	_, resp := serve(t, b, "frame", 5*time.Second)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	b.Publish("frame", Event{Name: "progress", Data: wire.ProgressEvent{JobID: "j", Stage: "page 1"}})
	if got, want := readN(t, resp.Body, 1, 5*time.Second), "event: progress\nid: 1\ndata: {\"job_id\":\"j\",\"stage\":\"page 1\"}\n\n"; got != want {
		t.Errorf("framed event = %q, want %q", got, want)
	}

	b.Publish("frame", Event{Name: "progress", Data: wire.ProgressEvent{JobID: "j", Stage: "page 2"}})
	if got, want := readN(t, resp.Body, 1, 5*time.Second), "event: progress\nid: 2\ndata: {\"job_id\":\"j\",\"stage\":\"page 2\"}\n\n"; got != want {
		t.Errorf("framed second event = %q, want %q (id increases per topic)", got, want)
	}
}

// TestServeTopicEmptyNameFramesEmptyEventField: a frame always carries an
// event field, so an Event with no Name frames it empty.
func TestServeTopicEmptyNameFramesEmptyEventField(t *testing.T) {
	b := New(Config{Heartbeat: time.Hour})
	_, resp := serve(t, b, "anon", 5*time.Second)
	defer resp.Body.Close()

	b.Publish("anon", Event{Data: "x"})
	if got, want := readN(t, resp.Body, 1, 5*time.Second), "event: \nid: 1\ndata: \"x\"\n\n"; got != want {
		t.Errorf("framed event = %q, want %q", got, want)
	}
}

// TestServeTopicHeartbeatBetweenEvents is the quiet-period probe: pings
// precede the event and pings resume after it. The interval is injected,
// and the default itself is pinned on the wire by
// TestDefaultHeartbeatReachesTheWire.
//
// Deterministic by margins, not luck: the event is published at 60ms, so
// the 25ms and 50ms ticks precede it and the next tick lands one interval
// after the event, because a real event resets the heartbeat. One read
// accumulates the stream from connect and one predicate asserts the
// ping-before, event, ping-after sequence.
func TestServeTopicHeartbeatBetweenEvents(t *testing.T) {
	b := New(Config{Heartbeat: 25 * time.Millisecond})
	_, resp := serve(t, b, "hb", 5*time.Second)
	defer resp.Body.Close()

	go func() {
		time.Sleep(60 * time.Millisecond)
		b.Publish("hb", Event{Name: "progress", Data: "done"})
	}()

	readUntil(t, resp.Body, func(s string) bool {
		i := strings.Index(s, "event: progress")
		if i < 0 {
			return false
		}
		return strings.Contains(s[:i], ": ping") && strings.Contains(s[i:], ": ping")
	}, 5*time.Second)
}

// TestDefaultHeartbeatReachesTheWire pins the 15s default through its
// default path: a Broker built with Config{} and no injected interval must
// put the first ping on the wire at the default cadence. It costs ~15s of
// wall clock, and that is the price of asserting the default at the wire.
func TestDefaultHeartbeatReachesTheWire(t *testing.T) {
	b := New(Config{})
	start := time.Now()
	_, resp := serve(t, b, "default-hb", 30*time.Second)
	defer resp.Body.Close()

	got := readN(t, resp.Body, 1, 25*time.Second)
	elapsed := time.Since(start)
	if !strings.Contains(got, ": ping") {
		t.Fatalf("first traffic = %q, want a : ping", got)
	}
	if elapsed < 14*time.Second || elapsed > 22*time.Second {
		t.Errorf("first ping after %v, want ~15s (the defaultHeartbeat, on the wire)", elapsed)
	}
}

// TestServeTopicClientDisconnectRemovesSubscriber: when the client goes
// away the request context ends and the subscriber is removed, observed
// through the real HTTP surface.
func TestServeTopicClientDisconnectRemovesSubscriber(t *testing.T) {
	b := New(Config{Heartbeat: time.Hour})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.ServeTopic(w, r, "disc")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// resp arriving proves the handler subscribed and flushed its
	// headers. There are no body bytes yet, so reading one would block.
	// Close straight away: the server must drop the dead subscriber.
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for b.subscribers("disc") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber still registered 2s after the client disconnected")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestServeTopicEventNameNewlineSanitized: the event Name gets the same
// newline hardening the frame writer applies, so a newline inside a name
// can never inject a field line into the frame.
func TestServeTopicEventNameNewlineSanitized(t *testing.T) {
	b := New(Config{Heartbeat: time.Hour})
	_, resp := serve(t, b, "nameframe", 5*time.Second)
	defer resp.Body.Close()

	b.Publish("nameframe", Event{Name: "done\ndata: injected", Data: "real"})
	if got, want := readN(t, resp.Body, 1, 5*time.Second), "event: done data: injected\nid: 1\ndata: \"real\"\n\n"; got != want {
		t.Errorf("framed event = %q, want %q (one field line, Name sanitized)", got, want)
	}

	// CRLF and bare CR flatten the same way.
	b.Publish("nameframe", Event{Name: "a\r\nb", Data: "x"})
	if got, want := readN(t, resp.Body, 1, 5*time.Second), "event: a b\nid: 2\ndata: \"x\"\n\n"; got != want {
		t.Errorf("framed CR event = %q, want %q", got, want)
	}
}

// TestServeTopicFramesMatchGolden drives a real connection with the event
// name and payload each job-shaped golden carries, then compares the
// captured frame with the golden byte for byte. A fresh topic numbers from
// its first event, so the test publishes filler events to reach the id the
// golden holds.
func TestServeTopicFramesMatchGolden(t *testing.T) {
	const jobID = "3f9a1c7e5b2d8046a1c3e5f7092b4d68"
	int64Ptr := func(v int64) *int64 { return &v }
	tests := []struct {
		name   string
		golden string
		event  string
		id     uint64
		build  func(t *testing.T) any
	}{
		{
			name:   "progress",
			golden: "event-progress.txt",
			event:  wire.EventProgress,
			id:     3,
			build: func(*testing.T) any {
				return wire.ProgressEvent{
					JobID:   jobID,
					Stage:   "transcoding",
					Current: int64Ptr(4),
					Total:   int64Ptr(12),
					Detail:  json.RawMessage(`{"pass":"loudness"}`),
				}
			},
		},
		{
			name:   "done",
			golden: "event-done.txt",
			event:  wire.EventDone,
			id:     4,
			build: func(*testing.T) any {
				return wire.StatusEvent{JobID: jobID, Status: wire.EventDone}
			},
		},
		{
			name:   "error",
			golden: "event-error.txt",
			event:  wire.EventError,
			id:     5,
			build: func(t *testing.T) any {
				event, err := wire.NewErrorEvent(jobID, "upstream_failed", "The media service failed.", errorAttempts{Attempts: 3})
				if err != nil {
					t.Fatalf("build error event: %v", err)
				}
				return event
			},
		},
		{
			name:   "cancelled",
			golden: "event-cancelled.txt",
			event:  wire.EventCancelled,
			id:     2,
			build: func(*testing.T) any {
				return wire.StatusEvent{JobID: jobID, Status: wire.EventCancelled}
			},
		},
		{
			name:   "interrupted",
			golden: "event-interrupted.txt",
			event:  wire.EventInterrupted,
			id:     7,
			build: func(*testing.T) any {
				return wire.StatusEvent{JobID: jobID, Status: wire.EventInterrupted}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topic := "golden-" + tt.name
			b := New(Config{Heartbeat: time.Hour})
			_, resp := serve(t, b, topic, 5*time.Second)
			defer resp.Body.Close()

			for i := uint64(1); i < tt.id; i++ {
				b.Publish(topic, Event{Name: wire.EventProgress, Data: wire.ProgressEvent{JobID: jobID, Stage: "waiting"}})
			}
			b.Publish(topic, Event{Name: tt.event, Data: tt.build(t), Terminal: true})

			got := readN(t, resp.Body, int(tt.id), 5*time.Second)
			frames := strings.Split(got, "\n\n")
			if len(frames) < int(tt.id) {
				t.Fatalf("captured %d frames, want %d", len(frames), tt.id)
			}
			frame := frames[tt.id-1] + "\n\n"
			want := string(readGolden(t, tt.golden))
			if frame != want {
				t.Fatalf("frame does not match %s\n got: %q\nwant: %q", tt.golden, frame, want)
			}
		})
	}
}

// TestServeTopicHeartbeatMatchesGolden drives a real connection and
// compares the first captured frame with the heartbeat golden byte for
// byte.
func TestServeTopicHeartbeatMatchesGolden(t *testing.T) {
	b := New(Config{Heartbeat: 20 * time.Millisecond})
	_, resp := serve(t, b, "golden-heartbeat", 5*time.Second)
	defer resp.Body.Close()

	got := readN(t, resp.Body, 1, 5*time.Second)
	frame := strings.SplitN(got, "\n\n", 2)[0] + "\n\n"
	want := string(readGolden(t, "event-heartbeat.txt"))
	if frame != want {
		t.Fatalf("heartbeat does not match golden\n got: %q\nwant: %q", frame, want)
	}
}

// errorAttempts is the app detail the error golden carries.
type errorAttempts struct {
	Attempts int `json:"attempts"`
}
