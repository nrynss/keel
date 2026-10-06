package identity_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/identity"
	identitysql "github.com/nrynss/keel/identity/sqlitestore"
	"github.com/nrynss/keel/mediastore"
	mediastoresql "github.com/nrynss/keel/mediastore/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// signingKey is the HMAC key the session tests wire. It never leaves the
// test process.
var signingKey = []byte("test-session-signing-key-with-length")

// codeKey is the HMAC key the code tests wire. It never leaves the test
// process.
var codeKey = []byte("test-code-key-with-length")

// testClock is a fixed clock the test moves by hand. Every stamp the
// package writes comes from it, so no check reads the wall clock.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

// now returns the clock time.
func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance moves the clock forward.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// sentMail is one message the fake sender recorded.
type sentMail struct {
	address string
	code    string
}

// fakeSender records every send, so a test counts the mails a route
// actually handed out. It can refuse, the way a provider outage does.
type fakeSender struct {
	mu    sync.Mutex
	mails []sentMail
	fail  bool
	fails int
}

// Send records one message, or refuses while fail holds.
func (f *fakeSender) Send(_ context.Context, address, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		f.fails++
		return errSendFailed
	}
	f.mails = append(f.mails, sentMail{address: address, code: code})
	return nil
}

// sent returns the recorded messages, oldest first.
func (f *fakeSender) sent() []sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMail(nil), f.mails...)
}

// setFail turns the provider outage on or off.
func (f *fakeSender) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// errSendFailed is the refusal the fake sender reports.
var errSendFailed = errors.New("fake send failed")

// okHandler is the inner handler the middleware tests wrap. It answers
// 200, so a test asserts on the resolution and not on the inner route.
var okHandler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

// fixture carries one open store and service on one test clock.
type fixture struct {
	t     *testing.T
	clock *testClock
	db    *sqlite.DB
	path  string
	mail  *fakeSender
	store *identitysql.Store
	svc   *identity.Service

	// ownsData backs the guest data check. A test flips it to model a
	// guest with app rows.
	ownsData bool
}

// openFixture opens one database with the identity schema, then a
// service on the test clock with code sign-in wired.
func openFixture(t *testing.T, clock *testClock) *fixture {
	t.Helper()
	fx := &fixture{t: t, clock: clock, mail: &fakeSender{}}
	fx.path = filepath.Join(t.TempDir(), "identity.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   fx.path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fx.db = db
	store, err := identitysql.Open(t.Context(), identitysql.Config{DB: db})
	if err != nil {
		t.Fatalf("open identity store: %v", err)
	}
	fx.store = store
	fx.openService()
	return fx
}

// openService builds the service on the open store. Tests that rebind
// configuration call it again.
func (fx *fixture) openService() {
	fx.t.Helper()
	fx.ownsData = false
	svc, err := identity.New(identity.Config{
		Store:      fx.store,
		SigningKey: signingKey,
		CodeKey:    codeKey,
		Mail:       fx.mail,
		Now:        fx.clock.now,
		GuestData: func(_ context.Context, _ string) (bool, error) {
			return fx.ownsData, nil
		},
	})
	if err != nil {
		fx.t.Fatalf("open identity service: %v", err)
	}
	fx.svc = svc
}

// visit runs one request through the middleware and returns the response
// with the user and session the handler saw.
func (fx *fixture) visit(cookie *http.Cookie, bearer string, handler http.Handler) (*httptest.ResponseRecorder, identity.User, identity.Session, bool) {
	fx.t.Helper()
	var got identity.User
	var session identity.Session
	var ok bool
	wrapped := fx.svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = identity.UserFromContext(r.Context())
		session, _ = identity.SessionFromContext(r.Context())
		handler.ServeHTTP(w, r)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec, got, session, ok
}

// mint visits once with no token and returns the cookie the response
// set, with the user it minted.
func (fx *fixture) mint() (*http.Cookie, identity.User) {
	fx.t.Helper()
	rec, user, _, ok := fx.visit(nil, "", okHandler)
	if !ok {
		fx.t.Fatal("middleware resolved no user on a first visit")
	}
	cookie := fx.cookie(rec)
	if cookie == nil {
		fx.t.Fatal("first visit set no session cookie")
	}
	return cookie, user
}

// cookie returns the session cookie a response set.
func (fx *fixture) cookie(rec *httptest.ResponseRecorder) *http.Cookie {
	fx.t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "identity_session" {
			return c
		}
	}
	return nil
}

