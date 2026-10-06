// Package sqlitestore implements the identity store over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner, so the ledger records what ran and a second run
// changes nothing. Reads go through the read pool and every write through
// the single write connection, so concurrent sign-ins queue rather than
// race for the file lock. A resolved sign-in runs in one transaction, so
// a code is consumed, an identity is attached and a session is rotated
// together or not at all.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/identity"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns.
// It shares the database file with every other namespace without
// colliding, because each keeps its own ledger.
const schemaNamespace = "identity"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned by Open for unusable configuration, such as a
// nil database.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database the store writes through. It must not be
	// nil, and this package never closes it.
	DB *sqlite.DB
	// Namespace overrides the migration ledger namespace. Empty means the
	// one this package owns, which lets two stores share one file.
	Namespace string
}

// Store is the identity store over one database. Create it with Open,
// because the zero value has no database. Store is safe for concurrent
// use.
type Store struct {
	db *sqlite.DB
}

// Open applies the identity schema to cfg.DB and returns the store. The
// schema arrives as ordered SQL files, so the migration runner owns what
// ran and records each file once.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: nil db", ErrInvalid)
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = schemaNamespace
	}
	schema, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	if err := sqlite.Migrate(ctx, cfg.DB, namespace, schema); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	return &Store{db: cfg.DB}, nil
}

// CreateGuest writes the user row and its session row in one
// transaction, so either both rows land or neither does.
func (s *Store) CreateGuest(ctx context.Context, user identity.User, session identity.Session) error {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: create guest: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO users (id, kind, created_at, last_seen_at) VALUES (?, ?, ?, ?)",
		user.ID, user.Kind, user.CreatedAt.Unix(), user.LastSeen.Unix()); err != nil {
		return fmt.Errorf("sqlitestore: create guest: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO sessions (id, user_id, created_at, revoked) VALUES (?, ?, ?, 0)",
		session.ID, session.UserID, session.CreatedAt.Unix()); err != nil {
		return fmt.Errorf("sqlitestore: create guest: %w", err)
	}
	return tx.Commit()
}

// sessionColumns is the column list every session read shares, in the
// order scanSession decodes it.
const sessionColumns = `SELECT s.id, s.user_id, s.created_at, s.revoked,
	u.id, u.kind, u.created_at, u.last_seen_at
	FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`

// Session reads one session row joined with its user. An unknown id
// reports identity.ErrUnknownSession.
func (s *Store) Session(ctx context.Context, id string) (identity.Session, identity.User, error) {
	var session identity.Session
	var user identity.User
	var sessionCreated, userCreated, seen int64
	var revoked int
	err := s.db.Reader().QueryRowContext(ctx, sessionColumns, id).
		Scan(&session.ID, &session.UserID, &sessionCreated, &revoked,
			&user.ID, &user.Kind, &userCreated, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Session{}, identity.User{}, fmt.Errorf("sqlitestore: session %s: %w", id, identity.ErrUnknownSession)
	}
	if err != nil {
		return identity.Session{}, identity.User{}, fmt.Errorf("sqlitestore: read session %s: %w", id, err)
	}
	session.CreatedAt = time.Unix(sessionCreated, 0)
	session.Revoked = revoked != 0
	user.CreatedAt = time.Unix(userCreated, 0)
	user.LastSeen = time.Unix(seen, 0)
	return session, user, nil
}

// TouchUser stamps the user's last seen time.
func (s *Store) TouchUser(ctx context.Context, userID string, at time.Time) error {
	if _, err := s.db.Writer().ExecContext(ctx,
		"UPDATE users SET last_seen_at = ? WHERE id = ?", at.Unix(), userID); err != nil {
		return fmt.Errorf("sqlitestore: touch user %s: %w", userID, err)
	}
	return nil
}

