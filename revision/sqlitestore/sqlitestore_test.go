package sqlitestore_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/revision"
	"github.com/nrynss/keel/revision/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the test clock starts at. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// clock is a test clock that advances one second per read.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// now returns the next instant.
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// openDB opens the database file at path and closes it at cleanup.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// openStore opens a revision store over a fresh file.
func openStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	c := &clock{t: base}
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB: openDB(t, filepath.Join(t.TempDir(), "revision.db")), Logger: slog.New(slog.DiscardHandler), Now: c.now,
	})
	if err != nil {
		t.Fatalf("open revision store: %v", err)
	}
	return store
}

// create makes doc with a first body and fails the test on error.
func create(t *testing.T, s *sqlitestore.Store, doc, owner string) revision.Revision {
	t.Helper()
	r, err := s.Create(t.Context(), doc, owner, []byte(`{"v":1}`), "ana")
	if err != nil {
		t.Fatalf("create %s: %v", doc, err)
	}
	return r
}

// commit commits body on parent and fails the test on error.
func commit(t *testing.T, s *sqlitestore.Store, doc string, parent int64, body string) revision.Revision {
	t.Helper()
	r, err := s.Commit(t.Context(), doc, parent, []byte(body), "ana")
	if err != nil {
		t.Fatalf("commit %s on %d: %v", doc, parent, err)
	}
	return r
}

func TestOpenRejectsNilDB(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("Open(nil DB) = %v", err)
	}
}

func TestOpenTwiceOnOneFileChangesNothing(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "revision.db"))
	for i := range 2 {
		if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db, Logger: slog.New(slog.DiscardHandler)}); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
}

func TestCreateCommitGetRoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	first := create(t, s, "doc", "owner-1")
	if first.Number != 1 || first.Parent != 0 || first.Author != "ana" {
		t.Fatalf("first = %+v", first)
	}
	second := commit(t, s, "doc", 1, `{"v":2}`)
	if second.Number != 2 || second.Parent != 1 {
		t.Fatalf("second = %+v", second)
	}
	got, err := s.Get(ctx, "doc", 2)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Body) != `{"v":2}` || !got.CommittedAt.Equal(second.CommittedAt) || got.CommittedAt.Nanosecond() != base.Nanosecond() {
		t.Fatalf("get = %+v, want %+v", got, second)
	}
	d, err := s.Document(ctx, "doc")
	if err != nil || d.Head != 2 || d.Owner != "owner-1" || !d.CreatedAt.Equal(first.CommittedAt) {
		t.Fatalf("document = %+v, %v", d, err)
	}
}

func TestCreateRefusesBadInputAndDuplicates(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	cases := []struct {
		name              string
		doc, owner, actor string
		body              string
	}{
		{"empty doc", "", "o", "a", `{}`},
		{"empty owner", "d", "", "a", `{}`},
		{"empty author", "d", "o", "", `{}`},
		{"not json", "d", "o", "a", `{nope`},
		{"empty body", "d", "o", "a", ``},
	}
	for _, c := range cases {
		if _, err := s.Create(ctx, c.doc, c.owner, []byte(c.body), c.actor); !errors.Is(err, revision.ErrInvalid) {
			t.Errorf("%s: Create = %v, want ErrInvalid", c.name, err)
		}
	}
	create(t, s, "d", "o")
	if _, err := s.Create(ctx, "d", "other", []byte(`{}`), "a"); !errors.Is(err, revision.ErrExists) {
		t.Fatalf("duplicate Create = %v", err)
	}
	if d, _ := s.Document(ctx, "d"); d.Owner != "o" || d.Head != 1 {
		t.Fatalf("duplicate create changed the document: %+v", d)
	}
}

