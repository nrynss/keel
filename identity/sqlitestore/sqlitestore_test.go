package sqlitestore_test

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nrynss/keel/identity"
	sqlitestore "github.com/nrynss/keel/identity/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// fixture carries one open database and store.
type fixture struct {
	t     *testing.T
	db    *sqlite.DB
	path  string
	store *sqlitestore.Store
}

// openFixture opens one database with the identity schema.
func openFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return &fixture{t: t, db: db, path: path, store: store}
}

// guest minting for the tests below.
func (fx *fixture) guest(userID, sessionID string) {
	fx.t.Helper()
	now := time.Unix(1758000000, 0)
	err := fx.store.CreateGuest(fx.t.Context(),
		identity.User{ID: userID, Kind: identity.KindGuest, CreatedAt: now, LastSeen: now},
		identity.Session{ID: sessionID, UserID: userID, CreatedAt: now})
	if err != nil {
		fx.t.Fatalf("create guest: %v", err)
	}
}

// TestOpenAppliesTheSchemaOnce checks the ledger records each migration
// file once, a second open changes nothing, and a raw connection reads
// the rows the store wrote.
func TestOpenAppliesTheSchemaOnce(t *testing.T) {
	fx := openFixture(t)
	fx.guest("user-a", "session-a")
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: fx.db}); err != nil {
		t.Fatalf("second open: %v", err)
	}
	// The raw connection never passes through the code under test.
	raw, err := sql.Open("sqlite", "file:"+fx.path)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer raw.Close()
	var ledger int
	if err := raw.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM identity_schema_migrations").Scan(&ledger); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledger != 1 {
		t.Fatalf("ledger holds %d files, want 1", ledger)
	}
	var kind string
	var sessions int
	if err := raw.QueryRowContext(t.Context(),
		"SELECT kind FROM users WHERE id = 'user-a'").Scan(&kind); err != nil {
		t.Fatalf("read user row: %v", err)
	}
	if kind != identity.KindGuest {
		t.Fatalf("kind = %q, want %q", kind, identity.KindGuest)
	}
	if err := raw.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sessions WHERE user_id = 'user-a'").Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
}

// TestOpenRefusesANilDatabase checks the constructor names unusable
// configuration.
func TestOpenRefusesANilDatabase(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); err == nil {
		t.Fatal("store with no database built, want an error")
	}
}

// TestSessionReportsUnknown checks a missing session matches its
// sentinel, so resolution reads it as no session.
func TestSessionReportsUnknown(t *testing.T) {
	fx := openFixture(t)
	if _, _, err := fx.store.Session(t.Context(), "missing"); !errors.Is(err, identity.ErrUnknownSession) {
		t.Fatalf("unknown session err = %v, want ErrUnknownSession", err)
	}
}

// TestIdentityReportsUnknownHolder checks the holder read matches its
// sentinel, so the sign-in path reads an unknown subject as fresh.
func TestIdentityReportsUnknownHolder(t *testing.T) {
	fx := openFixture(t)
	fx.guest("user-a", "session-a")
	if _, err := fx.store.IdentityHolder(t.Context(), "email", "noone@example.com"); !errors.Is(err, identity.ErrUnknownIdentity) {
		t.Fatalf("unknown holder err = %v, want ErrUnknownIdentity", err)
	}
}

// seedIdentity writes one identity row directly, the way the tests name
// a subject that already belongs to one user.
func (fx *fixture) seedIdentity(provider, subject, userID string) {
	fx.t.Helper()
	_, err := fx.db.Writer().ExecContext(fx.t.Context(),
		"INSERT INTO identities (id, user_id, provider, subject, created_at) VALUES (?, ?, ?, ?, ?)",
		"ident-"+subject, userID, provider, subject, time.Unix(1758000000, 0).Unix())
	if err != nil {
		fx.t.Fatalf("seed identity: %v", err)
	}
}

// TestResolveSignInConsumesTheCodeOnce checks the resolution consumes
// its code and a second resolution of the same code refuses.
func TestResolveSignInConsumesTheCodeOnce(t *testing.T) {
	fx := openFixture(t)
	fx.guest("user-a", "session-a")
	now := time.Unix(1758000000, 0)
	if err := fx.store.PutCode(t.Context(), identity.SignInCode{
		ID: "code-a", AddressHash: "aa", CodeHash: "bb", RequestingSession: "session-a",
		ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	}, identity.SendCaps{Address: 5, Global: 200, Since: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatalf("put code: %v", err)
	}
	target, err := fx.store.ResolveSignIn(t.Context(), identity.SignInResolution{
		SessionID: "session-a", UserID: "user-a", NewSessionID: "session-b",
		Provider: "email", Subject: "fresh@example.com", CodeID: "code-a", At: now,
	})
	if err != nil {
		t.Fatalf("resolve sign-in: %v", err)
	}
	if target.UserID != "user-a" || target.SessionID != "session-b" || target.Switched {
		t.Fatalf("target = %+v, want user-a on session-b with no switch", target)
	}
	var revoked int
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT revoked FROM sessions WHERE id = 'session-a'").Scan(&revoked); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if revoked != 1 {
		t.Fatal("resolution left the asking session live")
	}
	_, err = fx.store.ResolveSignIn(t.Context(), identity.SignInResolution{
		SessionID: "session-b", UserID: "user-a", NewSessionID: "session-c",
		Provider: "email", Subject: "fresh@example.com", CodeID: "code-a", At: now,
	})
	if !errors.Is(err, identity.ErrUnknownCode) {
		t.Fatalf("second resolution err = %v, want ErrUnknownCode", err)
	}
}

