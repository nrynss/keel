package sqlitestore_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nrynss/keel/revision"
	"github.com/nrynss/keel/revision/sqlitestore"
)

// grow commits until doc's head is n.
func grow(t *testing.T, s *sqlitestore.Store, doc string, n int64) {
	t.Helper()
	for parent := int64(1); parent < n; parent++ {
		commit(t, s, doc, parent, `{}`)
	}
}

// numbers lists the revision numbers of a page.
func numbers(rs []revision.Revision) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.Number
	}
	return out
}

// TestHistoryPagesCoverEveryRevisionOnce walks the history at several page
// sizes, including one that divides the count exactly, and checks every
// number appears once, newest first, with a zero cursor on the last page.
func TestHistoryPagesCoverEveryRevisionOnce(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	grow(t, s, "doc", 12)
	for _, limit := range []int{1, 3, 5, 12, 13} {
		var seen []int64
		var before int64
		pages := 0
		for {
			page, next, err := s.History(t.Context(), "doc", revision.Page{Limit: limit, Before: before})
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			pages++
			if len(page) > limit || (next != 0 && len(page) != limit) {
				t.Fatalf("limit %d: page of %d with next %d", limit, len(page), next)
			}
			seen = append(seen, numbers(page)...)
			if next == 0 {
				break
			}
			before = next
		}
		if len(seen) != 12 {
			t.Fatalf("limit %d saw %v", limit, seen)
		}
		for i, n := range seen {
			if n != int64(12-i) {
				t.Fatalf("limit %d order %v", limit, seen)
			}
		}
		if want := (12 + limit - 1) / limit; pages != want {
			t.Fatalf("limit %d took %d pages, want %d", limit, pages, want)
		}
	}
}

// TestHistoryCursorBounds pins the edges: a cursor at or below 1 is an empty
// last page, one past the head reads from the top, and the default limit
// applies to a zero Limit.
func TestHistoryCursorBounds(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	grow(t, s, "doc", revision.DefaultLimit+5)
	page, next, err := s.History(t.Context(), "doc", revision.Page{})
	if err != nil || len(page) != revision.DefaultLimit || next != page[len(page)-1].Number {
		t.Fatalf("default page = %d rows, next %d, %v", len(page), next, err)
	}
	for _, before := range []int64{1} {
		page, next, err := s.History(t.Context(), "doc", revision.Page{Before: before})
		if err != nil || len(page) != 0 || next != 0 {
			t.Fatalf("before %d = %v, %d, %v", before, numbers(page), next, err)
		}
	}
	page, _, err = s.History(t.Context(), "doc", revision.Page{Limit: 2, Before: 1000})
	if err != nil || len(page) != 2 || page[0].Number != revision.DefaultLimit+5 {
		t.Fatalf("before past head = %v, %v", numbers(page), err)
	}
}

// TestPageLimitsAreValidated pins the refusals for every paged listing.
func TestPageLimitsAreValidated(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	bad := []revision.Page{{Limit: -1}, {Limit: revision.MaxLimit + 1}, {Before: -1}}
	for _, p := range bad {
		if _, _, err := s.History(t.Context(), "doc", p); !errors.Is(err, revision.ErrInvalid) {
			t.Errorf("History(%+v) = %v", p, err)
		}
		if _, _, err := s.Proposals(t.Context(), "doc", p); !errors.Is(err, revision.ErrInvalid) {
			t.Errorf("Proposals(%+v) = %v", p, err)
		}
	}
	for _, p := range []revision.OwnerPage{{Limit: -1}, {Limit: revision.MaxLimit + 1}} {
		if _, _, err := s.Documents(t.Context(), "o", p); !errors.Is(err, revision.ErrInvalid) {
			t.Errorf("Documents(%+v) = %v", p, err)
		}
	}
	if _, _, err := s.History(t.Context(), "doc", revision.Page{Limit: revision.MaxLimit}); err != nil {
		t.Fatalf("History at MaxLimit = %v", err)
	}
	if _, _, err := s.Documents(t.Context(), "", revision.OwnerPage{}); !errors.Is(err, revision.ErrInvalid) {
		t.Fatalf("Documents(empty owner) = %v", err)
	}
}

