package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/id"
)

// ErrRunAbandoned reports that the runner of a shared quote did not reach
// an outcome, so a waiter has no result of its own to trust. The claim was
// freed, nothing was booked, and the caller may run the quote again.
var ErrRunAbandoned = errors.New("sqlitestore: quote run abandoned before it finished")

// The states a quote row moves through. A quote opens when it is issued,
// is claimed while its runner holds the reservation, and is done once the
// outcome is recorded. A done quote never runs again.
const (
	quoteOpen    = "open"
	quoteClaimed = "claimed"
	quoteDone    = "done"
)

// quoteChargeKind is the kind every charge the quote path records carries.
// The ref carries the quote id, so a report can split quoted spend by
// action through it.
const quoteChargeKind = "quoted"

// sweepQuotesSQL removes every quote past its life. An open or done quote
// dies at its confirm deadline. A claimed one dies when its claim lapses,
// because a lapsed claim means the runner is gone.
const sweepQuotesSQL = `DELETE FROM cost_quote
	WHERE (state <> ? AND expires_at <= ?) OR (state = ? AND claim_expires_at <= ?)`

// quoteRow is one cost_quote row in the shape the quote path reads.
type quoteRow struct {
	id              string
	owner           string
	price           cost.Price
	denomination    string
	state           string
	outcome         cost.Price
	outcomeMeasured bool
	claimExpiresAt  int64
	expiresAt       time.Time
	createdAt       time.Time
}

// live reports whether the quote row is still inside its confirm window.
func (r quoteRow) live(now time.Time) bool {
	return r.expiresAt.UnixMilli() > now.UnixMilli()
}

// quoteClaim is a live claim. It carries the reservation the claim took
// and the quote facts the settle and the abandon need.
type quoteClaim struct {
	reservationID string
	quoteID       string
	owner         string
	price         cost.Price
	expiresAt     time.Time
	createdAt     time.Time
}

// quoteFlight is one in-process quote run that waiters share. The leader
// writes the outcome and closes done, and the close is the hand-off that
// makes the outcome visible to every waiter.
type quoteFlight struct {
	done  chan struct{}
	usage cost.Usage
	err   error
}

// Quote states a price for one action and returns it with an id. The
// owner names the account a later Run reserves against, estimate is the
// confirmed amount in the store's denomination, and ttl is the confirm
// window. The quote holds no reservation, so a quote nobody confirms
// holds no budget. Expired quotes are swept on the way, so a steady
// stream of quotes keeps the table clean. It reports ErrInvalid when
// owner is empty or ttl is not positive, and an error matching
// cost.ErrNegativeEstimate when estimate is below zero. A cancelled
// context stores nothing.
func (s *Store) Quote(ctx context.Context, owner string, estimate cost.Price, ttl time.Duration) (cost.Quote, error) {
	if owner == "" {
		return cost.Quote{}, emptyOwner("quote")
	}
	if estimate < 0 {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote %d: %w", estimate, cost.ErrNegativeEstimate)
	}
	if ttl <= 0 {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote: %w: ttl must be positive", ErrInvalid)
	}
	now := s.now()
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := sweepQuotes(ctx, tx, now); err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote: %w", err)
	}
	quoteID, err := id.New()
	if err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote: %w", err)
	}
	expires := now.Add(ttl)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_quote (id, owner, price_nd, denomination, state, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		quoteID, owner, int64(estimate), s.denom.Name, quoteOpen, expires.UnixMilli(), now.UnixMilli()); err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: quote: %w", err)
	}
	return cost.Quote{ID: quoteID, Owner: owner, Price: estimate, Denomination: s.denom, ExpiresAt: expires}, nil
}