// TestFirstVisitMintsGuestAndSession checks a first visit creates a guest
// user row and a session row, and the cookie carries only the signed id.
func TestFirstVisitMintsGuestAndSession(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	if user.Kind != identity.KindGuest {
		t.Fatalf("first visit kind = %q, want %q", user.Kind, identity.KindGuest)
	}
	// A fresh handle on the same file reads the rows back, so the pin
	// never trusts the service's own pools.
	fresh, err := sqlite.Open(t.Context(), sqlite.Config{Path: fx.path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer fresh.Close()
	var kind string
	var sessions int
	if err := fresh.Reader().QueryRowContext(t.Context(),
		"SELECT kind FROM users WHERE id = ?", user.ID).Scan(&kind); err != nil {
		t.Fatalf("read user row: %v", err)
	}
	if kind != identity.KindGuest {
		t.Fatalf("stored kind = %q, want %q", kind, identity.KindGuest)
	}
	if err := fresh.Reader().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sessions WHERE user_id = ? AND revoked = 0", user.ID).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("fresh visit holds %d sessions, want 1", sessions)
	}
	if strings.Count(cookie.Value, ".") != 1 {
		t.Fatalf("cookie value %q is not one id plus one signature", cookie.Value)
	}
}

// TestSecondVisitKeepsTheUser checks a returning cookie resolves the
// same user without minting a second one.
func TestSecondVisitKeepsTheUser(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	rec, again, session, ok := fx.visit(cookie, "", okHandler)
	if !ok {
		t.Fatal("second visit resolved no user")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("second visit status = %d, want 200", rec.Code)
	}
	if again.ID != user.ID {
		t.Fatalf("second visit user = %q, want %q", again.ID, user.ID)
	}
	if session.ID != strings.SplitN(cookie.Value, ".", 2)[0] {
		t.Fatalf("resolved session %q does not match the cookie", session.ID)
	}
	if fx.cookie(rec) != nil {
		t.Fatal("second visit set a fresh cookie, want the browser to keep its own")
	}
}

// TestRevokedSessionFailsNextRequest checks a revoked session resolves
// as anonymous on the next request, and the minted guest is a new user.
func TestRevokedSessionFailsNextRequest(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	if err := fx.svc.Revoke(t.Context(), strings.SplitN(cookie.Value, ".", 2)[0]); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec, next, _, ok := fx.visit(cookie, "", okHandler)
	if !ok {
		t.Fatal("revoked visit resolved no user, want a minted guest")
	}
	if next.ID == user.ID {
		t.Fatal("revoked cookie kept its user, want a fresh guest")
	}
	if fx.cookie(rec) == nil {
		t.Fatal("the request after a revocation set no fresh cookie")
	}
}

// TestForgedAndUnknownCookiesFail checks every bad cookie minted a fresh
// guest instead of an error, and never resolved a user.
func TestForgedAndUnknownCookiesFail(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	good := cookie.Value
	cases := map[string]string{
		"tampered signature": good[:len(good)-2] + "00",
		"empty value":        "",
		"no signature":       strings.SplitN(good, ".", 2)[0],
		"unknown session":    "ffffffffffffffffffffffffffffffff." + strings.SplitN(good, ".", 2)[1],
		"garbage":            "not-a-token",
		"wrong id shape":     "short.00",
	}
	for name, value := range cases {
		bad := *cookie
		bad.Value = value
		_, stranger, _, ok := fx.visit(&bad, "", okHandler)
		if !ok {
			t.Fatalf("%s: resolved no user, want a minted guest", name)
		}
		if stranger.ID == user.ID {
			t.Fatalf("%s: resolved the forged user %q", name, user.ID)
		}
	}
}

// TestBearerCarriesTheSameSession checks the signed cookie value works
// as a bearer token, with the same rows and the same revocation.
func TestBearerCarriesTheSameSession(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	_, bearerUser, _, ok := fx.visit(nil, cookie.Value, okHandler)
	if !ok {
		t.Fatal("bearer request resolved no user")
	}
	if bearerUser.ID != user.ID {
		t.Fatalf("bearer user = %q, want the cookie user %q", bearerUser.ID, user.ID)
	}
	sessionID := strings.SplitN(cookie.Value, ".", 2)[0]
	if err := fx.svc.Revoke(t.Context(), sessionID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec, after, _, ok := fx.visit(nil, cookie.Value, okHandler)
	if !ok {
		t.Fatal("request after revocation resolved no user, want a minted guest")
	}
	if after.ID == user.ID {
		t.Fatal("revoked bearer token kept its user")
	}
	if fx.cookie(rec) == nil {
		t.Fatal("the request after a revoked bearer set no cookie")
	}
}

// TestResolveTouchesLastSeen checks a resolution stamps the last seen
// time from the injected clock.
func TestResolveTouchesLastSeen(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	clock.advance(5 * time.Minute)
	_, again, _, ok := fx.visit(cookie, "", okHandler)
	if !ok {
		t.Fatal("second visit resolved no user")
	}
	if !again.LastSeen.Equal(clock.now()) {
		t.Fatalf("last seen = %v, want the clock time %v", again.LastSeen, clock.now())
	}
	if user.LastSeen.Equal(again.LastSeen) {
		t.Fatal("last seen did not move")
	}
}

// TestOwnsChecksTheContextUser checks the ownership helper reads the
// middleware context and never matches an empty owner.
func TestOwnsChecksTheContextUser(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	var owned bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owned = fx.svc.Owns(r.Context(), user.ID)
	})
	if _, _, _, ok := fx.visit(cookie, "", handler); !ok {
		t.Fatal("visit resolved no user")
	}
	if !owned {
		t.Fatal("the owner failed its own ownership check")
	}
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fx.svc.Owns(r.Context(), "") {
			t.Fatal("an empty owner matched")
		}
		if fx.svc.Owns(r.Context(), "nobody") {
			t.Fatal("a stranger matched")
		}
		if _, ok := identity.UserFromContext(context.Background()); ok {
			t.Fatal("a bare context carried a user")
		}
	})
	if _, _, _, ok := fx.visit(cookie, "", handler); !ok {
		t.Fatal("visit resolved no user")
	}
}

