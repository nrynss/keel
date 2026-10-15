package revision_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nrynss/keel/revision"
)

// TestConflictErrorMatchesErrConflict pins that the typed conflict answers
// both errors.Is and errors.As through a wrap.
func TestConflictErrorMatchesErrConflict(t *testing.T) {
	err := fmt.Errorf("commit: %w", &revision.ConflictError{Document: "d", Parent: 1, Head: 3})
	if !errors.Is(err, revision.ErrConflict) {
		t.Fatalf("errors.Is(%v, ErrConflict) = false", err)
	}
	var c *revision.ConflictError
	if !errors.As(err, &c) || c.Head != 3 || c.Parent != 1 || c.Document != "d" {
		t.Fatalf("errors.As = %+v", c)
	}
	if errors.Is(err, revision.ErrInvalid) {
		t.Fatal("a conflict matched ErrInvalid")
	}
}

// TestWriteConflictUsesTheWireEnvelope pins the status, the stable code and
// the detail a browser client rebases from.
func TestWriteConflictUsesTheWireEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := revision.WriteConflict(rec, &revision.ConflictError{Document: "doc-1", Parent: 2, Head: 5}); err != nil {
		t.Fatalf("WriteConflict: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Error struct {
			Code   string `json:"code"`
			Detail struct {
				Document string `json:"document"`
				Parent   int64  `json:"parent"`
				Head     int64  `json:"head"`
			} `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if got.Error.Code != "revision_conflict" {
		t.Fatalf("code = %q", got.Error.Code)
	}
	if d := got.Error.Detail; d.Document != "doc-1" || d.Parent != 2 || d.Head != 5 {
		t.Fatalf("detail = %+v", d)
	}
}

// TestWriteConflictWithNoConflictAnswersInternal pins that a nil conflict
// cannot panic a handler.
func TestWriteConflictWithNoConflictAnswersInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := revision.WriteConflict(rec, nil); err != nil {
		t.Fatalf("WriteConflict(nil): %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}