func TestCommitOnAStaleParentConflictsWithTheHead(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	commit(t, s, "doc", 1, `{"v":2}`)
	for _, parent := range []int64{0, 1, 3} {
		_, err := s.Commit(ctx, "doc", parent, []byte(`{"stale":true}`), "bo")
		var c *revision.ConflictError
		if !errors.As(err, &c) || !errors.Is(err, revision.ErrConflict) || c.Head != 2 || c.Parent != parent || c.Document != "doc" {
			t.Fatalf("Commit on %d = %v", parent, err)
		}
	}
	if h, _, _ := s.History(ctx, "doc", revision.Page{}); len(h) != 2 {
		t.Fatalf("a refused commit stored a revision: %+v", h)
	}
	if _, err := s.Commit(ctx, "missing", 0, []byte(`{}`), "a"); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Fatalf("Commit on missing = %v", err)
	}
}

// TestConcurrentCommitsOnOneParentAdmitExactlyOne pins the compare-and-set:
// many writers racing on the same head produce one revision and conflicts.
func TestConcurrentCommitsOnOneParentAdmitExactlyOne(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	const writers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, conflicts := 0, 0
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Commit(context.Background(), "doc", 1, []byte(`{"v":2}`), "ana")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, revision.ErrConflict):
				conflicts++
			default:
				t.Errorf("commit: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || conflicts != writers-1 {
		t.Fatalf("won %d, conflicts %d", won, conflicts)
	}
}

func TestHistoryIsNewestFirstWithoutBodies(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	commit(t, s, "doc", 1, `{"v":2}`)
	commit(t, s, "doc", 2, `{"v":3}`)
	h, _, err := s.History(t.Context(), "doc", revision.Page{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h) != 3 || h[0].Number != 3 || h[1].Number != 2 || h[2].Number != 1 {
		t.Fatalf("history = %+v", h)
	}
	for _, r := range h {
		if r.Body != nil {
			t.Fatalf("history carried a body: %+v", r)
		}
	}
	if _, _, err := s.History(t.Context(), "missing", revision.Page{}); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Fatalf("History(missing) = %v", err)
	}
}

func TestGetTellsUnknownDocumentFromUnknownRevision(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	if _, err := s.Get(t.Context(), "doc", 7); !errors.Is(err, revision.ErrUnknownRevision) {
		t.Fatalf("Get(doc, 7) = %v", err)
	}
	if _, err := s.Get(t.Context(), "missing", 1); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Fatalf("Get(missing, 1) = %v", err)
	}
}

