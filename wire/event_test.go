package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testJobID is the job id every golden frame carries.
const testJobID = "3f9a1c7e5b2d8046a1c3e5f7092b4d68"

// progressDetail is the app detail the progress golden carries.
type progressDetail struct {
	Pass string `json:"pass"`
}

// errorDetail is the app detail the error golden carries.
type errorDetail struct {
	Attempts int `json:"attempts"`
}

// int64Ptr returns a pointer to v, for an optional progress member.
func int64Ptr(v int64) *int64 { return &v }

// errWrite is the failure the failWriter reports.
var errWrite = errors.New("write refused")

// failWriter accepts limit bytes and fails every later write. It keeps the
// partial bytes, so a failure lands mid-frame as it would on a closed
// connection.
type failWriter struct {
	header http.Header
	limit  int
	buf    bytes.Buffer
}

// Header returns the writer's header map.
func (w *failWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

// WriteHeader accepts a status and writes nothing.
func (w *failWriter) WriteHeader(int) {}

// Write accepts bytes up to the limit and fails after that limit.
func (w *failWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		return 0, errWrite
	}
	if room >= len(p) {
		w.buf.Write(p) // bytes.Buffer.Write never returns an error
		return len(p), nil
	}
	w.buf.Write(p[:room]) // bytes.Buffer.Write never returns an error
	return room, errWrite
}

// errRead is the failure the errReader reports.
var errRead = errors.New("read refused")

// errReader fails every read, standing in for a broken connection.
type errReader struct{}

// Read returns the read failure and no bytes.
func (errReader) Read([]byte) (int, error) { return 0, errRead }

// readGolden returns the bytes of one frame file under testdata/wire.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "wire", name))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	return raw
}

// decodeData reads a frame's data line into out.
func decodeData(t *testing.T, frame Frame, out any) {
	t.Helper()
	if err := json.Unmarshal(frame.Data, out); err != nil {
		t.Fatalf("decode event data: %v", err)
	}
}

func TestWriteEventGolden(t *testing.T) {
	tests := []struct {
		name    string
		golden  string
		event   string
		id      uint64
		payload func(t *testing.T) any
	}{
		{
			name:   "progress",
			golden: "event-progress.txt",
			event:  EventProgress,
			id:     3,
			payload: func(*testing.T) any {
				return ProgressEvent{
					JobID:   testJobID,
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
			event:  EventDone,
			id:     4,
			payload: func(*testing.T) any {
				return StatusEvent{JobID: testJobID, Status: EventDone}
			},
		},
		{
			name:   "error",
			golden: "event-error.txt",
			event:  EventError,
			id:     5,
			payload: func(t *testing.T) any {
				event, err := NewErrorEvent(testJobID, "upstream_failed", "The media service failed.", errorDetail{Attempts: 3})
				if err != nil {
					t.Fatalf("NewErrorEvent returned an error: %v", err)
				}
				return event
			},
		},
		{
			name:   "cancelled",
			golden: "event-cancelled.txt",
			event:  EventCancelled,
			id:     2,
			payload: func(*testing.T) any {
				return StatusEvent{JobID: testJobID, Status: EventCancelled}
			},
		},
		{
			name:   "interrupted",
			golden: "event-interrupted.txt",
			event:  EventInterrupted,
			id:     7,
			payload: func(*testing.T) any {
				return StatusEvent{JobID: testJobID, Status: EventInterrupted}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := WriteEvent(rec, tt.event, tt.id, tt.payload(t)); err != nil {
				t.Fatalf("WriteEvent returned an error: %v", err)
			}
			want := readGolden(t, tt.golden)
			if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("frame does not match %s\n got: %q\nwant: %q", tt.golden, got, want)
			}
		})
	}
}

func TestWriteHeartbeatGolden(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := WriteHeartbeat(rec); err != nil {
		t.Fatalf("WriteHeartbeat returned an error: %v", err)
	}
	want := readGolden(t, "event-heartbeat.txt")
	if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("heartbeat does not match golden\n got: %q\nwant: %q", got, want)
	}
}

func TestSetEventHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	SetEventHeaders(rec)
	want := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestReadEventGolden(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		assert func(t *testing.T, frame Frame)
	}{
		{
			name:   "progress",
			golden: "event-progress.txt",
			assert: func(t *testing.T, frame Frame) {
				if frame.Event != EventProgress || frame.ID != 3 {
					t.Fatalf("frame = %+v, want progress with id 3", frame)
				}
				var event ProgressEvent
				decodeData(t, frame, &event)
				if event.JobID != testJobID || event.Stage != "transcoding" {
					t.Errorf("event = %+v", event)
				}
				if event.Current == nil || *event.Current != 4 || event.Total == nil || *event.Total != 12 {
					t.Errorf("current/total = %v/%v, want 4/12", event.Current, event.Total)
				}
				var detail progressDetail
				if err := json.Unmarshal(event.Detail, &detail); err != nil {
					t.Fatalf("decode detail: %v", err)
				}
				if detail.Pass != "loudness" {
					t.Errorf("detail = %+v", detail)
				}
			},
		},
		{
			name:   "done",
			golden: "event-done.txt",
			assert: func(t *testing.T, frame Frame) {
				if frame.Event != EventDone || frame.ID != 4 {
					t.Fatalf("frame = %+v, want done with id 4", frame)
				}
				var event StatusEvent
				decodeData(t, frame, &event)
				if event.JobID != testJobID || event.Status != EventDone {
					t.Errorf("event = %+v", event)
				}
			},
		},
		{
			name:   "error",
			golden: "event-error.txt",
			assert: func(t *testing.T, frame Frame) {
				if frame.Event != EventError || frame.ID != 5 {
					t.Fatalf("frame = %+v, want error with id 5", frame)
				}
				var event ErrorEvent
				decodeData(t, frame, &event)
				if event.JobID != testJobID || event.Status != EventError {
					t.Errorf("event = %+v", event)
				}
				var envelope errorEnvelope
				if err := json.Unmarshal(event.Error, &envelope); err != nil {
					t.Fatalf("decode envelope: %v", err)
				}
				if envelope.Error.Code != "upstream_failed" || envelope.Error.Message != "The media service failed." {
					t.Errorf("envelope = %+v", envelope)
				}
				var detail errorDetail
				if err := json.Unmarshal(envelope.Error.Detail, &detail); err != nil {
					t.Fatalf("decode envelope detail: %v", err)
				}
				if detail.Attempts != 3 {
					t.Errorf("envelope detail = %+v", detail)
				}
			},
		},
		{
			name:   "cancelled",
			golden: "event-cancelled.txt",
			assert: func(t *testing.T, frame Frame) {
				if frame.Event != EventCancelled || frame.ID != 2 {
					t.Fatalf("frame = %+v, want cancelled with id 2", frame)
				}
				var event StatusEvent
				decodeData(t, frame, &event)
				if event.JobID != testJobID || event.Status != EventCancelled {
					t.Errorf("event = %+v", event)
				}
			},
		},
		{
			name:   "interrupted",
			golden: "event-interrupted.txt",
			assert: func(t *testing.T, frame Frame) {
				if frame.Event != EventInterrupted || frame.ID != 7 {
					t.Fatalf("frame = %+v, want interrupted with id 7", frame)
				}
				var event StatusEvent
				decodeData(t, frame, &event)
				if event.JobID != testJobID || event.Status != EventInterrupted {
					t.Errorf("event = %+v", event)
				}
			},
		},
		{
			name:   "heartbeat",
			golden: "event-heartbeat.txt",
			assert: func(t *testing.T, frame Frame) {
				if frame.Comment != "ping" {
					t.Errorf("comment = %q, want ping", frame.Comment)
				}
				if frame.Event != "" || frame.ID != 0 || frame.Data != nil {
					t.Errorf("heartbeat frame = %+v, want no event, id or data", frame)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(readGolden(t, tt.golden)))
			frame, err := ReadEvent(br)
			if err != nil {
				t.Fatalf("ReadEvent returned an error: %v", err)
			}
			tt.assert(t, frame)
			if _, err := ReadEvent(br); !errors.Is(err, io.EOF) {
				t.Fatalf("ReadEvent after the frame = %v, want io.EOF", err)
			}
		})
	}
}

func TestReadEventJoinsDataLines(t *testing.T) {
	raw := "event: progress\nid: 9\ndata: {\"a\":1\ndata: ,\"b\":2}\n\n"
	frame, err := ReadEvent(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("ReadEvent returned an error: %v", err)
	}
	if frame.Event != EventProgress || frame.ID != 9 {
		t.Fatalf("frame = %+v, want progress with id 9", frame)
	}
	want := "{\"a\":1\n,\"b\":2}"
	if got := string(frame.Data); got != want {
		t.Fatalf("joined data = %q, want %q", got, want)
	}
}

func TestReadEventEOF(t *testing.T) {
	if _, err := ReadEvent(bufio.NewReader(strings.NewReader(""))); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadEvent on an empty stream = %v, want io.EOF", err)
	}
}

func TestWriteEventReturnsWriteError(t *testing.T) {
	w := &failWriter{limit: 10}
	err := WriteEvent(w, EventDone, 4, StatusEvent{JobID: testJobID, Status: EventDone})
	if !errors.Is(err, errWrite) {
		t.Fatalf("WriteEvent error = %v, want it to wrap %v", err, errWrite)
	}
	if w.buf.Len() != 10 {
		t.Fatalf("wrote %d bytes before the failure, want the 10 byte limit", w.buf.Len())
	}
}

func TestWriteHeartbeatReturnsWriteError(t *testing.T) {
	w := &failWriter{limit: 3}
	err := WriteHeartbeat(w)
	if !errors.Is(err, errWrite) {
		t.Fatalf("WriteHeartbeat error = %v, want it to wrap %v", err, errWrite)
	}
	if got := w.buf.String(); got != ": p" {
		t.Fatalf("wrote %q before the failure, want %q", got, ": p")
	}
}

func TestWriteEventReturnsEncodeError(t *testing.T) {
	rec := httptest.NewRecorder()
	err := WriteEvent(rec, EventProgress, 3, make(chan int))
	var typeErr *json.UnsupportedTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("WriteEvent error = %v, want a json.UnsupportedTypeError", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote %q after an encode failure, want nothing", rec.Body.String())
	}
}

func TestNewErrorEventReturnsDetailError(t *testing.T) {
	_, err := NewErrorEvent(testJobID, "upstream_failed", "The media service failed.", make(chan int))
	var typeErr *json.UnsupportedTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("NewErrorEvent error = %v, want a json.UnsupportedTypeError", err)
	}
}

func TestReadEventReturnsReadError(t *testing.T) {
	_, err := ReadEvent(bufio.NewReader(errReader{}))
	if !errors.Is(err, errRead) {
		t.Fatalf("ReadEvent error = %v, want it to wrap %v", err, errRead)
	}
}

func TestReadEventReturnsMalformedIDError(t *testing.T) {
	raw := "event: done\nid: not-a-number\ndata: {}\n\n"
	_, err := ReadEvent(bufio.NewReader(strings.NewReader(raw)))
	if err == nil {
		t.Fatal("ReadEvent of a malformed id returned no error")
	}
}