// TestHistoryPagingSurvivesCommitsAndPrunes pins keyset stability: a commit
// between pages adds nothing to later pages, and a prune between pages only
// removes rows, never repeats one.
func TestHistoryPagingSurvivesCommitsAndPrunes(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	grow(t, s, "doc", 10)
	first, next, err := s.History(t.Context(), "doc", revision.Page{Limit: 4})
	if err != nil || fmt.Sprint(numbers(first)) != "[10 9 8 7]" || next != 7 {
		t.Fatalf("first page = %v next %d, %v", numbers(first), next, err)
	}
	commit(t, s, "doc", 10, `{}`)
	if _, err := s.Pin(t.Context(), "doc", 2, "take"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := s.Prune(t.Context(), "doc", 6); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var rest []int64
	for next != 0 {
		page, n, err := s.History(t.Context(), "doc", revision.Page{Limit: 4, Before: next})
		if err != nil {
			t.Fatalf("next page: %v", err)
		}
		rest = append(rest, numbers(page)...)
		next = n
	}
	if fmt.Sprint(rest) != "[6 2]" {
		t.Fatalf("pages after the commit and the prune = %v, want [6 2]", rest)
	}
}

// TestDocumentsListOneOwnerInPages pins owner separation and id paging.
func TestDocumentsListOneOwnerInPages(t *testing.T) {
	s := openStore(t)
	for _, id := range []string{"d3", "d1", "d5", "d2", "d4"} {
		create(t, s, id, "alice")
	}
	create(t, s, "b1", "bob")
	create(t, s, "d0", "bob")
	var got []string
	after := ""
	for {
		page, next, err := s.Documents(t.Context(), "alice", revision.OwnerPage{Limit: 2, After: after})
		if err != nil {
			t.Fatalf("documents: %v", err)
		}
		for _, d := range page {
			if d.Owner != "alice" {
				t.Fatalf("another owner's document listed: %+v", d)
			}
			got = append(got, d.ID)
		}
		if next == "" {
			break
		}
		after = next
	}
	if strings.Join(got, ",") != "d1,d2,d3,d4,d5" {
		t.Fatalf("alice's documents = %v", got)
	}
	page, next, err := s.Documents(t.Context(), "nobody", revision.OwnerPage{})
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("unknown owner = %v, %q, %v", page, next, err)
	}
	if err := s.ForgetOwner(t.Context(), "alice"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if page, _, _ := s.Documents(t.Context(), "alice", revision.OwnerPage{}); len(page) != 0 {
		t.Fatalf("documents after forget = %v", page)
	}
}

// TestProposalSeqIsNeverReusedAndPages pins that Seq keeps rising after a
// prune drops the newest proposal, and that Proposals pages on it.
func TestProposalSeqIsNeverReusedAndPages(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	var ids []string
	for range 5 {
		p, err := s.Propose(ctx, "doc", 1, []byte(`{}`), "m")
		if err != nil {
			t.Fatalf("propose: %v", err)
		}
		ids = append(ids, p.ID)
		if p.Seq != int64(len(ids)) {
			t.Fatalf("seq = %d, want %d", p.Seq, len(ids))
		}
	}
	if _, err := s.Reject(ctx, ids[4], "ana"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	commit(t, s, "doc", 1, `{}`)
	if _, err := s.Prune(ctx, "doc", 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	p, err := s.Propose(ctx, "doc", 2, []byte(`{}`), "m")
	if err != nil || p.Seq != 6 {
		t.Fatalf("propose after prune seq = %d, %v, want 6", p.Seq, err)
	}
	var seqs []int64
	var before int64
	for {
		page, next, err := s.Proposals(ctx, "doc", revision.Page{Limit: 2, Before: before})
		if err != nil {
			t.Fatalf("proposals: %v", err)
		}
		for _, q := range page {
			seqs = append(seqs, q.Seq)
		}
		if next == 0 {
			break
		}
		before = next
	}
	if fmt.Sprint(seqs) != "[6 4 3 2 1]" {
		t.Fatalf("proposal seqs = %v", seqs)
	}
}
