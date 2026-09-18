// Package sqlitestore persists runtime flags over SQLite.
//
// The package owns one schema. It applies it through the sqlite package's
// migration runner under its own namespace, so the ledger records what ran
// and a second open on the same database changes nothing.
//
// Reads go through to the database every time. A flag check is one indexed
// primary-key lookup, so a cache would add an invalidation problem for no
// measurable gain. An operator's flip must be visible to the next read
// without a restart.
//
// A value row records the kind its writer declared. A read whose
// declaration names a different kind fails rather than guessing, so a
// boolean flag can never silently return text. A name with no row reads as
// the declaration's default, which keeps a flag nobody has set from acting
// like a stored false.
//
// This package is one of the few allowed to import the SQLite driver. The
// flag declarations themselves stay in the flag package.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/nrynss/keel/flag"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each namespace keeps its own ledger.
const schemaNamespace = "flag"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned for an argument this package cannot honour, such as
// a nil database.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database. It must not be nil, and this package never
	// closes it.
	DB *sqlite.DB

	// Namespace overrides the migration ledger namespace. Empty means the
	// one this package owns.
	Namespace string

	// Now supplies the clock that stamps flag changes. Nil means
	// time.Now.
	Now func() time.Time

	// Logger receives the open record. Nil means slog.Default.
	Logger *slog.Logger
}

// Store persists flag values over one database. Create it with Open, because
// the zero value has no database. Store is safe for concurrent use, and
// satisfies flag.Store.
type Store struct {
	db  *sqlite.DB
	now func() time.Time
	log *slog.Logger
}

// Open applies the flag schema to cfg.DB and returns the store. A cancelled
// context stops the open and no store comes back, but the schema and its
// migration ledger row may already have landed. Those bytes are inert, so a
// caller may retry the open and a second open on the same file is
// unaffected.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: DB must not be nil", ErrInvalid)
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "sqlitestore: opened flags", "namespace", namespace)
	return &Store{db: cfg.DB, now: now, log: logger}, nil
}

// Bool reads f through to the database. A name with no row reads as the
// declaration's default with the zero time. A row written under a different
// kind reports an error matching flag.ErrKind.
func (s *Store) Bool(ctx context.Context, f flag.Bool) (bool, time.Time, error) {
	if f.Name == "" {
		return false, time.Time{}, fmt.Errorf("sqlitestore: bool flag: %w: name must not be empty", flag.ErrInvalid)
	}
	var kind, value string
	var changed int64
	err := s.db.Reader().QueryRowContext(ctx,
		`SELECT kind, value, changed_at FROM flag_value WHERE name = ?`, f.Name,
	).Scan(&kind, &value, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return f.Default, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("sqlitestore: read flag %q: %w", f.Name, err)
	}
	if kind != kindBool {
		return false, time.Time{}, fmt.Errorf("sqlitestore: flag %q: %w: stored %s", f.Name, flag.ErrKind, kind)
	}
	got, err := parseBool(value)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("sqlitestore: flag %q: %w", f.Name, err)
	}
	return got, time.Unix(0, changed).UTC(), nil
}

// SetBool stores value for f and reports the change time. A cancelled
// context stops the write, so the flag keeps its old value.
func (s *Store) SetBool(ctx context.Context, f flag.Bool, value bool) (time.Time, error) {
	if f.Name == "" {
		return time.Time{}, fmt.Errorf("sqlitestore: bool flag: %w: name must not be empty", flag.ErrInvalid)
	}
	return s.write(ctx, f.Name, kindBool, boolText(value))
}

// Text reads f through to the database. A name with no row reads as the
// declaration's default with the zero time. A row written under a different
// kind reports an error matching flag.ErrKind.
func (s *Store) Text(ctx context.Context, f flag.Text) (string, time.Time, error) {
	if f.Name == "" {
		return "", time.Time{}, fmt.Errorf("sqlitestore: text flag: %w: name must not be empty", flag.ErrInvalid)
	}
	var kind, value string
	var changed int64
	err := s.db.Reader().QueryRowContext(ctx,
		`SELECT kind, value, changed_at FROM flag_value WHERE name = ?`, f.Name,
	).Scan(&kind, &value, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return f.Default, time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sqlitestore: read flag %q: %w", f.Name, err)
	}
	if kind != kindText {
		return "", time.Time{}, fmt.Errorf("sqlitestore: flag %q: %w: stored %s", f.Name, flag.ErrKind, kind)
	}
	return value, time.Unix(0, changed).UTC(), nil
}

// SetText stores value for f and reports the change time. A cancelled
// context stops the write, so the flag keeps its old value.
func (s *Store) SetText(ctx context.Context, f flag.Text, value string) (time.Time, error) {
	if f.Name == "" {
		return time.Time{}, fmt.Errorf("sqlitestore: text flag: %w: name must not be empty", flag.ErrInvalid)
	}
	return s.write(ctx, f.Name, kindText, value)
}

// write upserts one value row and returns the change time the store stamped.
// The upsert updates both value and kind, so a name redeclared under the
// other kind takes the new kind on its next set.
func (s *Store) write(ctx context.Context, name, kind, value string) (time.Time, error) {
	changed := s.now().UTC().UnixNano()
	_, err := s.db.Writer().ExecContext(ctx, `INSERT INTO flag_value (name, kind, value, changed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, value = excluded.value,
			changed_at = excluded.changed_at`,
		name, kind, value, changed)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlitestore: write flag %q: %w", name, err)
	}
	return time.Unix(0, changed).UTC(), nil
}

// kinds a value row may carry.
const (
	kindBool = "bool"
	kindText = "text"
)

// boolText renders a boolean the way the store persists it.
func boolText(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// parseBool reads a persisted boolean back.
func parseBool(s string) (bool, error) {
	switch s {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, flag.ErrKind
}
