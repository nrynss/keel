// Package wire holds the HTTP shapes that every Keel handler and every
// client share. Package wire pins one error envelope for every non-2xx
// JSON response. A client branches on the envelope code and never on the
// message, so a message can change wording without a client release.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
)

// errorEnvelope is the JSON object that every non-2xx JSON response carries.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody is the envelope payload. Detail stays raw, so an absent or
// dropped detail never forces a second pass over the rest of the body.
type errorBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// retrySeconds reads the retry hint out of an encoded detail object and
// returns it in whole seconds, at least 1. The hint is the
// retry_after_seconds member. A fraction rounds up, so the header never
// promises a shorter wait than the detail reports. A detail without a
// usable hint yields the 1 second floor.
func retrySeconds(detail json.RawMessage) int {
	var hint struct {
		RetryAfter float64 `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(detail, &hint); err != nil {
		return 1
	}
	seconds := int(math.Ceil(hint.RetryAfter))
	if seconds < 1 {
		return 1
	}
	return seconds
}

// WriteError writes the error envelope that every non-2xx JSON response
// carries. The body is one JSON object of the shape
// {"error":{"code","message","detail"}} followed by a newline.
//
// code is the stable snake_case identifier that a client branches on.
// message is a human sentence that a client may show but never branches
// on. detail is optional and travels as the detail member.
//
// WriteError sets Content-Type application/json and Cache-Control
// no-store on every response. A 429 also sets Retry-After in whole
// seconds, at least 1. The header takes the retry_after_seconds member of
// detail, rounds a fraction up, and falls back to 1 when detail carries
// no usable hint, so header and body never disagree.
//
// A detail that fails to encode, or one that encodes to null, leaves the
// body without the detail member. A failed detail encoding comes back as
// the returned error, and so does a failed write. The status goes out in
// every case, so the client always sees one envelope.
//
// The signature takes no request context. A response write belongs to the
// handler's own request, and a second context would add nothing to it.
func WriteError(w http.ResponseWriter, status int, code, message string, detail any) error {
	body := errorBody{Code: code, Message: message}
	var detailErr error
	if detail != nil {
		raw, err := json.Marshal(detail)
		switch {
		case err != nil:
			detailErr = fmt.Errorf("encode error detail: %w", err)
		case string(raw) == "null":
			// A detail that encodes to null counts as absent.
		default:
			body.Detail = raw
		}
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(body.Detail)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	payload, err := json.Marshal(errorEnvelope{Error: body})
	if err != nil {
		return fmt.Errorf("encode error envelope: %w", err)
	}
	payload = append(payload, '\n')
	w.WriteHeader(status)
	if _, err := w.Write(payload); err != nil {
		if detailErr != nil {
			return errors.Join(err, detailErr)
		}
		return err
	}
	return detailErr
}
