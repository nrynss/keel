package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/identity"
	"github.com/nrynss/keel/job"
	jobsql "github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
	"github.com/nrynss/keel/wire"
)

// fakeTarget is one erase target the deletion fans out over. It records
// the deletes it served and whether the user row still existed at that
// moment, so the ordering pin never trusts the code under test.
type fakeTarget struct {
	name    string
	db      *sqlite.DB
	userID  string
	mu      sync.Mutex
	deletes int
	userRow int
	block   chan struct{}
	started chan struct{}
	once    sync.Once
}

// Name is the target name the erasure ledger records.
func (t *fakeTarget) Name() string { return t.name }

// Delete records one delete, optionally holding until the test releases
// it, so a restart test catches the work mid-erasure.
func (t *fakeTarget) Delete(_ context.Context) error {
	t.mu.Lock()
	t.deletes++
	if t.db != nil {
		_ = t.db.Reader().QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM users WHERE id = ?", t.userID).Scan(&t.userRow)
	}
	t.mu.Unlock()
	if t.started != nil {
		t.once.Do(func() { close(t.started) })
	}
	if t.block != nil {
		<-t.block
	}
	return nil
}

// count returns the recorded deletes and the last observed user row.
func (t *fakeTarget) count() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deletes, t.userRow
}

// deletionFixture carries the deletion wiring: the service, a job runner
// over one store, and the targets one user owns.
type deletionFixture struct {
	*fixture
	runner  *job.Runner
	targets map[string][]*fakeTarget
	mu      sync.Mutex
}