// Run carries one confirmed quote out. It claims the quote and takes the
// owner's reservation in one transaction, so the claim is durable before
// the work runs and a crash or a retry between claim and work can never
// charge twice. It then runs the work and settles the measured price,
// the same shape the meter settles a call with, and records the charge in
// the ledger under the quote's id. A work that reports no measurement
// settles at the quoted price. Every observed failure frees the hold and
// returns the quote to open, so a retry of a failed action runs again and
// a retry of a finished one cannot.
//
// A second Run with the same id returns the first outcome and charges
// nothing. A concurrent Run in this process waits for the one that
// claimed and shares its outcome. A Run from another process finds the
// live claim and refuses with the quote_pending code, and finds the
// recorded outcome once the claim finished.
//
// Before it claims a live quote, Run calls the configured Reprice and
// compares the price it reports with the quoted one. A move past the
// tolerance refuses with the quote_price_moved code and a fresh quote at
// the current price, so the client can re-confirm before anything spends.
// An expired quote refuses with the quote_expired code and a fresh quote
// the same way. A quote no row backs refuses with quote_unknown. Every
// refusal is a cost.QuoteRefusal whose Code a handler passes to the wire
// envelope unchanged.
//
// A claim whose runner vanishes holds the quote until the reservation TTL
// lapses, then the sweep frees it, and the id reads as unknown. Work that
// outlives the TTL can therefore be asked to run again, so the TTL should
// outlast the slowest quoted action. A panic in work frees the claim and
// continues past Run with its own value and stack, as it does past the
// meter. A cancelled context stops the claim, the reprice and the work.
// Once the work succeeds, the settle and the outcome record run on a
// context detached from the caller's cancellation, so a late cancel
// cannot leave a paid action unbooked. Run reports the settled outcome
// with a nil error in that case, because the action ran and the booking
// stands. It reports ErrInvalid when quoteID is empty.
func (s *Store) Run(ctx context.Context, quoteID string, work cost.Work) (usage cost.Usage, err error) {
	if quoteID == "" {
		return cost.Usage{}, fmt.Errorf("sqlitestore: run quote: %w: the quote id must not be empty", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return cost.Usage{}, err
	}
	flight, leader := s.registerFlight(quoteID)
	if !leader {
		<-flight.done
		return flight.usage, flight.err
	}
	completed := false
	defer func() {
		if !completed {
			// The leader never reached its outcome, so a panic is
			// unwinding through it. The panic continues past Run, the
			// claim below is freed by the abandon, and the waiters
			// receive their own error instead of a silent success.
			usage = cost.Usage{}
			err = fmt.Errorf("sqlitestore: run quote %s: %w", quoteID, ErrRunAbandoned)
		}
		s.finishFlight(quoteID, flight, usage, err)
	}()
	usage, err = s.runLead(ctx, quoteID, work)
	completed = true
	return usage, err
}

// registerFlight joins the in-process single-flight table for quoteID. It
// reports the flight and whether this caller leads it. The leader runs the
// work and every other caller waits on its outcome.
func (s *Store) registerFlight(quoteID string) (*quoteFlight, bool) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	if flight, ok := s.flights[quoteID]; ok {
		return flight, false
	}
	flight := &quoteFlight{done: make(chan struct{})}
	s.flights[quoteID] = flight
	return flight, true
}

// finishFlight records the leader's outcome, removes the flight from the
// table and releases every waiter. It runs on every leader exit path,
// including a panic, so a waiter never blocks forever.
func (s *Store) finishFlight(quoteID string, flight *quoteFlight, usage cost.Usage, err error) {
	s.flightMu.Lock()
	if s.flights[quoteID] == flight {
		delete(s.flights, quoteID)
	}
	s.flightMu.Unlock()
	flight.usage = usage
	flight.err = err
	close(flight.done)
}

