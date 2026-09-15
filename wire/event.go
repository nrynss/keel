package wire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Event names for a job stream. The name travels in the event field of one
// frame. Each terminal name also serves as the status member of its payload.
const (
	// EventProgress reports work in flight and is never terminal.
	EventProgress = "progress"
	// EventDone reports a job that finished on its own.
	EventDone = "done"
	// EventError reports a job that failed.
	EventError = "error"
	// EventCancelled reports a job that ended because its context was
	// cancelled.
	EventCancelled = "cancelled"
	// EventInterrupted reports a job that a restart ended.
	EventInterrupted = "interrupted"
)

// heartbeat is the comment line a stream sends after a quiet period. It keeps
// an intermediary from reaping a connection that carries no traffic.
const heartbeat = ": ping\n\n"

// SetEventHeaders sets the response headers every server-sent event stream
// carries. Content-Type names the stream. Cache-Control no-store stops a cache
// from keeping a copy. X-Accel-Buffering no stops a reverse proxy from holding
// frames back.
func SetEventHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
}

// WriteEvent writes one server-sent event frame to w. The frame carries the
// event name, the id, exactly one data line of compact JSON, and a blank line
// that ends the frame.
//
// data is any value that json.Marshal encodes, normally one of the payload
// structs in this file. json.Marshal emits compact JSON, which holds no
// newline, so the frame always carries exactly one data line.
//
// The caller flushes w after each frame and drops a duplicate on job_id. A
// frame that fails to encode or to write comes back as the returned error.
//
// The signature takes no request context. A response write belongs to the
// handler's own request, and a second context would add nothing to it.
func WriteEvent(w http.ResponseWriter, name string, id uint64, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(oneLine(name))
	b.WriteByte('\n')
	b.WriteString("id: ")
	b.WriteString(strconv.FormatUint(id, 10))
	b.WriteByte('\n')
	b.WriteString("data: ")
	b.Write(payload)
	b.WriteString("\n\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write event frame: %w", err)
	}
	return nil
}

// WriteHeartbeat writes the heartbeat frame, a comment line and a blank line.
// A stream sends it after a quiet period so an intermediary does not reap the
// connection. A client ignores a comment frame.
func WriteHeartbeat(w http.ResponseWriter) error {
	if _, err := io.WriteString(w, heartbeat); err != nil {
		return fmt.Errorf("write heartbeat frame: %w", err)
	}
	return nil
}

// ProgressEvent is the payload of a progress event. It reports work in flight
// and is never terminal. Stage names the step, and Current and Total are
// optional. A nil Current or Total leaves the member out. Detail is optional
// raw JSON that the app alone reads.
type ProgressEvent struct {
	JobID   string          `json:"job_id"`
	Stage   string          `json:"stage"`
	Current *int64          `json:"current,omitempty"`
	Total   *int64          `json:"total,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// StatusEvent is the payload of a done, cancelled or interrupted event. Each
// of those events is terminal and carries no other member. Status repeats the
// event name.
type StatusEvent struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// ErrorEvent is the payload of an error event. Error carries the envelope body
// that WriteError also writes, so a reader feeds the member to the one parser
// it already uses for a failed response. Build it with NewErrorEvent.
type ErrorEvent struct {
	JobID  string          `json:"job_id"`
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
}

// NewErrorEvent builds the payload of an error event. code, message and detail
// fill the same envelope body that WriteError writes for a failed response, so
// the event and a failed request carry one parser. A detail that cannot encode
// comes back as the returned error.
func NewErrorEvent(jobID, code, message string, detail any) (ErrorEvent, error) {
	body := errorBody{Code: code, Message: message}
	if detail != nil {
		raw, err := json.Marshal(detail)
		switch {
		case err != nil:
			return ErrorEvent{}, fmt.Errorf("encode error event detail: %w", err)
		case string(raw) == "null":
			// A detail that encodes to null counts as absent.
		default:
			body.Detail = raw
		}
	}
	envelope, err := json.Marshal(errorEnvelope{Error: body})
	if err != nil {
		return ErrorEvent{}, fmt.Errorf("encode error event envelope: %w", err)
	}
	return ErrorEvent{JobID: jobID, Status: EventError, Error: envelope}, nil
}

// Frame is one server-sent event frame read from a stream. A comment frame,
// such as the heartbeat, carries Comment and no event, id or data. An event
// frame carries Event, ID and Data. Data holds the compact JSON of the frame's
// data lines, joined with a newline.
type Frame struct {
	Comment string
	Event   string
	ID      uint64
	Data    json.RawMessage
}

// ReadEvent reads one frame from br. It returns io.EOF at the end of the
// stream. A malformed id or an unreadable frame comes back as the returned
// error.
//
// A blank line ends a frame. Every data line joins with a newline, so a payload
// that a producer split over several data lines reads back whole.
//
// A client that reads a job stream follows one order. It subscribes first and
// waits for the stream to open. It then fetches the job's current state and
// deduplicates the terminal event on job_id. Catch-up reads that state, so a
// frame id never promises a replay of progress. An id only lets the client
// drop a duplicate, and ids increase within one topic.
func ReadEvent(br *bufio.Reader) (Frame, error) {
	var (
		frame    Frame
		data     strings.Builder
		sawField bool
	)
read:
	for {
		line, err := br.ReadString('\n')
		atEOF := errors.Is(err, io.EOF)
		if err != nil && !atEOF {
			return Frame{}, fmt.Errorf("read event frame: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !sawField {
				if atEOF {
					return Frame{}, io.EOF
				}
				continue
			}
			break read
		}
		sawField = true
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "":
			frame.Comment = value
		case "event":
			frame.Event = value
		case "id":
			id, convErr := strconv.ParseUint(value, 10, 64)
			if convErr != nil {
				return Frame{}, fmt.Errorf("parse event id %q: %w", value, convErr)
			}
			frame.ID = id
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
		if atEOF {
			break read
		}
	}
	if data.Len() > 0 {
		frame.Data = json.RawMessage(strings.TrimSuffix(data.String(), "\n"))
	}
	return frame, nil
}

// oneLine flattens every newline spelling in s to a space. An event name frames
// as exactly one field line, so an embedded newline must never reach the wire.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", " ")
}