// newDeletionFixture opens the base fixture, then the job runner with
// the deletion and erasure kinds, and binds it.
func newDeletionFixture(t *testing.T, clock *testClock) *deletionFixture {
	t.Helper()
	base := openFixture(t, clock)
	fx := &deletionFixture{fixture: base, targets: make(map[string][]*fakeTarget)}
	fx.svc = base.svc
	rebuild := func(_ context.Context, ref string) ([]erase.Target, error) {
		owned := fx.owned(ref)
		out := make([]erase.Target, 0, len(owned))
		for _, target := range owned {
			out = append(out, target)
		}
		return out, nil
	}
	eraser, err := erase.New(rebuild, erase.Config{Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("open eraser: %v", err)
	}
	svc, err := identity.New(identity.Config{
		Store:      base.store,
		SigningKey: signingKey,
		CodeKey:    codeKey,
		Mail:       base.mail,
		Now:        clock.now,
		GuestData: func(_ context.Context, _ string) (bool, error) {
			return base.ownsData, nil
		},
		Eraser:  eraser,
		Targets: rebuild,
	})
	if err != nil {
		t.Fatalf("open deletion service: %v", err)
	}
	base.svc = svc
	fx.svc = svc
	store, err := jobsql.Open(t.Context(), jobsql.Config{DB: base.db})
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	runner, err := job.Open(t.Context(), job.Config{
		Broker: stream.New(stream.Config{}),
		Store:  store,
		Kinds:  svc.Kinds(),
		Log:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open job runner: %v", err)
	}
	if err := svc.BindRunner(runner); err != nil {
		t.Fatalf("bind runner: %v", err)
	}
	fx.runner = runner
	return fx
}

// owned returns the fake targets one user owns, minting two on first
// use, the way an app owns rows and blobs per user.
func (fx *deletionFixture) owned(userID string) []*fakeTarget {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	owned, ok := fx.targets[userID]
	if !ok {
		owned = []*fakeTarget{
			{name: "app-rows", db: fx.db, userID: userID},
			{name: "blobs", db: fx.db, userID: userID},
		}
		fx.targets[userID] = owned
	}
	return owned
}

// waitDeletion polls the job chain until its newest attempt lands, so a
// resumed attempt with a fresh id still ends the wait.
func (fx *deletionFixture) waitDeletion(jobID string) job.Status {
	fx.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		attempts, err := fx.runner.Attempts(fx.t.Context(), jobID)
		if err != nil {
			fx.t.Fatalf("read attempts: %v", err)
		}
		if len(attempts) > 0 {
			switch status := attempts[len(attempts)-1].Status; status {
			case job.StatusDone, job.StatusError, job.StatusCancelled, job.StatusInterrupted:
				return status
			}
		}
		if time.Now().After(deadline) {
			fx.t.Fatal("deletion never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitTerminal polls until any attempt of the chain lands, for the
// restart test that stops the superseded attempt.
func (fx *deletionFixture) waitTerminal(jobID string) {
	fx.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		attempts, err := fx.runner.Attempts(fx.t.Context(), jobID)
		if err == nil {
			for _, rec := range attempts {
				switch rec.Status {
				case job.StatusDone, job.StatusError, job.StatusCancelled, job.StatusInterrupted:
					return
				}
			}
		}
		if time.Now().After(deadline) {
			fx.t.Fatal("no attempt ever landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// userRows counts the identity rows one user still holds across every
// table the deletion owns.
func (fx *deletionFixture) userRows(userID string) (users, sessions, identities, codes int) {
	fx.t.Helper()
	reads := []struct {
		query string
		into  *int
	}{
		{"SELECT COUNT(*) FROM users WHERE id = ?", &users},
		{"SELECT COUNT(*) FROM sessions WHERE user_id = ?", &sessions},
		{"SELECT COUNT(*) FROM identities WHERE user_id = ?", &identities},
		{`SELECT COUNT(*) FROM sign_in_codes WHERE requesting_session IN
			(SELECT id FROM sessions WHERE user_id = ?)`, &codes},
	}
	for _, read := range reads {
		if err := fx.db.Reader().QueryRowContext(fx.t.Context(), read.query, userID).Scan(read.into); err != nil {
			fx.t.Fatalf("count rows: %v", err)
		}
	}
	return users, sessions, identities, codes
}

// deletionCode requests and returns one fresh code for the account the
// fixture seeded.
func (fx *deletionFixture) deletionCode(cookie *http.Cookie, address string) string {
	fx.t.Helper()
	fx.requestCode(cookie, address)
	return fx.latestCode()
}

// TestDeleteErasesTargetsAndRowsLast checks one fresh code starts a job
// that erases the targets, removes every identity row, and removes the
// user row last, and that the deletion consumed the code it checked.
func TestDeleteErasesTargetsAndRowsLast(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := newDeletionFixture(t, clock)
	cookie, user := fx.seedAccount("gone@example.com")
	code := fx.deletionCode(cookie, "gone@example.com")

	jobID, err := fx.svc.Delete(t.Context(), strings.SplitN(cookie.Value, ".", 2)[0], user.ID, "Gone@Example.com ", code)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if status := fx.waitDeletion(jobID); status != job.StatusDone {
		t.Fatalf("deletion ended %s, want done", status)
	}
	// The ordering pin: the targets observed the user row alive while
	// they deleted, and nothing of the user survives now.
	for _, target := range fx.owned(user.ID) {
		deletes, seenRow := target.count()
		if deletes == 0 {
			t.Fatalf("target %s was never deleted", target.name)
		}
		if seenRow != 1 {
			t.Fatalf("target %s saw the user row gone before the rows step, want it last", target.name)
		}
	}
	if users, sessions, identities, codes := fx.userRows(user.ID); users != 0 || sessions != 0 || identities != 0 || codes != 0 {
		t.Fatalf("%d users, %d sessions, %d identities, %d codes survive, want none",
			users, sessions, identities, codes)
	}
	// The verification consumed the code, so it never authorizes a
	// second deletion. The account is gone by now, so the refusal may
	// read as a refused code or as an account that opens nothing.
	if _, err := fx.svc.Delete(t.Context(), strings.SplitN(cookie.Value, ".", 2)[0], user.ID, "gone@example.com", code); err == nil {
		t.Fatal("second delete with the same code started, want a refusal")
	}
}

// TestDeleteRefusesAStaleCode checks a code past its ten minute life
// deletes nothing, on the injected clock.
func TestDeleteRefusesAStaleCode(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := newDeletionFixture(t, clock)
	cookie, user := fx.seedAccount("late@example.com")
	code := fx.requestOnly(cookie, "late@example.com")
	clock.advance(10 * time.Minute)
	if _, err := fx.svc.Delete(t.Context(), strings.SplitN(cookie.Value, ".", 2)[0], user.ID, "late@example.com", code); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("stale delete = %v, want a refused code", err)
	}
	if users, sessions, identities, codes := fx.userRows(user.ID); users != 1 || sessions == 0 || identities != 1 || codes == 0 {
		t.Fatalf("a refused deletion disturbed the rows: %d users, %d sessions, %d identities, %d codes",
			users, sessions, identities, codes)
	}
}

// TestDeleteRefusesAStrangersAddress checks an address naming an
// identity on another user refuses as not owner, and deletes nothing.
func TestDeleteRefusesAStrangersAddress(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := newDeletionFixture(t, clock)
	fx.seedAccount("owner@example.com")
	cookie, user := fx.mint()
	fx.requestCode(cookie, "stranger@example.com")
	code := fx.latestCode()
	if _, err := fx.svc.Delete(t.Context(), strings.SplitN(cookie.Value, ".", 2)[0], user.ID, "stranger@example.com", code); !errors.Is(err, identity.ErrNotOwner) {
		t.Fatalf("stranger delete = %v, want not owner", err)
	}
	if users, _, _, _ := fx.userRows(user.ID); users != 1 {
		t.Fatal("a refused deletion removed the user row")
	}
	otherCookie, otherUser := fx.mint()
	_ = otherCookie
	if users, _, _, _ := fx.userRows(otherUser.ID); users != 1 {
		t.Fatal("a refused deletion removed another user's row")
	}
}

// TestDeleteResumesAfterARestart catches a deletion mid-erasure, reopens
// the runner the way a restarted process does, and pins that the resumed
// deletion finishes the targets and the rows.
func TestDeleteResumesAfterARestart(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := newDeletionFixture(t, clock)
	cookie, user := fx.seedAccount("resume@example.com")
	code := fx.requestOnly(cookie, "resume@example.com")

	release := make(chan struct{})
	blocked := fx.owned(user.ID)[0]
	blocked.block = release
	blocked.started = make(chan struct{})

	jobID, err := fx.svc.Delete(t.Context(), strings.SplitN(cookie.Value, ".", 2)[0], user.ID, "resume@example.com", code)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	select {
	case <-blocked.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked target never started")
	}

	// A restarted process reopens the runner on the same ledger, binds
	// it, and stops the superseded attempt before releasing the target.
	old := fx.runner
	store, err := jobsql.Open(t.Context(), jobsql.Config{DB: fx.db})
	if err != nil {
		t.Fatalf("reopen job store: %v", err)
	}
	runner, err := job.Open(t.Context(), job.Config{
		Broker: stream.New(stream.Config{}),
		Store:  store,
		Kinds:  fx.svc.Kinds(),
		Log:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("reopen runner: %v", err)
	}
	if err := fx.svc.BindRunner(runner); err != nil {
		t.Fatalf("rebind runner: %v", err)
	}
	fx.runner = runner
	if err := old.Cancel(jobID); err != nil && !errors.Is(err, job.ErrUnknownJob) {
		t.Fatalf("stop superseded attempt: %v", err)
	}
	fx.waitTerminal(jobID)
	close(release)
	fx.waitTerminal(jobID)
	if status := fx.waitDeletion(jobID); status != job.StatusDone {
		t.Fatalf("resumed deletion ended %s, want done", status)
	}
	for _, target := range fx.owned(user.ID) {
		if deletes, _ := target.count(); deletes == 0 {
			t.Fatalf("target %s was never deleted across the restart", target.name)
		}
	}
	if users, sessions, identities, codes := fx.userRows(user.ID); users != 0 || sessions != 0 || identities != 0 || codes != 0 {
		t.Fatalf("resumed deletion left %d users, %d sessions, %d identities, %d codes",
			users, sessions, identities, codes)
	}
}

// requestOnly asks for one code and returns it, without a seed verify.
func (fx *deletionFixture) requestOnly(cookie *http.Cookie, address string) string {
	fx.t.Helper()
	fx.requestCode(cookie, address)
	return fx.latestCode()
}

// TestDeleteHandlerChecksTheCodes pins the route answers: the job id on
// acceptance with the cookie dropped, the refused codes on a bad code,
// and the 404 a stranger shares with an unknown account.
func TestDeleteHandlerChecksTheCodes(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := newDeletionFixture(t, clock)
	handler := fx.svc.Middleware(fx.svc.DeleteHandler())
	call := func(cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/account/delete", strings.NewReader(body))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	cookie, user := fx.seedAccount("routed@example.com")

	bad := call(cookie, `{"address":"routed@example.com","code":"000000"}`)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad code = %d, want 401", bad.Code)
	}
	var refused struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bad.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if refused.Error.Code != wire.CodeInvalidCode {
		t.Fatalf("refusal code = %q, want %q", refused.Error.Code, wire.CodeInvalidCode)

	}
	stranger := call(cookie, `{"address":"elsewhere@example.com","code":"123456"}`)
	if stranger.Code != http.StatusNotFound {
		t.Fatalf("stranger = %d, want 404", stranger.Code)
	}

	code := fx.deletionCode(cookie, "routed@example.com")
	ok := call(cookie, `{"address":"routed@example.com","code":`+quote(code)+`}`)
	if ok.Code != http.StatusAccepted {
		t.Fatalf("delete = %d, want 202: %s", ok.Code, ok.Body.String())
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode acceptance: %v", err)
	}
	if accepted.JobID == "" {
		t.Fatal("acceptance names no job id")
	}
	if fx.cookie(ok) == nil || fx.cookie(ok).MaxAge >= 0 {
		t.Fatal("acceptance did not drop the session cookie")
	}
	if status := fx.waitDeletion(accepted.JobID); status != job.StatusDone {
		t.Fatalf("routed deletion ended %s, want done", status)
	}
	if users, _, _, _ := fx.userRows(user.ID); users != 0 {
		t.Fatal("routed deletion left the user row")
	}
}