// RevokeSession marks one session revoked. An unknown id is not an
// error, because the session is already unusable.
func (s *Store) RevokeSession(ctx context.Context, sessionID string) error {
	if _, err := s.db.Writer().ExecContext(ctx,
		"UPDATE sessions SET revoked = 1 WHERE id = ?", sessionID); err != nil {
		return fmt.Errorf("sqlitestore: revoke session %s: %w", sessionID, err)
	}
	return nil
}

// IdentityHolder reads the user id one provider subject is attached to.
// An unknown pair reports identity.ErrUnknownIdentity.
func (s *Store) IdentityHolder(ctx context.Context, provider, subject string) (string, error) {
	var holder string
	err := s.db.Reader().QueryRowContext(ctx,
		"SELECT user_id FROM identities WHERE provider = ? AND subject = ?",
		provider, subject).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("sqlitestore: identity %s %s: %w", provider, subject, identity.ErrUnknownIdentity)
	}
	if err != nil {
		return "", fmt.Errorf("sqlitestore: read identity: %w", err)
	}
	return holder, nil
}

// PutCode reserves one code send and stores the fresh code row in one
// transaction. The reservation counts both durable windows inside the
// transaction that inserts the row, so concurrent requests serialise on
// the single writer and cannot oversend past the caps. A full window
// reports identity.ErrSendLimited, and the transaction then inserts
// nothing.
func (s *Store) PutCode(ctx context.Context, code identity.SignInCode, caps identity.SendCaps) error {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: put code: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	var address, global int
	if err := tx.QueryRowContext(ctx,
		`SELECT
			(SELECT COUNT(*) FROM sign_in_codes WHERE address_hash = ? AND created_at > ?),
			(SELECT COUNT(*) FROM sign_in_codes WHERE created_at > ?)`,
		code.AddressHash, caps.Since.Unix(), caps.Since.Unix()).Scan(&address, &global); err != nil {
		return fmt.Errorf("sqlitestore: put code: count windows: %w", err)
	}
	if address >= caps.Address || global >= caps.Global {
		return fmt.Errorf("sqlitestore: put code: %w", identity.ErrSendLimited)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE sign_in_codes SET used_at = ? WHERE address_hash = ? AND requesting_session = ? AND used_at = 0",
		code.CreatedAt.Unix(), code.AddressHash, code.RequestingSession); err != nil {
		return fmt.Errorf("sqlitestore: put code: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sign_in_codes
		(id, address_hash, code_hash, requesting_session, expires_at, attempts, used_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		code.ID, code.AddressHash, code.CodeHash, code.RequestingSession,
		code.ExpiresAt.Unix(), code.Attempts, code.CreatedAt.Unix()); err != nil {
		return fmt.Errorf("sqlitestore: put code: %w", err)
	}
	return tx.Commit()
}

// codeColumns is the column list every code read shares, in the order
// scanCode decodes it.
const codeColumns = `SELECT id, address_hash, code_hash, requesting_session,
	expires_at, attempts, used_at, created_at FROM sign_in_codes`

// LiveCode reads the newest live code one address hash and session hold.
// No live row reports identity.ErrUnknownCode.
func (s *Store) LiveCode(ctx context.Context, addressHash, sessionID string) (identity.SignInCode, error) {
	row, err := scanCode(s.db.Reader().QueryRowContext(ctx, codeColumns+`
		WHERE address_hash = ? AND requesting_session = ? AND used_at = 0
		ORDER BY created_at DESC, id DESC LIMIT 1`, addressHash, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return identity.SignInCode{}, fmt.Errorf("sqlitestore: live code: %w", identity.ErrUnknownCode)
	}
	if err != nil {
		return identity.SignInCode{}, err
	}
	return row, nil
}

// CloseCode retires one code row. A later LiveCode skips it.
func (s *Store) CloseCode(ctx context.Context, codeID string, at time.Time) error {
	if _, err := s.db.Writer().ExecContext(ctx,
		"UPDATE sign_in_codes SET used_at = ? WHERE id = ?", at.Unix(), codeID); err != nil {
		return fmt.Errorf("sqlitestore: close code %s: %w", codeID, err)
	}
	return nil
}

// DeleteCode removes one code row entirely. A send that failed never
// delivered, so its row leaves the send ceilings untouched.
func (s *Store) DeleteCode(ctx context.Context, codeID string) error {
	if _, err := s.db.Writer().ExecContext(ctx,
		"DELETE FROM sign_in_codes WHERE id = ?", codeID); err != nil {
		return fmt.Errorf("sqlitestore: delete code %s: %w", codeID, err)
	}
	return nil
}

// RecordCodeAttempt counts one wrong try with an atomic increment, and
// retires the row inside the same write once the wrong tries reach the
// cap, so concurrent guesses cannot collapse onto one count and reopen a
// closed code.
func (s *Store) RecordCodeAttempt(ctx context.Context, codeID string, wrongCap int, at time.Time) error {
	if _, err := s.db.Writer().ExecContext(ctx,
		`UPDATE sign_in_codes SET attempts = attempts + 1,
			used_at = CASE WHEN attempts + 1 >= ? THEN ? ELSE used_at END
			WHERE id = ?`,
		wrongCap, at.Unix(), codeID); err != nil {
		return fmt.Errorf("sqlitestore: record attempt %s: %w", codeID, err)
	}
	return nil
}

// CodeSends counts the code rows one address hash holds since the
// cutoff, with the oldest row's creation time.
func (s *Store) CodeSends(ctx context.Context, addressHash string, since time.Time) (identity.SendWindow, error) {
	return s.sendWindow(ctx,
		"SELECT COUNT(*), MIN(created_at) FROM sign_in_codes WHERE address_hash = ? AND created_at > ?",
		addressHash, since.Unix())
}

// AllCodeSends counts the code rows every address holds since the
// cutoff, with the oldest row's creation time.
func (s *Store) AllCodeSends(ctx context.Context, since time.Time) (identity.SendWindow, error) {
	return s.sendWindow(ctx,
		"SELECT COUNT(*), MIN(created_at) FROM sign_in_codes WHERE created_at > ?",
		since.Unix())
}

// sendWindow runs one window count. An empty window reads a zero time.
func (s *Store) sendWindow(ctx context.Context, query string, args ...any) (identity.SendWindow, error) {
	var count int64
	var oldest sql.NullInt64
	if err := s.db.Reader().QueryRowContext(ctx, query, args...).Scan(&count, &oldest); err != nil {
		return identity.SendWindow{}, fmt.Errorf("sqlitestore: count code sends: %w", err)
	}
	window := identity.SendWindow{Count: count}
	if oldest.Valid {
		window.Oldest = time.Unix(oldest.Int64, 0)
	}
	return window, nil
}

// ResolveSignIn runs one verified sign-in in one transaction. It
// consumes the code while it is still live, attaches or joins the
// identity, opens the fresh session for the target user, and revokes the
// session that asked.
func (s *Store) ResolveSignIn(ctx context.Context, in identity.SignInResolution) (identity.SignInTarget, error) {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() // the transaction already returned its own error
		}
	}()
	if in.CodeID != "" {
		consumed, err := tx.ExecContext(ctx,
			"UPDATE sign_in_codes SET used_at = ? WHERE id = ? AND used_at = 0",
			in.At.Unix(), in.CodeID)
		if err != nil {
			return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
		}
		if n, err := consumed.RowsAffected(); err != nil {
			return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
		} else if n != 1 {
			return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", identity.ErrUnknownCode)
		}
	}
	target, switched, err := resolveIdentity(ctx, tx, in)
	if err != nil {
		return identity.SignInTarget{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO sessions (id, user_id, created_at, revoked) VALUES (?, ?, ?, 0)",
		in.NewSessionID, target, in.At.Unix()); err != nil {
		return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE sessions SET revoked = 1 WHERE id = ?", in.SessionID); err != nil {
		return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return identity.SignInTarget{}, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
	}
	committed = true
	return identity.SignInTarget{UserID: target, SessionID: in.NewSessionID, Switched: switched}, nil
}

// resolveIdentity attaches or joins one identity inside the open
// transaction and returns the target user with its switched flag. A
// fresh subject attaches to the asking user and flips its kind to owner.
// A subject on the asking user keeps. A subject on another user moves to
// that user. A racing attach lands between the read and the insert, so a
// refused insert re-reads the holder and joins it, honouring the same
// conflict rule the caller applied outside the transaction.
func resolveIdentity(ctx context.Context, tx *sql.Tx, in identity.SignInResolution) (string, bool, error) {
	var holder string
	err := tx.QueryRowContext(ctx,
		"SELECT user_id FROM identities WHERE provider = ? AND subject = ?",
		in.Provider, in.Subject).Scan(&holder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("sqlitestore: resolve sign-in: read identity: %w", err)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rowID, err := id.New()
		if err != nil {
			return "", false, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO identities (id, user_id, provider, subject, created_at) VALUES (?, ?, ?, ?, ?)",
			rowID, in.UserID, in.Provider, in.Subject, in.At.Unix())
		if err != nil {
			// A racing sign-in attached this subject first, so the insert
			// collided on the provider subject pair. Join that holder
			// instead of failing, and honour the conflict exactly as a
			// later sign-in would.
			var fresh string
			if rerr := tx.QueryRowContext(ctx,
				"SELECT user_id FROM identities WHERE provider = ? AND subject = ?",
				in.Provider, in.Subject).Scan(&fresh); rerr != nil {
				return "", false, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
			}
			if fresh == in.UserID {
				return in.UserID, false, nil
			}
			if in.GuestData && !in.AllowSwitch {
				return "", false, fmt.Errorf("sqlitestore: resolve sign-in: %w", identity.ErrGuestDataConflict)
			}
			return fresh, true, nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE users SET kind = ? WHERE id = ?", identity.KindOwner, in.UserID); err != nil {
			return "", false, fmt.Errorf("sqlitestore: resolve sign-in: %w", err)
		}
		return in.UserID, false, nil
	case holder == in.UserID:
		return in.UserID, false, nil
	default:
		return holder, true, nil
	}
}

// DeleteUserRows removes one user's code rows, session rows and identity
// rows, then the user row last, in one transaction. Codes go before
// sessions, and sessions and identities go before the user, because each
// names the next.
func (s *Store) DeleteUserRows(ctx context.Context, userID string) error {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: delete user rows %s: %w", userID, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	steps := []struct {
		name string
		sql  string
		arg  any
	}{
		{"code rows", "DELETE FROM sign_in_codes WHERE requesting_session IN (SELECT id FROM sessions WHERE user_id = ?)", userID},
		{"session rows", "DELETE FROM sessions WHERE user_id = ?", userID},
		{"identity rows", "DELETE FROM identities WHERE user_id = ?", userID},
		{"user row", "DELETE FROM users WHERE id = ?", userID},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.sql, step.arg); err != nil {
			return fmt.Errorf("sqlitestore: delete user rows %s: %s: %w", userID, step.name, err)
		}
	}
	return tx.Commit()
}

// scanCode decodes one code row. A zero used stamp reads as the zero
// time, which is how a live code reports itself.
func scanCode(row *sql.Row) (identity.SignInCode, error) {
	var code identity.SignInCode
	var expires, used, created int64
	err := row.Scan(&code.ID, &code.AddressHash, &code.CodeHash, &code.RequestingSession,
		&expires, &code.Attempts, &used, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.SignInCode{}, err
		}
		return identity.SignInCode{}, fmt.Errorf("sqlitestore: read code: %w", err)
	}
	code.ExpiresAt = time.Unix(expires, 0)
	code.UsedAt = unixToTime(used)
	code.CreatedAt = time.Unix(created, 0)
	return code, nil
}

// unixToTime converts a stored stamp. A zero stamp reads as the zero
// time, because the column uses zero as absent.
func unixToTime(stamp int64) time.Time {
	if stamp == 0 {
		return time.Time{}
	}
	return time.Unix(stamp, 0)
}