// runLead drives one quote from the read that prices it to the claim that
// spends it. The caller is the flight's leader, so no other caller in
// this process is here at the same time.
func (s *Store) runLead(ctx context.Context, quoteID string, work cost.Work) (cost.Usage, error) {
	// The read phase refuses what no claim can fix, and prices the
	// action through Reprice outside any transaction, so app I/O never
	// sits inside a claim.
	row, err := readQuoteRow(ctx, s.db.Reader(), quoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return cost.Usage{}, unknownQuote(quoteID)
	}
	if err != nil {
		return cost.Usage{}, fmt.Errorf("sqlitestore: run quote %s: %w", quoteID, err)
	}
	if row.denomination != s.denom.Name {
		return cost.Usage{}, s.quoteDenomination(row)
	}
	now := s.now()
	switch {
	case row.state == quoteDone:
		return cost.Usage{Price: row.outcome, Measured: row.outcomeMeasured}, nil
	case row.state == quoteClaimed && row.claimExpiresAt > now.UnixMilli():
		return cost.Usage{}, pendingQuote(quoteID)
	case row.state == quoteClaimed:
		// The claim lapsed, so the runner that held it is gone. The
		// sweep frees the row, and the id reads as unknown.
		return cost.Usage{}, unknownQuote(quoteID)
	}
	current, repriced := row.price, false
	if row.live(now) && s.reprice != nil {
		current, err = s.reprice(ctx, row.owner)
		if err != nil {
			return cost.Usage{}, fmt.Errorf("sqlitestore: run quote %s: reprice: %w", quoteID, err)
		}
		if current < 0 {
			return cost.Usage{}, fmt.Errorf("sqlitestore: run quote %s: reprice %d: %w", quoteID, current, cost.ErrNegativeEstimate)
		}
		repriced = true
		moved, err := priceMoved(current, row.price, s.quoteTolerance)
		if err != nil {
			return cost.Usage{}, fmt.Errorf("sqlitestore: run quote %s: %w", quoteID, err)
		}
		if moved {
			fresh, err := s.freshQuote(ctx, row, current)
			if err != nil {
				return cost.Usage{}, err
			}
			return cost.Usage{}, &cost.QuoteRefusal{
				Code:  cost.CodeQuotePriceMoved,
				Quote: fresh,
				Err:   fmt.Errorf("sqlitestore: quote %s moved from %s to %s: %w", quoteID, row.price, current, cost.ErrQuotePriceMoved),
			}
		}
	}
	return s.runClaimed(ctx, quoteID, work, current, repriced)
}

// runClaimed claims the quote, runs the work and settles. The claim and
// the reservation share one transaction, so the claim is durable before
// the work runs.
func (s *Store) runClaimed(ctx context.Context, quoteID string, work cost.Work, current cost.Price, repriced bool) (usage cost.Usage, err error) {
	claim, done, doneUsage, err := s.claimQuote(ctx, quoteID, current, repriced)
	if err != nil {
		return cost.Usage{}, err
	}
	if done {
		return doneUsage, nil
	}
	// From here the claim holds the owner's budget. An observed failure
	// or a panic frees the hold and returns the quote to open, even when
	// the caller's context is gone, so money does not wait on a live
	// request.
	live := true
	defer func() {
		if !live {
			return
		}
		abandonCtx := context.WithoutCancel(ctx)
		if aerr := s.abandonQuote(abandonCtx, claim); aerr != nil {
			err = errors.Join(err, aerr)
			s.log.WarnContext(abandonCtx, "sqlitestore: quote claim abandoned without release", "quote", quoteID, "err", aerr)
		}
	}()
	usage, err = work(ctx)
	if err != nil {
		return cost.Usage{}, err
	}
	price := claim.price
	if usage.Measured {
		if usage.Price < 0 {
			return cost.Usage{}, fmt.Errorf("sqlitestore: run quote %s: measured price %d: %w", quoteID, usage.Price, cost.ErrNegativePrice)
		}
		price = usage.Price
	}
	// The work succeeded, so the booking cannot depend on the caller's
	// context. The settle drops the cancellation and keeps the values,
	// because a paid action that finished must always book.
	settleCtx := context.WithoutCancel(ctx)
	if err := s.settleQuote(settleCtx, claim, price, usage.Measured); err != nil {
		return cost.Usage{}, err
	}
	live = false
	return cost.Usage{Price: price, Measured: usage.Measured}, nil
}

