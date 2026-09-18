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

// KeyedBudget bounds the spend of several owners over one durable store. It
// sits over the same tables, the same global ceiling and the same expiry the
// store keeps, and adds a ceiling per owner, so an owner that exhausts its
// share never touches another owner's headroom while the global ceiling
// still bounds them all together. The empty owner key names the unkeyed
// budget Store itself keeps, so a keyed budget refuses it. Create one with
// NewKeyedBudget. A KeyedBudget is safe for concurrent use, as Store is.
type KeyedBudget struct {
	store *Store
}

// NewKeyedBudget returns a keyed budget over store. The store must come from
// Open, because the keyed budget reads and writes the schema Open applies.
func NewKeyedBudget(store *Store) *KeyedBudget {
	return &KeyedBudget{store: store}
}

// emptyOwner reports the empty owner key, which names the unkeyed budget the
// store itself keeps rather than an owner of a keyed one.
func emptyOwner(op string) error {
	return fmt.Errorf("sqlitestore: %s: %w: the owner key must not be empty", op, ErrInvalid)
}

// SetLimit gives owner a ceiling of its own, or replaces the ceiling it has.
// The row is created on the first call and updated on every later one, so
// the owner's booked spend survives the change, as the store's own ceiling
// does across an open. It reports an error matching cost.ErrNegativeLimit
// when limit is below zero, and ErrInvalid when owner is empty. A cancelled
// context stops the write, so no ceiling is set.
func (k *KeyedBudget) SetLimit(ctx context.Context, owner string, limit cost.Price) error {
	if owner == "" {
		return emptyOwner("set owner limit")
	}
	if limit < 0 {
		return fmt.Errorf("sqlitestore: set owner limit %d: %w", limit, cost.ErrNegativeLimit)
	}
	const query = `INSERT INTO cost_owner_budget (owner, limit_nd, spent_nd) VALUES (?, ?, 0)
		ON CONFLICT(owner) DO UPDATE SET limit_nd = excluded.limit_nd`
	if _, err := k.store.db.Writer().ExecContext(ctx, query, owner, int64(limit)); err != nil {
		return fmt.Errorf("sqlitestore: set owner limit: %w", err)
	}
	return nil
}

// Reserve commits estimate against owner's ceiling and against the store's
// global ceiling, and returns the hold. It reports ErrInvalid when owner is
// empty, an error matching cost.ErrUnknownOwner when no ceiling was set for
// owner, and an error matching cost.ErrOverBudget when either bound would be
// passed, committing nothing then. It reports an error matching
// cost.ErrNegativeEstimate for a negative estimate. The checks and the
// insert share one transaction, so concurrent callers never overspend either
// bound. The hold expires after the configured TTL, as every hold on the
// store does. A cancelled context stops the reserve, so no hold is taken.
func (k *KeyedBudget) Reserve(ctx context.Context, owner string, estimate cost.Price) (Reservation, error) {
	if owner == "" {
		return Reservation{}, emptyOwner("reserve")
	}
	if estimate < 0 {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve %d: %w", estimate, cost.ErrNegativeEstimate)
	}
	now := k.store.now()
	tx, err := k.store.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: purge expired: %w", err)
	}
	global, err := loadState(ctx, tx, now)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	globalRemaining, err := remainingOf(global)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	if estimate > globalRemaining {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve %s over remaining %s: %w",
			estimate, globalRemaining, cost.ErrOverBudget)
	}
	owned, err := loadOwner(ctx, tx, owner, now)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	ownerRemaining, err := remainingOf(owned)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	if estimate > ownerRemaining {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve %s over owner remaining %s: %w",
			estimate, ownerRemaining, cost.ErrOverBudget)
	}
	reservationID, err := id.New()
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	expires := now.Add(k.store.reservationTTL)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_reservation (id, owner, amount_nd, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		reservationID, owner, int64(estimate), expires.UnixMilli(), now.UnixMilli()); err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	return Reservation{ID: reservationID, Amount: estimate, ExpiresAt: expires}, nil
}