// TestAuthorizeMediaChecksTheOwner opens a media store wired to the
// service, then pins the access rule: public passes signed out, private
// passes only its owner, and a stranger reads the same 404 as an unknown
// blob.
func TestAuthorizeMediaChecksTheOwner(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	index, err := mediastoresql.Open(t.Context(), mediastoresql.Config{DB: fx.db})
	if err != nil {
		t.Fatalf("open media index: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "blobs")
	store, err := mediastore.Open(t.Context(), mediastore.Config{
		Dir:       dir,
		Index:     index,
		Authorize: fx.svc.AuthorizeMedia,
	})
	if err != nil {
		t.Fatalf("open media store: %v", err)
	}
	cookie, user := fx.mint()
	private, err := store.Persist(t.Context(), strings.NewReader("private-bytes"), mediastore.Put{
		ContentType: "image/png",
		Owner:       user.ID,
		Visibility:  mediastore.Private,
	})
	if err != nil {
		t.Fatalf("persist private blob: %v", err)
	}
	public, err := store.Persist(t.Context(), strings.NewReader("public-bytes"), mediastore.Put{
		ContentType: "image/png",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist public blob: %v", err)
	}
	if code := fx.serveMedia(store, cookie, private); code != http.StatusOK {
		t.Fatalf("owner read of a private blob = %d, want 200", code)
	}
	if code := fx.serveMedia(store, nil, private); code != http.StatusNotFound {
		t.Fatalf("anonymous read of a private blob = %d, want 404", code)
	}
	otherCookie, _ := fx.mint()
	if code := fx.serveMedia(store, otherCookie, private); code != http.StatusNotFound {
		t.Fatalf("stranger read of a private blob = %d, want 404", code)
	}
	if code := fx.serveMedia(store, nil, public); code != http.StatusOK {
		t.Fatalf("anonymous read of a public blob = %d, want 200", code)
	}
}

// serveMedia reads one blob through the media store with one cookie.
// The store reads the blob id from the routed path value, so the request
// goes through a mux that names the same pattern an app registers.
func (fx *fixture) serveMedia(store *mediastore.Store, cookie *http.Cookie, blobID string) int {
	fx.t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /media/{id}", store)
	req := httptest.NewRequest(http.MethodGet, "/media/"+blobID, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// TestOpenTwiceMigratesCleanly checks a second open of the store and the
// service on the same database changes nothing and still resolves.
func TestOpenTwiceMigratesCleanly(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	cookie, user := fx.mint()
	if _, err := identitysql.Open(t.Context(), identitysql.Config{DB: fx.db}); err != nil {
		t.Fatalf("second store open: %v", err)
	}
	fx.openService()
	_, again, _, ok := fx.visit(cookie, "", okHandler)
	if !ok {
		t.Fatal("second open resolved no user")
	}
	if again.ID != user.ID {
		t.Fatalf("second open user = %q, want %q", again.ID, user.ID)
	}
}

// TestNewRefusesBadConfig checks the constructor names unusable
// configuration instead of returning a service that cannot run.
func TestNewRefusesBadConfig(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openFixture(t, clock)
	if _, err := identity.New(identity.Config{SigningKey: signingKey}); err == nil {
		t.Fatal("service with no store built, want an error")
	}
	if _, err := identity.New(identity.Config{Store: fx.store}); err == nil {
		t.Fatal("service with no signing key built, want an error")
	}
	if _, err := identity.New(identity.Config{
		Store:      fx.store,
		SigningKey: signingKey,
		Targets: func(context.Context, string) ([]erase.Target, error) {
			return nil, nil
		},
	}); err == nil {
		t.Fatal("service with targets and no eraser built, want an error")
	}
	if _, err := identity.New(identity.Config{
		Store:      fx.store,
		SigningKey: signingKey,
		CodeKey:    codeKey,
	}); err != nil {
		t.Fatalf("service with a code key and no mail sender refused, want it built with code sign-in disabled: %v", err)
	}
	if err := fx.svc.RequestSignInCode(t.Context(), "session", "reader@example.com"); err == nil {
		t.Fatal("code request on a service with no mail sender sent, want a refusal")
	}
}
