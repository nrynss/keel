package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nrynss/keel/cost"
)

// ErrGrantRepeated reports a grant whose key was already posted. A non-empty
// key posts once, so this error means the pool already holds that grant and
// the second call changed nothing. An application that posts one grant per
// allowance window keys it by the window's start, and treats this error as
// the window already being funded.
var ErrGrantRepeated = errors.New("sqlitestore: grant key already posted")

// Grant adds amount credit to the pool this store's budget spends from, and
// the credit lapses at expiresAt. The balance a reservation draws on is the
// sum of the unexpired grants minus what spend has drawn from them. Spend
// draws from the grant that expires soonest first, so a short grant is
// consumed before a longer one and its undrawn remainder lapses with it.
// Expiry is judged by the store against its own clock at read and spend
// time, so a restart cannot resurrect lapsed credit.
//
// key makes the grant idempotent. A non-empty key posts once: a second call
// with the same key reports ErrGrantRepeated and changes nothing. An
// application that refills an allowance every window keys the grant by the
// window's start, and a restart or a double post cannot fund a window twice.
// A unique index over the key holds the guarantee in the schema, so it
// survives every reopen. An empty key never repeats.
//
// A grant is never deleted, because deleting one would let its key post
// again. A grant whose expiry has passed is recorded and funds nothing. The
// method reports ErrInvalid when amount is below zero, because a grant only
// adds credit, and a correction posts through the ledger as a charge. A
// cancelled context stores nothing.
func (s *Store) Grant(ctx context.Context, amount cost.Price, expiresAt time.Time, key string) error {
	if amount < 0 {
		return fmt.Errorf("sqlitestore: grant %d: %w: amount must not be negative", amount, ErrInvalid)
	}
	now := s.now()
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	// The check and the insert share one transaction on the single write
	// connection, so two callers cannot both pass the check. The unique
	// index stays behind them as the schema's own guarantee.
	if key != "" {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM cost_grant WHERE grant_key = ?`, key).Scan(&one)
		if err == nil {
			return fmt.Errorf("sqlitestore: grant %q: %w", key, ErrGrantRepeated)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlitestore: grant: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_grant (amount_nd, drawn_nd, grant_key, expires_at, created_at)
			VALUES (?, 0, ?, ?, ?)`,
		int64(amount), key, expiresAt.UnixMilli(), now.UnixMilli()); err != nil {
		return fmt.Errorf("sqlitestore: grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: grant: %w", err)
	}
	return nil
}

// Balance returns the credit the store's unexpired grants still fund, minus
// what spend has drawn from them. A store with no grants answers zero,
// because its budget then draws on its ceiling alone and Remaining reports
// that bound. It reports an error matching cost.ErrOverflow when the exact
// balance leaves the int64 range.
func (s *Store) Balance(ctx context.Context) (cost.Price, error) {
	return grantBalance(ctx, s.db.Reader(), s.now())
}

// grantBalance sums what the unexpired grants still fund. An empty or absent
// grant table answers zero.
func grantBalance(ctx context.Context, q querier, now time.Time) (cost.Price, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT amount_nd, drawn_nd FROM cost_grant WHERE expires_at > ?`, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: read grants: %w", err)
	}
	defer rows.Close() // the cursor is drained before the caller returns
	var total cost.Price
	for rows.Next() {
		var amount, drawn int64
		if err := rows.Scan(&amount, &drawn); err != nil {
			return 0, fmt.Errorf("sqlitestore: scan grants: %w", err)
		}
		live, err := subPrice(cost.Price(amount), cost.Price(drawn))
		if err != nil {
			return 0, err
		}
		total, err = addPrice(total, live)
		if err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlitestore: read grants: %w", err)
	}
	return total, nil
}

// drawGrants places actual into the grants that expire soonest first, so a
// short grant is consumed before a longer one. A positive draw stops at each
// grant's remaining amount. A negative one returns credit only as far as
// zero, so a correction never grows a grant past what was posted. A draw
// past the live pool places what fits, and the rest books as spend with
// nothing behind it, exactly as a settle past the ceiling still books. The
// caller holds the write transaction.
func drawGrants(ctx context.Context, tx *sql.Tx, now time.Time, actual cost.Price) error {
	if actual == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, amount_nd, drawn_nd FROM cost_grant WHERE expires_at > ?
			ORDER BY expires_at ASC, id ASC`,
		now.UnixMilli())
	if err != nil {
		return fmt.Errorf("read grants: %w", err)
	}
	// The rows are read into a slice before any update writes, so one
	// statement never walks and writes the same table at once.
	type grantRow struct {
		id            int64
		amount, drawn cost.Price
	}
	var live []grantRow
	for rows.Next() {
		var g grantRow
		var amount, drawn int64
		if err := rows.Scan(&g.id, &amount, &drawn); err != nil {
			_ = rows.Close() // the error return already ends the walk
			return fmt.Errorf("scan grants: %w", err)
		}
		g.amount, g.drawn = cost.Price(amount), cost.Price(drawn)
		live = append(live, g)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close() // the error return already ends the walk
		return fmt.Errorf("read grants: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read grants: %w", err)
	}
	remaining := actual
	for i := range live {
		if remaining == 0 {
			break
		}
		g := &live[i]
		var take cost.Price
		if remaining > 0 {
			room, err := subPrice(g.amount, g.drawn)
			if err != nil {
				return err
			}
			if room <= 0 {
				continue
			}
			take = room
			if take > remaining {
				take = remaining
			}
		} else {
			// remaining is negative here, so take is a negative delta that
			// un-draws credit. A stored draw never passes the largest
			// int64, so the sum below cannot overflow and the negations
			// are safe.
			covers, err := addPrice(g.drawn, remaining)
			if err != nil {
				return err
			}
			if covers >= 0 {
				take = remaining // un-draws exactly the overdraft
			} else {
				take = -g.drawn // empties this draw and moves on
			}
			if take == 0 {
				continue
			}
		}
		drawn, err := addPrice(g.drawn, take)
		if err != nil {
			return err
		}
		remaining, err = subPrice(remaining, take)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cost_grant SET drawn_nd = ? WHERE id = ?`, int64(drawn), g.id); err != nil {
			return fmt.Errorf("draw grant: %w", err)
		}
	}
	return nil
}
