// Package sqlitestore keeps revision documents, revisions, pins and
// proposals in SQLite.
//
// The package owns one schema, applied through the sqlite migration runner
// under its own namespace, so a second open changes nothing. Every write runs
// in one write transaction, so the head check of a commit and the commit
// itself cannot interleave with another writer. A cancelled context stops a
// call before it commits. The call fails and stores nothing, so a caller may
// retry it.
//
// This package is one of the few allowed to import the SQLite driver.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"time"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/revision"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns.
const schemaNamespace = "revision"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned for a config this package cannot honour, such as a
// nil database.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database. It must not be nil, and this package never
	// closes it.
	DB *sqlite.DB

	// Logger receives the open record. Nil means slog.Default.
	Logger *slog.Logger

	// Now reads the clock every stamp comes from. Nil means time.Now.
	Now func() time.Time
}

// Store keeps revision rows over one database. Create it with Open, because
// the zero value has no database. Store is safe for concurrent use, and
// satisfies revision.Store.
type Store struct {
	db  *sqlite.DB
	now func() time.Time
}

// Open applies the revision schema to cfg.DB and returns the store. A
// cancelled context stops the open and no store comes back, but the schema
// may already have landed, and a retry is unaffected.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: DB must not be nil", ErrInvalid)
	}
	schema, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	if err := sqlite.Migrate(ctx, cfg.DB, schemaNamespace, schema); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger.InfoContext(ctx, "sqlitestore: opened revisions", "namespace", schemaNamespace)
	return &Store{db: cfg.DB, now: now}, nil
}

// querier is what a read needs, so one helper serves a pool and a
// transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkBody refuses an empty author or a body that is not valid JSON.
func checkBody(op string, body []byte, author string) error {
	if author == "" {
		return fmt.Errorf("sqlitestore: %s: %w: author must not be empty", op, revision.ErrInvalid)
	}
	if !json.Valid(body) {
		return fmt.Errorf("sqlitestore: %s: %w: body is not valid JSON", op, revision.ErrInvalid)
	}
	return nil
}

// checkDoc refuses an empty document id.
func checkDoc(op, doc string) error {
	if doc == "" {
		return fmt.Errorf("sqlitestore: %s: %w: document must not be empty", op, revision.ErrInvalid)
	}
	return nil
}

// Create stores a new document owned by owner, with body as revision 1. An
// id the store already holds reports revision.ErrExists. An empty id, owner
// or author, or a body that is not valid JSON, reports revision.ErrInvalid.
func (s *Store) Create(ctx context.Context, doc, owner string, body []byte, author string) (revision.Revision, error) {
	if err := checkDoc("create", doc); err != nil {
		return revision.Revision{}, err
	}
	if owner == "" {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create: %w: owner must not be empty", revision.ErrInvalid)
	}
	if err := checkBody("create", body, author); err != nil {
		return revision.Revision{}, err
	}
	body = slices.Clone(body) // the returned revision must not alias the caller's buffer
	now := s.now()
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create %s: %w", doc, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	res, err := tx.ExecContext(ctx,
		`INSERT INTO revision_document (id, owner, head, created_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (id) DO NOTHING`, doc, owner, now.UnixNano())
	if err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create %s: %w", doc, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create %s: %w", doc, err)
	} else if n == 0 {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create %s: %w", doc, revision.ErrExists)
	}
	rev := revision.Revision{Document: doc, Number: 1, Author: author, Body: body, CommittedAt: now}
	if err := insertRevision(ctx, tx, rev); err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create %s: %w", doc, err)
	}
	if err := tx.Commit(); err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: create %s: %w", doc, err)
	}
	return rev, nil
}

// Document reads the document's record. An unknown id reports
// revision.ErrUnknownDocument.
func (s *Store) Document(ctx context.Context, doc string) (revision.Document, error) {
	d, err := loadDocument(ctx, s.db.Reader(), doc)
	if err != nil {
		return revision.Document{}, fmt.Errorf("sqlitestore: document %s: %w", doc, err)
	}
	return d, nil
}

// loadDocument reads one document row through q.
func loadDocument(ctx context.Context, q querier, doc string) (revision.Document, error) {
	var d revision.Document
	var created int64
	err := q.QueryRowContext(ctx,
		`SELECT id, owner, head, created_at FROM revision_document WHERE id = ?`, doc).
		Scan(&d.ID, &d.Owner, &d.Head, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return revision.Document{}, revision.ErrUnknownDocument
	}
	if err != nil {
		return revision.Document{}, err
	}
	d.CreatedAt = time.Unix(0, created)
	return d, nil
}

