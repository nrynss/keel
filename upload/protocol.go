package upload

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/wire"
)

// chunkDigestHeader carries the SHA-256 of the chunk in a PUT request.
const chunkDigestHeader = "X-Chunk-SHA256"

// maxRequestBytes bounds a JSON request body.
const maxRequestBytes = 64 << 10

// Codes carried in the shared error envelope.
const (
	codeInvalidRequest  = "invalid_request"
	codeNotFound        = "not_found"
	codeChunkMismatch   = "chunk_mismatch"
	codeHashMismatch    = "hash_mismatch"
	codeIncomplete      = "incomplete"
	codeLimitExceeded   = "limit_exceeded"
	codeUnsupportedType = "unsupported_type"
	codeConflict        = "conflict"
	codeStorageError    = "storage_error"
)

// limitUpload and limitOwner name the two byte limits a request can exceed,
// as they appear in the error envelope.
const (
	limitUpload = "upload"
	limitOwner  = "owner"
)

// startRequest is the POST body that opens an upload. SizeBytes is a
// pointer so an absent size is told apart from a declared zero.
type startRequest struct {
	Owner       string `json:"owner"`
	ContentType string `json:"content_type"`
	Group       string `json:"group,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
	SizeBytes   *int64 `json:"size_bytes,omitempty"`
	ChunkSize   int64  `json:"chunk_size,omitempty"`
}

// completeRequest is the POST body that finishes an upload.
type completeRequest struct {
	SHA256 string `json:"sha256"`
}

// stateResponse is the state of an upload, answered to the request that
// opened it and to every request that advances it.
type stateResponse struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	ContentType string `json:"content_type"`
	Visibility  string `json:"visibility"`
	ChunkSize   int64  `json:"chunk_size"`
	SizeBytes   *int64 `json:"size_bytes,omitempty"`
	ChunkCount  *int   `json:"chunk_count,omitempty"`
	StoredBytes int64  `json:"stored_bytes"`
	Received    []int  `json:"received"`
	Missing     []int  `json:"missing"`
	ExpiresAt   string `json:"expires_at"`
}

// completeResponse is the answer to a finished upload: the id the bytes
// are stored under and the digest that was verified for them.
type completeResponse struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	ContentType string `json:"content_type"`
	Visibility  string `json:"visibility"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
}

// mismatchDetail names the digest a chunk declared and the digest its
// bytes produced.
type mismatchDetail struct {
	Index    int64  `json:"index"`
	Declared string `json:"declared"`
	Actual   string `json:"actual"`
}

// digestDetail names the digest declared for a whole upload and the digest
// its assembled chunks produced.
type digestDetail struct {
	Declared string `json:"declared"`
	Actual   string `json:"actual"`
}

// missingDetail names the chunk indices an upload still needs.
type missingDetail struct {
	Missing []int `json:"missing"`
}

// limitDetail names the byte limit that refused a request, the size of the
// limit, and the total the request would have brought the upload or the
// owner to.
type limitDetail struct {
	Limit string `json:"limit"`
	Max   int64  `json:"max_bytes"`
	Total int64  `json:"total_bytes"`
}

// fieldDetail names the request field that was refused.
type fieldDetail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// apiError is a failure together with the envelope it becomes.
type apiError struct {
	status  int
	code    string
	message string
	detail  any
}

// Error renders the failure as one line for a log.
func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// badRequest reports a request field the handler will not accept.
func badRequest(field, reason string) *apiError {
	return &apiError{
		status:  http.StatusBadRequest,
		code:    codeInvalidRequest,
		message: "The request is not valid.",
		detail:  fieldDetail{Field: field, Reason: reason},
	}
}

// noSuchUpload reports an upload that is unknown or has expired.
func noSuchUpload() *apiError {
	return &apiError{
		status:  http.StatusNotFound,
		code:    codeNotFound,
		message: "No such upload exists.",
	}
}

// mismatch reports a chunk whose bytes do not match its declared digest.
func mismatch(index int64, declared, actual string) *apiError {
	return &apiError{
		status:  http.StatusConflict,
		code:    codeChunkMismatch,
		message: "The chunk does not match the digest declared for it.",
		detail:  mismatchDetail{Index: index, Declared: declared, Actual: actual},
	}
}

// incomplete reports an upload that is missing chunks.
func incomplete(missing []int) *apiError {
	return &apiError{
		status:  http.StatusConflict,
		code:    codeIncomplete,
		message: "The upload is missing chunks.",
		detail:  missingDetail{Missing: missing},
	}
}

// overLimit reports an upload or an owner refused for its size, naming the
// limit that refused it.
func overLimit(limit string, max, total int64) *apiError {
	message := "The bytes exceed the byte limit for this upload."
	if limit == limitOwner {
		message = "The bytes exceed the byte limit for this owner."
	}
	return &apiError{
		status:  http.StatusRequestEntityTooLarge,
		code:    codeLimitExceeded,
		message: message,
		detail:  limitDetail{Limit: limit, Max: max, Total: total},
	}
}

// storeFailure maps a BlobStore failure onto the envelope.
func storeFailure(err error) error {
	switch {
	case errors.Is(err, mediastore.ErrInvalidContentType):
		return &apiError{
			status:  http.StatusUnsupportedMediaType,
			code:    codeUnsupportedType,
			message: "The content type is not accepted.",
			detail:  fieldDetail{Field: "content_type", Reason: "not an accepted media type"},
		}
	case errors.Is(err, mediastore.ErrNotFound):
		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
			message: "The request is not valid.",
			detail:  fieldDetail{Field: "id", Reason: "not a storable media id"},
		}
	case errors.Is(err, mediastore.ErrAlreadyExists):
		return &apiError{
			status:  http.StatusConflict,
			code:    codeConflict,
			message: "The upload id already names stored bytes.",
		}
	}
	return fmt.Errorf("upload: persist assembled upload: %w", err)
}

// decodeBody reads a bounded JSON request body into v.
func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes)).Decode(v); err != nil {
		return badRequest("body", "must be a JSON object")
	}
	return nil
}

// validDigest reports whether s is a SHA-256 in hex.
func validDigest(s string) bool {
	if len(s) != sha256HexSize {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// writeJSON answers with v as JSON, in the framing wire.WriteError uses.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.log.Error("upload: response not encoded", "err", err.Error())
		_ = wire.WriteError(w, http.StatusInternalServerError, codeStorageError, "The upload could not be answered.", nil) // The response has already failed, so there is no second envelope to send.
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		h.log.Debug("upload: response not written", "err", err.Error())
	}
}

// fail answers with err as an error envelope, logging a failure the client
// did not cause.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	var api *apiError
	if !errors.As(err, &api) {
		h.log.Error("upload: request failed", "err", err.Error())
		api = &apiError{
			status:  http.StatusInternalServerError,
			code:    codeStorageError,
			message: "The upload could not be advanced.",
		}
	}
	if werr := wire.WriteError(w, api.status, api.code, api.message, api.detail); werr != nil {
		h.log.Error("upload: error envelope not written", "err", werr.Error())
	}
}