// claimQuote moves the quote to claimed and takes the owner's reservation
// in one transaction. The transaction re-reads the row, because the read
// phase ran outside it, and a quote another process moved on is refused
// the same way the read phase would refuse it. It reports the claim, or
// done with the recorded outcome when the quote already finished, or a
// refusal.
func (s *Store) claimQuote(ctx context.Context, quoteID string, current cost.Price, repriced bool) (claim quoteClaim, done bool, usage cost.Usage, err error) {
	now := s.now()
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	row, err := readQuoteRow(ctx, tx, quoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return quoteClaim{}, false, cost.Usage{}, unknownQuote(quoteID)
	}
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	if row.denomination != s.denom.Name {
		return quoteClaim{}, false, cost.Usage{}, s.quoteDenomination(row)
	}
	// The sweep runs after the read, so a quote this run refuses as
	// expired is refused as expired rather than swept into unknown. The
	// expired holds go with it, so the bounds below read only live ones.
	if _, err := sweepQuotes(ctx, tx, now); err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	if _, err := purgeExpiredHolds(ctx, tx, now); err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	switch {
	case row.state == quoteDone:
		if err := tx.Commit(); err != nil {
			return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
		}
		return quoteClaim{}, true, cost.Usage{Price: row.outcome, Measured: row.outcomeMeasured}, nil
	case row.state == quoteClaimed && row.claimExpiresAt > now.UnixMilli():
		if err := tx.Commit(); err != nil {
			return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
		}
		return quoteClaim{}, false, cost.Usage{}, pendingQuote(quoteID)
	case row.state == quoteClaimed:
		if err := tx.Commit(); err != nil {
			return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
		}
		return quoteClaim{}, false, cost.Usage{}, unknownQuote(quoteID)
	case !row.live(now):
		fresh, err := s.insertFreshQuote(ctx, tx, row, refusalPrice(row, current, repriced), now)
		if err != nil {
			return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
		}
		if err := tx.Commit(); err != nil {
			return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
		}
		return quoteClaim{}, false, cost.Usage{}, &cost.QuoteRefusal{
			Code:  cost.CodeQuoteExpired,
			Quote: fresh,
			Err:   fmt.Errorf("sqlitestore: quote %s expired at %s: %w", quoteID, row.expiresAt.Format(time.RFC3339), cost.ErrQuoteExpired),
		}
	}
	// The quote is open and inside its window. The claim checks the same
	// bounds a keyed reserve checks, so a quoted action never takes more
	// than the owner's share or the pool's headroom allows.
	owned, err := loadOwner(ctx, tx, row.owner, now, s.windowSince(now))
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	ownerRemaining, err := remainingOf(owned)
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	if row.price > ownerRemaining {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf(
			"sqlitestore: claim quote %s: reserve %s over owner remaining %s: %w",
			quoteID, row.price, ownerRemaining, cost.ErrOverBudget)
	}
	global, err := s.loadGlobal(ctx, tx, now)
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	globalRemaining, err := remainingOf(global)
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	if row.price > globalRemaining {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf(
			"sqlitestore: claim quote %s: reserve %s over remaining %s: %w",
			quoteID, row.price, globalRemaining, cost.ErrOverBudget)
	}
	reservationID, err := id.New()
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	until := now.Add(s.reservationTTL)
	result, err := tx.ExecContext(ctx,
		`UPDATE cost_quote SET state = ?, reservation_id = ?, claim_expires_at = ?
			WHERE id = ? AND state = ?`,
		quoteClaimed, reservationID, until.UnixMilli(), quoteID, quoteOpen)
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	if updated != 1 {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: state changed during the claim", quoteID)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_reservation (id, owner, amount_nd, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		reservationID, row.owner, int64(row.price), until.UnixMilli(), now.UnixMilli()); err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	if err := tx.Commit(); err != nil {
		return quoteClaim{}, false, cost.Usage{}, fmt.Errorf("sqlitestore: claim quote %s: %w", quoteID, err)
	}
	return quoteClaim{
		reservationID: reservationID,
		quoteID:       quoteID,
		owner:         row.owner,
		price:         row.price,
		expiresAt:     row.expiresAt,
		createdAt:     row.createdAt,
	}, false, cost.Usage{}, nil
}