// insertRevision writes one revision row and moves the document head to it.
func insertRevision(ctx context.Context, tx *sql.Tx, r revision.Revision) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO revision_entry (document, number, parent, author, proposal, body, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Document, r.Number, r.Parent, r.Author, r.Proposal, r.Body, r.CommittedAt.UnixNano()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE revision_document SET head = ? WHERE id = ?`, r.Number, r.Document)
	return err
}

// commitOn checks parent against the head inside tx and writes the next
// revision. A stale parent reports a *revision.ConflictError.
func commitOn(ctx context.Context, tx *sql.Tx, doc string, parent int64, body []byte, author, proposal string, now time.Time) (revision.Revision, error) {
	d, err := loadDocument(ctx, tx, doc)
	if err != nil {
		return revision.Revision{}, err
	}
	if parent != d.Head {
		return revision.Revision{}, &revision.ConflictError{Document: doc, Parent: parent, Head: d.Head}
	}
	rev := revision.Revision{Document: doc, Number: d.Head + 1, Parent: parent, Author: author,
		Proposal: proposal, Body: body, CommittedAt: now}
	if err := insertRevision(ctx, tx, rev); err != nil {
		return revision.Revision{}, err
	}
	return rev, nil
}

// Commit stores body as the next revision when parent is the document's
// head. A stale parent reports a *revision.ConflictError carrying the head,
// matching revision.ErrConflict, and stores nothing. An unknown id reports
// revision.ErrUnknownDocument.
func (s *Store) Commit(ctx context.Context, doc string, parent int64, body []byte, author string) (revision.Revision, error) {
	if err := checkDoc("commit", doc); err != nil {
		return revision.Revision{}, err
	}
	if err := checkBody("commit", body, author); err != nil {
		return revision.Revision{}, err
	}
	body = slices.Clone(body) // the returned revision must not alias the caller's buffer
	now := s.now()
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: commit %s: %w", doc, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	rev, err := commitOn(ctx, tx, doc, parent, body, author, "", now)
	if err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: commit %s: %w", doc, err)
	}
	if err := tx.Commit(); err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: commit %s: %w", doc, err)
	}
	return rev, nil
}

// History lists the document's revisions newest first. Bodies stay nil, so
// a long history lists cheaply; Get reads one body. An unknown id reports
// revision.ErrUnknownDocument.
func (s *Store) History(ctx context.Context, doc string) ([]revision.Revision, error) {
	if _, err := loadDocument(ctx, s.db.Reader(), doc); err != nil {
		return nil, fmt.Errorf("sqlitestore: history %s: %w", doc, err)
	}
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT number, parent, author, proposal, committed_at FROM revision_entry
		WHERE document = ? ORDER BY number DESC`, doc)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: history %s: %w", doc, err)
	}
	defer rows.Close() // the cursor is drained before return
	var out []revision.Revision
	for rows.Next() {
		r := revision.Revision{Document: doc}
		var at int64
		if err := rows.Scan(&r.Number, &r.Parent, &r.Author, &r.Proposal, &at); err != nil {
			return nil, fmt.Errorf("sqlitestore: history %s: %w", doc, err)
		}
		r.CommittedAt = time.Unix(0, at)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: history %s: %w", doc, err)
	}
	return out, nil
}

// Get reads one revision with its body. A number the document does not hold,
// never committed or pruned, reports revision.ErrUnknownRevision. An unknown
// id reports revision.ErrUnknownDocument.
func (s *Store) Get(ctx context.Context, doc string, rev int64) (revision.Revision, error) {
	r, err := loadRevision(ctx, s.db.Reader(), doc, rev, true)
	if err != nil {
		return revision.Revision{}, fmt.Errorf("sqlitestore: get %s@%d: %w", doc, rev, err)
	}
	return r, nil
}