func TestPinsAreStableAndNeverMoveSilently(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	commit(t, s, "doc", 1, `{"v":2}`)
	p, err := s.Pin(ctx, "doc", 1, "approved")
	if err != nil || p.Revision != 1 || p.Name != "approved" {
		t.Fatalf("pin = %+v, %v", p, err)
	}
	again, err := s.Pin(ctx, "doc", 1, "approved")
	if err != nil || !again.PinnedAt.Equal(p.PinnedAt) {
		t.Fatalf("repeat pin = %+v, %v, want the first pin back", again, err)
	}
	if _, err := s.Pin(ctx, "doc", 2, "approved"); !errors.Is(err, revision.ErrPinTaken) {
		t.Fatalf("moving pin = %v", err)
	}
	if _, err := s.Pin(ctx, "doc", 9, "x"); !errors.Is(err, revision.ErrUnknownRevision) {
		t.Fatalf("pin unknown rev = %v", err)
	}
	if _, err := s.Pin(ctx, "doc", 1, ""); !errors.Is(err, revision.ErrInvalid) {
		t.Fatalf("pin empty name = %v", err)
	}
	if _, err := s.Pin(ctx, "doc", 2, "draft"); err != nil {
		t.Fatalf("second pin: %v", err)
	}
	pins, err := s.Pins(ctx, "doc")
	if err != nil || len(pins) != 2 || pins[0].Name != "approved" || pins[1].Name != "draft" {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	if err := s.Unpin(ctx, "doc", "approved"); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if err := s.Unpin(ctx, "doc", "approved"); !errors.Is(err, revision.ErrUnknownPin) {
		t.Fatalf("repeat unpin = %v", err)
	}
	if _, err := s.Pin(ctx, "doc", 2, "approved"); err != nil {
		t.Fatalf("re-pin after unpin: %v", err)
	}
}

func TestPruneSparesPinsAndTheHead(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	for parent := int64(1); parent < 6; parent++ {
		commit(t, s, "doc", parent, `{}`)
	}
	if _, err := s.Pin(ctx, "doc", 2, "take"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := s.Prune(ctx, "doc", 0); !errors.Is(err, revision.ErrInvalid) {
		t.Fatalf("Prune(keep 0) = %v", err)
	}
	n, err := s.Prune(ctx, "doc", 1)
	if err != nil || n != 4 {
		t.Fatalf("prune = %d, %v, want 4", n, err)
	}
	h, _, _ := s.History(ctx, "doc", revision.Page{})
	if len(h) != 2 || h[0].Number != 6 || h[1].Number != 2 {
		t.Fatalf("history after prune = %+v", h)
	}
	if _, err := s.Get(ctx, "doc", 3); !errors.Is(err, revision.ErrUnknownRevision) {
		t.Fatalf("Get(pruned) = %v", err)
	}
	if n, err := s.Prune(ctx, "doc", 1); err != nil || n != 0 {
		t.Fatalf("repeat prune = %d, %v", n, err)
	}
	commit(t, s, "doc", 6, `{}`)
	if _, err := s.Prune(ctx, "missing", 1); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Fatalf("Prune(missing) = %v", err)
	}
}

func TestProposalStaysASuggestionUntilAccepted(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	p, err := s.Propose(ctx, "doc", 1, []byte(`{"v":"model"}`), "model")
	if err != nil || p.State != revision.ProposalOpen || p.ID == "" {
		t.Fatalf("propose = %+v, %v", p, err)
	}
	if d, _ := s.Document(ctx, "doc"); d.Head != 1 {
		t.Fatalf("a proposal moved the head to %d", d.Head)
	}
	rev, err := s.Accept(ctx, p.ID, "ana")
	if err != nil || rev.Number != 2 || rev.Parent != 1 || rev.Author != "ana" || rev.Proposal != p.ID || string(rev.Body) != `{"v":"model"}` {
		t.Fatalf("accept = %+v, %v", rev, err)
	}
	got, err := s.Proposal(ctx, p.ID)
	if err != nil || got.State != revision.ProposalAccepted || got.Revision != 2 || got.DecidedBy != "ana" || got.DecidedAt.IsZero() || got.Author != "model" {
		t.Fatalf("accepted proposal = %+v, %v", got, err)
	}
	if _, err := s.Accept(ctx, p.ID, "ana"); !errors.Is(err, revision.ErrClosed) {
		t.Fatalf("second accept = %v", err)
	}
	if _, err := s.Reject(ctx, p.ID, "ana"); !errors.Is(err, revision.ErrClosed) {
		t.Fatalf("reject after accept = %v", err)
	}
}

func TestAcceptOfAStaleProposalConflictsAndStaysOpen(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	p, err := s.Propose(ctx, "doc", 1, []byte(`{}`), "model")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	commit(t, s, "doc", 1, `{"v":2}`)
	_, err = s.Accept(ctx, p.ID, "ana")
	var c *revision.ConflictError
	if !errors.As(err, &c) || c.Head != 2 || c.Parent != 1 {
		t.Fatalf("stale accept = %v", err)
	}
	if got, _ := s.Proposal(ctx, p.ID); got.State != revision.ProposalOpen {
		t.Fatalf("stale accept closed the proposal: %+v", got)
	}
	r, err := s.Reject(ctx, p.ID, "ana")
	if err != nil || r.State != revision.ProposalRejected || r.DecidedBy != "ana" {
		t.Fatalf("reject = %+v, %v", r, err)
	}
	if got, _ := s.Proposal(ctx, p.ID); got.State != revision.ProposalRejected || got.DecidedAt.IsZero() {
		t.Fatalf("rejected proposal = %+v", got)
	}
	if d, _ := s.Document(ctx, "doc"); d.Head != 2 {
		t.Fatalf("reject moved the head to %d", d.Head)
	}
}

func TestProposeAndDecideRefuseBadInput(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	for _, parent := range []int64{0, 2} {
		if _, err := s.Propose(ctx, "doc", parent, []byte(`{}`), "m"); !errors.Is(err, revision.ErrUnknownRevision) {
			t.Errorf("Propose on %d = %v", parent, err)
		}
	}
	if _, err := s.Propose(ctx, "missing", 1, []byte(`{}`), "m"); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Errorf("Propose(missing) = %v", err)
	}
	if _, err := s.Propose(ctx, "doc", 1, []byte(`nope`), "m"); !errors.Is(err, revision.ErrInvalid) {
		t.Errorf("Propose(bad body) = %v", err)
	}
	if _, err := s.Accept(ctx, "nope", "a"); !errors.Is(err, revision.ErrUnknownProposal) {
		t.Errorf("Accept(unknown) = %v", err)
	}
	if _, err := s.Reject(ctx, "nope", ""); !errors.Is(err, revision.ErrInvalid) {
		t.Errorf("Reject(empty author) = %v", err)
	}
	if _, err := s.Proposal(ctx, "nope"); !errors.Is(err, revision.ErrUnknownProposal) {
		t.Errorf("Proposal(unknown) = %v", err)
	}
}

