package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nrynss/keel/sqlite"
)

// beginWrite opens a write transaction on db's writer pool. The pool begins
// IMMEDIATE, so a held lock is waited out inside BEGIN, and SQLite reports
// SQLITE_BUSY there even when the caller's context ended the wait. The
// context error is joined in, so errors.Is still tells a cancelled or timed
// out write from a contended one.
func beginWrite(ctx context.Context, db *sqlite.DB) (*sql.Tx, error) {
	tx, err := db.Writer().BeginTx(ctx, nil)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
			return nil, errors.Join(cerr, err)
		}
		return nil, err
	}
	return tx, nil
}