// loadRevision reads one revision through q, telling an unknown document
// from an unknown number.
func loadRevision(ctx context.Context, q querier, doc string, rev int64, body bool) (revision.Revision, error) {
	r := revision.Revision{Document: doc, Number: rev}
	var at int64
	err := q.QueryRowContext(ctx,
		`SELECT parent, author, proposal, body, committed_at FROM revision_entry
		WHERE document = ? AND number = ?`, doc, rev).
		Scan(&r.Parent, &r.Author, &r.Proposal, &r.Body, &at)
	if errors.Is(err, sql.ErrNoRows) {
		if _, derr := loadDocument(ctx, q, doc); derr != nil {
			return revision.Revision{}, derr
		}
		return revision.Revision{}, revision.ErrUnknownRevision
	}
	if err != nil {
		return revision.Revision{}, err
	}
	if !body {
		r.Body = nil
	}
	r.CommittedAt = time.Unix(0, at)
	return r, nil
}

// Pin marks rev as an approved take under name. Pinning the same name on the
// same revision again is a no-op that returns the existing pin. A name that
// marks another revision reports revision.ErrPinTaken. A number the document
// does not hold reports revision.ErrUnknownRevision.
func (s *Store) Pin(ctx context.Context, doc string, rev int64, name string) (revision.Pin, error) {
	if name == "" {
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin: %w: name must not be empty", revision.ErrInvalid)
	}
	now := s.now()
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin %s@%d: %w", doc, rev, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := loadRevision(ctx, tx, doc, rev, false); err != nil {
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin %s@%d: %w", doc, rev, err)
	}
	var held, at int64
	err = tx.QueryRowContext(ctx,
		`SELECT number, pinned_at FROM revision_pin WHERE document = ? AND name = ?`, doc, name).Scan(&held, &at)
	switch {
	case err == nil && held == rev:
		return revision.Pin{Name: name, Revision: rev, PinnedAt: time.Unix(0, at)}, nil
	case err == nil:
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin %s@%d: %q marks %d: %w", doc, rev, name, held, revision.ErrPinTaken)
	case !errors.Is(err, sql.ErrNoRows):
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin %s@%d: %w", doc, rev, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO revision_pin (document, name, number, pinned_at) VALUES (?, ?, ?, ?)`,
		doc, name, rev, now.UnixNano()); err != nil {
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin %s@%d: %w", doc, rev, err)
	}
	if err := tx.Commit(); err != nil {
		return revision.Pin{}, fmt.Errorf("sqlitestore: pin %s@%d: %w", doc, rev, err)
	}
	return revision.Pin{Name: name, Revision: rev, PinnedAt: now}, nil
}

// Unpin removes the pin name, so its revision may be pruned again. A name
// the document holds no pin under reports revision.ErrUnknownPin.
func (s *Store) Unpin(ctx context.Context, doc, name string) error {
	res, err := s.db.Writer().ExecContext(ctx,
		`DELETE FROM revision_pin WHERE document = ? AND name = ?`, doc, name)
	if err != nil {
		return fmt.Errorf("sqlitestore: unpin %s %q: %w", doc, name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: unpin %s %q: %w", doc, name, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlitestore: unpin %s %q: %w", doc, name, revision.ErrUnknownPin)
	}
	return nil
}

// Pins lists the document's pins ordered by name. An unknown id reports
// revision.ErrUnknownDocument.
func (s *Store) Pins(ctx context.Context, doc string) ([]revision.Pin, error) {
	if _, err := loadDocument(ctx, s.db.Reader(), doc); err != nil {
		return nil, fmt.Errorf("sqlitestore: pins %s: %w", doc, err)
	}
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT name, number, pinned_at FROM revision_pin WHERE document = ? ORDER BY name`, doc)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: pins %s: %w", doc, err)
	}
	defer rows.Close() // the cursor is drained before return
	var out []revision.Pin
	for rows.Next() {
		var p revision.Pin
		var at int64
		if err := rows.Scan(&p.Name, &p.Revision, &at); err != nil {
			return nil, fmt.Errorf("sqlitestore: pins %s: %w", doc, err)
		}
		p.PinnedAt = time.Unix(0, at)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: pins %s: %w", doc, err)
	}
	return out, nil
}

// Prune drops every revision older than the newest keep, and reports how
// many it dropped. A pinned revision and the head are never dropped,
// whatever keep says. A keep below one reports revision.ErrInvalid. Open
// proposals made on a dropped revision stay open, and an accept still
// answers with the conflict rule.
func (s *Store) Prune(ctx context.Context, doc string, keep int) (int, error) {
	if keep < 1 {
		return 0, fmt.Errorf("sqlitestore: prune %s: %w: keep must be at least 1", doc, revision.ErrInvalid)
	}
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: prune %s: %w", doc, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	d, err := loadDocument(ctx, tx, doc)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: prune %s: %w", doc, err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM revision_entry WHERE document = ? AND number <= ?
		AND NOT EXISTS (SELECT 1 FROM revision_pin p WHERE p.document = revision_entry.document AND p.number = revision_entry.number)`,
		doc, d.Head-int64(keep))
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: prune %s: %w", doc, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: prune %s: %w", doc, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlitestore: prune %s: %w", doc, err)
	}
	return int(n), nil
}