// settleQuote books the settled price and records the outcome in one
// transaction, so a crash between booking and recording leaves nothing
// behind and a retry starts clean. The booking mirrors the keyed settle:
// the global pool and the owner's account book the spend, the settle
// history grows, the grants draw their share and the hold is freed. The
// charge lands in the ledger under the quote's id, and the quote row
// moves to done. The upsert heals the row a sweep took while the work ran
// past its claim, so the outcome survives either way.
func (s *Store) settleQuote(ctx context.Context, claim quoteClaim, settled cost.Price, measured bool) error {
	now := s.now()
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	var spent int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spent_nd FROM cost_budget WHERE id = 1`).Scan(&spent); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	next, err := addPrice(cost.Price(spent), settled)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cost_budget SET spent_nd = ? WHERE id = 1`, int64(next)); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	var ownerSpent int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spent_nd FROM cost_owner_budget WHERE owner = ?`, claim.owner).Scan(&ownerSpent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlitestore: settle quote %s: owner %q: %w", claim.quoteID, claim.owner, cost.ErrUnknownOwner)
		}
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	nextOwner, err := addPrice(cost.Price(ownerSpent), settled)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cost_owner_budget SET spent_nd = ? WHERE owner = ?`, int64(nextOwner), claim.owner); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_settle (owner, amount_nd, created_at) VALUES (?, ?, ?)`,
		claim.owner, int64(settled), now.UnixMilli()); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if err := drawGrants(ctx, tx, now, settled); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE id = ?`, claim.reservationID); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_charge (kind, units, unit_price, ref, denomination, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
		quoteChargeKind, 1, int64(settled), claim.quoteID, s.denom.Name, now.UnixMilli()); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_quote (id, owner, price_nd, denomination, state, outcome_nd, outcome_measured,
			reservation_id, claim_expires_at, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?)
			ON CONFLICT(id) DO UPDATE SET state = excluded.state,
				outcome_nd = excluded.outcome_nd,
				outcome_measured = excluded.outcome_measured,
				reservation_id = '',
				claim_expires_at = 0`,
		claim.quoteID, claim.owner, int64(claim.price), s.denom.Name, quoteDone,
		int64(settled), measuredInt(measured), claim.expiresAt.UnixMilli(), claim.createdAt.UnixMilli()); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: settle quote %s: %w", claim.quoteID, err)
	}
	return nil
}

// abandonQuote frees a live claim after an observed failure or a panic.
// The hold is released and the quote returns to open, so the id stays
// runnable within its confirm window and nothing was booked. It runs on a
// context that survives the caller's cancellation, because freeing money
// must not wait on a live request. A quote the sweep already took, or a
// hold that already lapsed, frees nothing and that is not an error.
func (s *Store) abandonQuote(ctx context.Context, claim quoteClaim) error {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: abandon quote %s: %w", claim.quoteID, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE id = ?`, claim.reservationID); err != nil {
		return fmt.Errorf("sqlitestore: abandon quote %s: %w", claim.quoteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cost_quote SET state = ?, reservation_id = '', claim_expires_at = 0
			WHERE id = ? AND state = ?`, quoteOpen, claim.quoteID, quoteClaimed); err != nil {
		return fmt.Errorf("sqlitestore: abandon quote %s: %w", claim.quoteID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: abandon quote %s: %w", claim.quoteID, err)
	}
	return nil
}

// freshQuote issues the fresh quote a refusal carries. It is a real open
// quote, so a client can confirm it with a Run and no second round trip.
// Its confirm window is the one the refused quote had.
func (s *Store) freshQuote(ctx context.Context, old quoteRow, price cost.Price) (cost.Quote, error) {
	now := s.now()
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: fresh quote: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := sweepQuotes(ctx, tx, now); err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: fresh quote: %w", err)
	}
	fresh, err := s.insertFreshQuote(ctx, tx, old, price, now)
	if err != nil {
		return cost.Quote{}, err
	}
	if err := tx.Commit(); err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: fresh quote: %w", err)
	}
	return fresh, nil
}

// insertFreshQuote writes the fresh quote row inside the caller's
// transaction. The confirm window is the refused quote's own, so a client
// gets the same time to confirm that the original had.
func (s *Store) insertFreshQuote(ctx context.Context, tx *sql.Tx, old quoteRow, price cost.Price, now time.Time) (cost.Quote, error) {
	quoteID, err := id.New()
	if err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: fresh quote: %w", err)
	}
	expires := now.Add(old.expiresAt.Sub(old.createdAt))
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_quote (id, owner, price_nd, denomination, state, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		quoteID, old.owner, int64(price), s.denom.Name, quoteOpen, expires.UnixMilli(), now.UnixMilli()); err != nil {
		return cost.Quote{}, fmt.Errorf("sqlitestore: fresh quote: %w", err)
	}
	return cost.Quote{ID: quoteID, Owner: old.owner, Price: price, Denomination: s.denom, ExpiresAt: expires}, nil
}

// sweepQuotes removes every expired quote inside the caller's
// transaction. It runs the statement the house sweeps reservations with,
// so no scheduler and no caller code are needed.
func sweepQuotes(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, sweepQuotesSQL,
		quoteClaimed, now.UnixMilli(), quoteClaimed, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep quotes: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep quotes: %w", err)
	}
	return removed, nil
}

// sweepQuotesAtOpen removes every expired quote before the store serves
// its first caller, so a restart never carries dead quotes forward. It is
// the statement the write paths run, on the writer pool directly.
func (s *Store) sweepQuotesAtOpen(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.Writer().ExecContext(ctx, sweepQuotesSQL,
		quoteClaimed, now.UnixMilli(), quoteClaimed, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep quotes: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep quotes: %w", err)
	}
	return removed, nil
}

// purgeExpiredHolds removes every reservation whose expiry has passed, so
// the claim bounds read only live holds. It is the sweep the reserve
// paths run.
func purgeExpiredHolds(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE expires_at <= ?`, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: purge reservations: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: purge reservations: %w", err)
	}
	return removed, nil
}