func TestProposalsListNewestFirst(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	a, _ := s.Propose(ctx, "doc", 1, []byte(`{"a":1}`), "m")
	b, _ := s.Propose(ctx, "doc", 1, []byte(`{"b":1}`), "m")
	list, _, err := s.Proposals(ctx, "doc", revision.Page{})
	if err != nil || len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("proposals = %+v, %v", list, err)
	}
	if _, _, err := s.Proposals(ctx, "missing", revision.Page{}); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Fatalf("Proposals(missing) = %v", err)
	}
}

func TestForgetOwnerRemovesEverythingIncludingPins(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "a", "owner-1")
	create(t, s, "b", "owner-1")
	create(t, s, "keep", "owner-2")
	commit(t, s, "a", 1, `{}`)
	if _, err := s.Pin(ctx, "a", 1, "take"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	p, err := s.Propose(ctx, "b", 1, []byte(`{}`), "m")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if err := s.ForgetOwner(ctx, "owner-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	for _, doc := range []string{"a", "b"} {
		if _, err := s.Document(ctx, doc); !errors.Is(err, revision.ErrUnknownDocument) {
			t.Fatalf("Document(%s) after forget = %v", doc, err)
		}
	}
	if _, err := s.Proposal(ctx, p.ID); !errors.Is(err, revision.ErrUnknownProposal) {
		t.Fatalf("proposal after forget = %v", err)
	}
	if _, err := s.Document(ctx, "keep"); err != nil {
		t.Fatalf("another owner's document went: %v", err)
	}
	if err := s.ForgetOwner(ctx, "owner-1"); !errors.Is(err, revision.ErrUnknownDocument) {
		t.Fatalf("repeat forget = %v", err)
	}
	// The id is free again once forgotten.
	create(t, s, "a", "owner-3")
	if pins, err := s.Pins(ctx, "a"); err != nil || len(pins) != 0 {
		t.Fatalf("pins of a recreated document = %+v, %v", pins, err)
	}
}

func TestCancelledContextStoresNothing(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Commit(ctx, "doc", 1, []byte(`{}`), "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled commit = %v", err)
	}
	if d, _ := s.Document(t.Context(), "doc"); d.Head != 1 {
		t.Fatalf("cancelled commit moved the head to %d", d.Head)
	}
}

// TestRacingAcceptsOfOneProposalCommitOnce pins that a proposal accepted
// from two places at once becomes exactly one revision.
func TestRacingAcceptsOfOneProposalCommitOnce(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	p, err := s.Propose(t.Context(), "doc", 1, []byte(`{}`), "m")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, closed := 0, 0
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Accept(context.Background(), p.ID, "ana")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, revision.ErrClosed):
				closed++
			default:
				t.Errorf("accept: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || closed != racers-1 {
		t.Fatalf("won %d, closed %d", won, closed)
	}
	if d, _ := s.Document(t.Context(), "doc"); d.Head != 2 {
		t.Fatalf("head = %d, want 2", d.Head)
	}
}