// Propose stores a proposal made on parent. It is not a revision and changes
// no head. A parent above the head, or below 1, reports
// revision.ErrUnknownRevision. A parent below the head is accepted, because
// the proposal may be stale, and Accept judges it.
func (s *Store) Propose(ctx context.Context, doc string, parent int64, body []byte, author string) (revision.Proposal, error) {
	if err := checkDoc("propose", doc); err != nil {
		return revision.Proposal{}, err
	}
	if err := checkBody("propose", body, author); err != nil {
		return revision.Proposal{}, err
	}
	body = slices.Clone(body) // the returned proposal must not alias the caller's buffer
	pid, err := id.New()
	if err != nil {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: propose %s: %w", doc, err)
	}
	now := s.now()
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: propose %s: %w", doc, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	d, err := loadDocument(ctx, tx, doc)
	if err != nil {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: propose %s: %w", doc, err)
	}
	if parent < 1 || parent > d.Head {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: propose %s on %d: %w", doc, parent, revision.ErrUnknownRevision)
	}
	p := revision.Proposal{ID: pid, Document: doc, Parent: parent, Author: author, Body: body,
		State: revision.ProposalOpen, CreatedAt: now}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO revision_proposal (id, document, parent, author, body, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, doc, parent, author, body, string(p.State), now.UnixNano()); err != nil {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: propose %s: %w", doc, err)
	}
	if err := tx.Commit(); err != nil {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: propose %s: %w", doc, err)
	}
	return p, nil
}

// proposalColumns is the select list scanProposal reads.
const proposalColumns = `id, document, parent, author, body, state, number, decided_by, created_at, decided_at`

// scanner is one row a proposal scans from.
type scanner interface {
	Scan(dest ...any) error
}

// scanProposal decodes one proposal row.
func scanProposal(row scanner) (revision.Proposal, error) {
	var p revision.Proposal
	var state string
	var created, decided int64
	if err := row.Scan(&p.ID, &p.Document, &p.Parent, &p.Author, &p.Body, &state,
		&p.Revision, &p.DecidedBy, &created, &decided); err != nil {
		return revision.Proposal{}, err
	}
	p.State = revision.ProposalState(state)
	p.CreatedAt = time.Unix(0, created)
	if decided != 0 {
		p.DecidedAt = time.Unix(0, decided)
	}
	return p, nil
}

// loadProposal reads one proposal through q.
func loadProposal(ctx context.Context, q querier, pid string) (revision.Proposal, error) {
	p, err := scanProposal(q.QueryRowContext(ctx,
		`SELECT `+proposalColumns+` FROM revision_proposal WHERE id = ?`, pid))
	if errors.Is(err, sql.ErrNoRows) {
		return revision.Proposal{}, revision.ErrUnknownProposal
	}
	return p, err
}

// Proposal reads one proposal. An unknown id reports
// revision.ErrUnknownProposal.
func (s *Store) Proposal(ctx context.Context, pid string) (revision.Proposal, error) {
	p, err := loadProposal(ctx, s.db.Reader(), pid)
	if err != nil {
		return revision.Proposal{}, fmt.Errorf("sqlitestore: proposal %s: %w", pid, err)
	}
	return p, nil
}