// Settle books the price owner's call actually cost and frees its hold. It
// reports ErrInvalid when owner is empty, an error matching
// cost.ErrUnknownOwner when no ceiling was set for owner, and an error
// matching cost.ErrOverflow when actual would push either booked spend past
// the int64 range, committing nothing then. The booking lands on the global
// pool and on the owner's own account, because both ceilings read the same
// fact. A hold that already expired or belongs to another owner frees
// nothing, and that is not an error. A cancelled context stops the settle,
// so nothing is booked.
func (k *KeyedBudget) Settle(ctx context.Context, owner string, r Reservation, actual cost.Price) error {
	if owner == "" {
		return emptyOwner("settle")
	}
	tx, err := k.store.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	var spent, ownerSpent int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spent_nd FROM cost_budget WHERE id = 1`).Scan(&spent); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT spent_nd FROM cost_owner_budget WHERE owner = ?`, owner).Scan(&ownerSpent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlitestore: settle: owner %q: %w", owner, cost.ErrUnknownOwner)
		}
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	nextSpent, err := addPrice(cost.Price(spent), actual)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	nextOwnerSpent, err := addPrice(cost.Price(ownerSpent), actual)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cost_budget SET spent_nd = ? WHERE id = 1`, int64(nextSpent)); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cost_owner_budget SET spent_nd = ? WHERE owner = ?`, int64(nextOwnerSpent), owner); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if r.ID != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM cost_reservation WHERE id = ? AND owner = ?`, r.ID, owner); err != nil {
			return fmt.Errorf("sqlitestore: settle: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	return nil
}

// Release frees a hold owner never spent. It reports ErrInvalid when owner
// is empty and an error matching cost.ErrUnknownOwner when no ceiling was
// set for owner. A hold that already expired or was already settled frees
// nothing, and that is not an error, as Store.Release does. A cancelled
// context stops the release, so the hold stays until its expiry frees it.
func (k *KeyedBudget) Release(ctx context.Context, owner string, r Reservation) error {
	if r.ID == "" {
		return nil
	}
	if owner == "" {
		return emptyOwner("release")
	}
	var one int
	err := k.store.db.Reader().QueryRowContext(ctx,
		`SELECT 1 FROM cost_owner_budget WHERE owner = ?`, owner).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlitestore: release: owner %q: %w", owner, cost.ErrUnknownOwner)
	}
	if err != nil {
		return fmt.Errorf("sqlitestore: release: %w", err)
	}
	if _, err := k.store.db.Writer().ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE id = ? AND owner = ?`, r.ID, owner); err != nil {
		return fmt.Errorf("sqlitestore: release: %w", err)
	}
	return nil
}

// Remaining returns the headroom owner still has under its own ceiling. It
// is the owner's ceiling minus what its settles booked minus its unexpired
// holds. It reports ErrInvalid when owner is empty, an error matching
// cost.ErrUnknownOwner when no ceiling was set for owner, and an error
// matching cost.ErrOverflow when that difference leaves the int64 range. A
// reserve can still fail with an error matching cost.ErrOverBudget when the
// global ceiling binds first.
func (k *KeyedBudget) Remaining(ctx context.Context, owner string) (cost.Price, error) {
	if owner == "" {
		return 0, emptyOwner("remaining")
	}
	owned, err := loadOwner(ctx, k.store.db.Reader(), owner, k.store.now())
	if err != nil {
		return 0, err
	}
	return remainingOf(owned)
}

// loadOwner reads one owner's ceiling, booked spend and unexpired holds. It
// counts only reservations whose expiry is still in the future, as loadState
// does, and reports an error matching cost.ErrUnknownOwner when no ceiling
// was set for owner.
func loadOwner(ctx context.Context, q querier, owner string, now time.Time) (budgetState, error) {
	var state budgetState
	var limit, spent int64
	err := q.QueryRowContext(ctx,
		`SELECT limit_nd, spent_nd FROM cost_owner_budget WHERE owner = ?`, owner).Scan(&limit, &spent)
	if errors.Is(err, sql.ErrNoRows) {
		return budgetState{}, fmt.Errorf("sqlitestore: owner %q: %w", owner, cost.ErrUnknownOwner)
	}
	if err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read owner budget: %w", err)
	}
	state.limit = cost.Price(limit)
	state.spent = cost.Price(spent)
	var reserved int64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_nd), 0) FROM cost_reservation WHERE owner = ? AND expires_at > ?`,
		owner, now.UnixMilli()).Scan(&reserved); err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read owner reservations: %w", err)
	}
	state.reserved = cost.Price(reserved)
	return state, nil
}