// TestCommitsAcrossTwoHandlesOnOneFileKeepTheRule pins the compare-and-set
// when two processes, each with its own handle, write one file.
func TestCommitsAcrossTwoHandlesOnOneFileKeepTheRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "revision.db")
	var stores [2]*sqlitestore.Store
	for i := range stores {
		st, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path), Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		stores[i] = st
	}
	create(t, stores[0], "doc", "o")
	const writers = 8
	const rounds = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range writers {
		wg.Add(1)
		go func(st *sqlitestore.Store) {
			defer wg.Done()
			for range rounds {
				d, err := st.Document(context.Background(), "doc")
				if err != nil {
					t.Errorf("document: %v", err)
					return
				}
				_, err = st.Commit(context.Background(), "doc", d.Head, []byte(`{}`), "ana")
				if err != nil && !errors.Is(err, revision.ErrConflict) {
					t.Errorf("commit: %v", err)
					return
				}
				if err == nil {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}
		}(stores[i%2])
	}
	wg.Wait()
	d, err := stores[1].Document(t.Context(), "doc")
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	if d.Head != int64(won)+1 {
		t.Fatalf("head %d after %d won commits, want %d", d.Head, won, won+1)
	}
	h, _, _ := stores[1].History(t.Context(), "doc", revision.Page{Limit: revision.MaxLimit})
	for i, r := range h {
		if r.Number != d.Head-int64(i) || r.Parent != r.Number-1 {
			t.Fatalf("history %d = %+v, the chain has a gap or a fork", i, r)
		}
	}
}

// TestPruneDropsDecidedProposalsAndKeepsOpenOnes pins that pruning bounds
// the proposal table without discarding an undecided suggestion.
func TestPruneDropsDecidedProposalsAndKeepsOpenOnes(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	create(t, s, "doc", "o")
	rejected, _ := s.Propose(ctx, "doc", 1, []byte(`{}`), "m")
	open, _ := s.Propose(ctx, "doc", 1, []byte(`{}`), "m")
	accepted, _ := s.Propose(ctx, "doc", 1, []byte(`{}`), "m")
	if _, err := s.Reject(ctx, rejected.ID, "ana"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := s.Accept(ctx, accepted.ID, "ana"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	commit(t, s, "doc", 2, `{}`)
	if _, err := s.Prune(ctx, "doc", 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	list, _, _ := s.Proposals(ctx, "doc", revision.Page{})
	if len(list) != 1 || list[0].ID != open.ID {
		t.Fatalf("proposals after prune = %+v, want only the open one", list)
	}
}

// TestEmptyIDsAreInvalidEverywhere pins that an empty document or owner is
// refused as input, never read as an absent row.
func TestEmptyIDsAreInvalidEverywhere(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	checks := map[string]error{}
	_, checks["Document"] = s.Document(ctx, "")
	_, _, checks["History"] = s.History(ctx, "", revision.Page{})
	_, checks["Get"] = s.Get(ctx, "", 1)
	_, checks["Pin"] = s.Pin(ctx, "", 1, "x")
	checks["Unpin"] = s.Unpin(ctx, "", "x")
	_, checks["Pins"] = s.Pins(ctx, "")
	_, checks["Prune"] = s.Prune(ctx, "", 1)
	_, _, checks["Proposals"] = s.Proposals(ctx, "", revision.Page{})
	checks["ForgetOwner"] = s.ForgetOwner(ctx, "")
	for name, err := range checks {
		if !errors.Is(err, revision.ErrInvalid) {
			t.Errorf("%s with an empty id = %v, want ErrInvalid", name, err)
		}
	}
}

// TestStampsReadBackInUTC pins the location every read stamps, as the other
// stores do.
func TestStampsReadBackInUTC(t *testing.T) {
	s := openStore(t)
	create(t, s, "doc", "o")
	r, err := s.Get(t.Context(), "doc", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	d, _ := s.Document(t.Context(), "doc")
	if r.CommittedAt.Location() != time.UTC || d.CreatedAt.Location() != time.UTC {
		t.Fatalf("stamps not UTC: %v %v", r.CommittedAt.Location(), d.CreatedAt.Location())
	}
}