// Proposals lists the document's proposals newest first, in every state. An
// unknown id reports revision.ErrUnknownDocument.
func (s *Store) Proposals(ctx context.Context, doc string) ([]revision.Proposal, error) {
	if _, err := loadDocument(ctx, s.db.Reader(), doc); err != nil {
		return nil, fmt.Errorf("sqlitestore: proposals %s: %w", doc, err)
	}
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT `+proposalColumns+` FROM revision_proposal WHERE document = ?
		ORDER BY created_at DESC, id DESC`, doc)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: proposals %s: %w", doc, err)
	}
	defer rows.Close() // the cursor is drained before return
	var out []revision.Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: proposals %s: %w", doc, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: proposals %s: %w", doc, err)
	}
	return out, nil
}

// decide claims an open proposal inside a write transaction and hands it to
// apply. A closed proposal reports revision.ErrClosed.
func (s *Store) decide(ctx context.Context, op, pid, author string, apply func(tx *sql.Tx, p revision.Proposal, now time.Time) error) error {
	if author == "" {
		return fmt.Errorf("sqlitestore: %s: %w: author must not be empty", op, revision.ErrInvalid)
	}
	now := s.now()
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("sqlitestore: %s %s: %w", op, pid, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	p, err := loadProposal(ctx, tx, pid)
	if err != nil {
		return fmt.Errorf("sqlitestore: %s %s: %w", op, pid, err)
	}
	if p.State != revision.ProposalOpen {
		return fmt.Errorf("sqlitestore: %s %s: %s: %w", op, pid, p.State, revision.ErrClosed)
	}
	if err := apply(tx, p, now); err != nil {
		return fmt.Errorf("sqlitestore: %s %s: %w", op, pid, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: %s %s: %w", op, pid, err)
	}
	return nil
}

// Accept commits an open proposal's body as the next revision, authored by
// the person who accepted, under the commit conflict rule. A proposal whose
// parent is no longer the head reports a *revision.ConflictError and stays
// open, so the proposer can propose again on the head. A closed proposal
// reports revision.ErrClosed.
func (s *Store) Accept(ctx context.Context, pid, author string) (revision.Revision, error) {
	var rev revision.Revision
	err := s.decide(ctx, "accept", pid, author, func(tx *sql.Tx, p revision.Proposal, now time.Time) error {
		r, err := commitOn(ctx, tx, p.Document, p.Parent, p.Body, author, p.ID, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE revision_proposal SET state = ?, number = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
			string(revision.ProposalAccepted), r.Number, author, now.UnixNano(), p.ID); err != nil {
			return err
		}
		rev = r
		return nil
	})
	if err != nil {
		return revision.Revision{}, err
	}
	return rev, nil
}

// Reject closes an open proposal without applying it. A closed proposal
// reports revision.ErrClosed.
func (s *Store) Reject(ctx context.Context, pid, author string) (revision.Proposal, error) {
	var out revision.Proposal
	err := s.decide(ctx, "reject", pid, author, func(tx *sql.Tx, p revision.Proposal, now time.Time) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE revision_proposal SET state = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
			string(revision.ProposalRejected), author, now.UnixNano(), p.ID); err != nil {
			return err
		}
		p.State, p.DecidedBy, p.DecidedAt = revision.ProposalRejected, author, now
		out = p
		return nil
	})
	if err != nil {
		return revision.Proposal{}, err
	}
	return out, nil
}

// ForgetOwner removes every document of owner, with all its revisions, pins
// and proposals. Erasure outranks a pin, so pinned revisions go too. An owner
// with no documents reports an error matching revision.ErrUnknownDocument,
// so an erase.Target maps it to erase.ErrGone and a repeated erase answers
// the same way. A cancelled context removes nothing.
func (s *Store) ForgetOwner(ctx context.Context, owner string) error {
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("sqlitestore: forget owner: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	// Pins reference revisions and revisions reference documents, and the
	// cascades take both, but the explicit deletes keep the erase whole on
	// a handle whose foreign keys were turned off.
	for _, stmt := range []string{
		`DELETE FROM revision_pin WHERE document IN (SELECT id FROM revision_document WHERE owner = ?)`,
		`DELETE FROM revision_proposal WHERE document IN (SELECT id FROM revision_document WHERE owner = ?)`,
		`DELETE FROM revision_entry WHERE document IN (SELECT id FROM revision_document WHERE owner = ?)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, owner); err != nil {
			return fmt.Errorf("sqlitestore: forget owner: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM revision_document WHERE owner = ?`, owner)
	if err != nil {
		return fmt.Errorf("sqlitestore: forget owner: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: forget owner: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("sqlitestore: forget owner: owner %q: %w", owner, revision.ErrUnknownDocument)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: forget owner: %w", err)
	}
	return nil
}

// The compiler holds Store to the interface the revision package declares.
var _ revision.Store = (*Store)(nil)