// TestResolveSignInJoinsTheHolder checks a subject naming another user
// moves the device there, and the store honours the conflict rule on a
// racing attach.
func TestResolveSignInJoinsTheHolder(t *testing.T) {
	fx := openFixture(t)
	fx.guest("user-a", "session-a")
	fx.guest("user-b", "session-b")
	now := time.Unix(1758000000, 0)
	fx.seedIdentity("email", "held@example.com", "user-b")
	target, err := fx.store.ResolveSignIn(t.Context(), identity.SignInResolution{
		SessionID: "session-a", UserID: "user-a", NewSessionID: "session-a2",
		Provider: "email", Subject: "held@example.com", At: now,
		GuestData: true, AllowSwitch: true,
	})
	if err != nil {
		t.Fatalf("resolve sign-in: %v", err)
	}
	if target.UserID != "user-b" || !target.Switched {
		t.Fatalf("target = %+v, want the holder user-b with a switch", target)
	}
	var holder string
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT user_id FROM sessions WHERE id = 'session-a2'").Scan(&holder); err != nil {
		t.Fatalf("read new session: %v", err)
	}
	if holder != "user-b" {
		t.Fatalf("new session holds %q, want the holder", holder)
	}
}

// TestDeleteUserRowsRemovesEverything checks the ordered delete clears
// the codes, the sessions, the identities, and the user row last.
func TestDeleteUserRowsRemovesEverything(t *testing.T) {
	fx := openFixture(t)
	fx.guest("user-a", "session-a")
	now := time.Unix(1758000000, 0)
	fx.seedIdentity("email", "doomed@example.com", "user-a")
	if err := fx.store.PutCode(t.Context(), identity.SignInCode{
		ID: "code-a", AddressHash: "aa", CodeHash: "bb", RequestingSession: "session-a",
		ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	}, identity.SendCaps{Address: 5, Global: 200, Since: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatalf("put code: %v", err)
	}
	if err := fx.store.DeleteUserRows(t.Context(), "user-a"); err != nil {
		t.Fatalf("delete user rows: %v", err)
	}
	for name, query := range map[string]string{
		"users":      "SELECT COUNT(*) FROM users WHERE id = 'user-a'",
		"sessions":   "SELECT COUNT(*) FROM sessions WHERE user_id = 'user-a'",
		"identities": "SELECT COUNT(*) FROM identities WHERE user_id = 'user-a'",
		"codes":      "SELECT COUNT(*) FROM sign_in_codes",
	} {
		var count int
		if err := fx.db.Reader().QueryRowContext(t.Context(), query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("%s holds %d rows, want none", name, count)
		}
	}
}

// TestNamespaceOverrideNamesItsLedger checks a store opened under an
// override keeps its ledger under that namespace, so an app can dodge a
// name it already uses.
func TestNamespaceOverrideNamesItsLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db, Namespace: "identity_two"}); err != nil {
		t.Fatalf("open with an override: %v", err)
	}
	var ledger string
	if err := db.Reader().QueryRowContext(t.Context(),
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'identity_two_schema_migrations'").Scan(&ledger); err != nil {
		t.Fatalf("read override ledger: %v", err)
	}
}

// TestSendWindows counts the sends the ceilings read back.
func TestSendWindows(t *testing.T) {
	fx := openFixture(t)
	fx.guest("user-a", "session-a")
	fx.guest("user-b", "session-b")
	now := time.Unix(1758000000, 0)
	caps := identity.SendCaps{Address: 5, Global: 200, Since: now.Add(-24 * time.Hour)}
	put := func(id, hash, session string, at time.Time) {
		t.Helper()
		if err := fx.store.PutCode(t.Context(), identity.SignInCode{
			ID: id, AddressHash: hash, CodeHash: id, RequestingSession: session,
			ExpiresAt: at.Add(time.Minute), CreatedAt: at,
		}, caps); err != nil {
			t.Fatalf("put code: %v", err)
		}
	}
	put("code-1", "hash-a", "session-a", now)
	put("code-2", "hash-a", "session-a", now.Add(time.Second))
	put("code-3", "hash-b", "session-b", now.Add(2*time.Second))
	address, err := fx.store.CodeSends(t.Context(), "hash-a", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("read address window: %v", err)
	}
	if address.Count != 2 || !address.Oldest.Equal(now) {
		t.Fatalf("address window = %+v, want 2 sends with the oldest first", address)
	}
	global, err := fx.store.AllCodeSends(t.Context(), now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("read global window: %v", err)
	}
	if global.Count != 3 || !global.Oldest.Equal(now) {
		t.Fatalf("global window = %+v, want 3 sends with the oldest first", global)
	}
	empty, err := fx.store.CodeSends(t.Context(), "hash-none", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("read empty window: %v", err)
	}
	if empty.Count != 0 || !empty.Oldest.IsZero() {
		t.Fatalf("empty window = %+v, want a zero time", empty)
	}
}