// readQuoteRow loads one quote row. It runs on the reader pool and inside
// a write transaction alike, because both satisfy the query shape it
// needs.
func readQuoteRow(ctx context.Context, q querier, quoteID string) (quoteRow, error) {
	var row quoteRow
	var price, outcome, outcomeMeasured, expiresAt, createdAt int64
	err := q.QueryRowContext(ctx,
		`SELECT id, owner, price_nd, denomination, state, outcome_nd, outcome_measured,
			claim_expires_at, expires_at, created_at
		FROM cost_quote WHERE id = ?`, quoteID).Scan(
		&row.id, &row.owner, &price, &row.denomination, &row.state, &outcome,
		&outcomeMeasured, &row.claimExpiresAt, &expiresAt, &createdAt)
	if err != nil {
		return quoteRow{}, err
	}
	row.price = cost.Price(price)
	row.outcome = cost.Price(outcome)
	row.outcomeMeasured = outcomeMeasured != 0
	row.expiresAt = time.UnixMilli(expiresAt)
	row.createdAt = time.UnixMilli(createdAt)
	return row, nil
}

// priceMoved reports whether current left the tolerance window around
// quoted. The window reaches exactly to the tolerance, so a move of the
// tolerance itself still proceeds. It reports cost.ErrOverflow when the
// difference leaves the int64 range, and Run returns that error as a
// plain failure, not as a refusal with a code and a fresh quote.
func priceMoved(current, quoted, tolerance cost.Price) (bool, error) {
	diff, err := subPrice(current, quoted)
	if err != nil {
		return true, err
	}
	return diff > tolerance || diff < -tolerance, nil
}

// refusalPrice picks the price a fresh quote carries. A price Reprice
// reported wins, because it is the current one, and the quoted price
// stands in when no reprice ran.
func refusalPrice(row quoteRow, current cost.Price, repriced bool) cost.Price {
	if repriced {
		return current
	}
	return row.price
}

// unknownQuote refuses a quote id no row backs.
func unknownQuote(quoteID string) error {
	return &cost.QuoteRefusal{
		Code: cost.CodeQuoteUnknown,
		Err:  fmt.Errorf("sqlitestore: quote %s: %w", quoteID, cost.ErrQuoteUnknown),
	}
}

// pendingQuote refuses a quote another runner still claims.
func pendingQuote(quoteID string) error {
	return &cost.QuoteRefusal{
		Code: cost.CodeQuotePending,
		Err:  fmt.Errorf("sqlitestore: quote %s: %w", quoteID, cost.ErrQuotePending),
	}
}

// quoteDenomination refuses a quote row stored under a denomination the
// store no longer speaks. Open refuses a file stored under another unit,
// so this guards the row itself and refuses before anything books.
func (s *Store) quoteDenomination(row quoteRow) error {
	return fmt.Errorf("sqlitestore: quote %s in %q over a %q store: %w",
		row.id, row.denomination, s.denom.Name, cost.ErrDenominationMismatch)
}

// measuredInt stores the measured flag as the one integer the row keeps.
func measuredInt(measured bool) int64 {
	if measured {
		return 1
	}
	return 0
}
