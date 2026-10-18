package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nrynss/keel/cost"
)

// Account returns the store's unkeyed budget as a cost.ContextAccount, so
// cost.NewContextMeter can drive the durable budget. The meter then owns the
// reserve, the settle, the release on every failure and panic, the settle
// once per pair, and the booking that still lands when the call's context
// is cancelled after the work ran. Pass the store as the meter's sink too,
// so the charge lands in the same file:
//
//	meter, err := cost.NewContextMeter(store.Account(), store)
//
// Every Hold the account hands out carries the reservation's ID, amount and
// expiry, exactly the fields Reserve returns in a Reservation. The account
// reserves, settles and releases through Reserve, Settle, SettleOnce and
// Release, so it meets every rule those methods document. A nil store
// returns a nil account, so cost.NewContextMeter refuses it with
// cost.ErrNilAccount rather than failing at the first call.
func (s *Store) Account() cost.ContextAccount {
	if s == nil {
		return nil
	}
	return storeAccount{store: s}
}

// storeAccount drives the store's unkeyed budget through the ContextAccount
// shape. A Hold and a Reservation have the same fields, so each converts to
// the other without loss.
type storeAccount struct {
	store *Store
}

// A storeAccount satisfies cost.ContextAccount.
var _ cost.ContextAccount = storeAccount{}

// Denomination reports the store's unit.
func (a storeAccount) Denomination() cost.Denomination {
	return a.store.Denomination()
}

// Reserve takes a hold on the store's budget.
func (a storeAccount) Reserve(ctx context.Context, estimate cost.Price) (cost.Hold, error) {
	r, err := a.store.Reserve(ctx, estimate)
	if err != nil {
		return cost.Hold{}, err
	}
	return cost.Hold(r), nil
}

// Settle books actual and frees the hold.
func (a storeAccount) Settle(ctx context.Context, h cost.Hold, actual cost.Price) error {
	return a.store.Settle(ctx, Reservation(h), actual)
}

// SettleOnce books actual at most once per kind and reference pair and
// frees the hold.
func (a storeAccount) SettleOnce(ctx context.Context, h cost.Hold, actual cost.Price, kind, ref string) (bool, error) {
	return a.store.SettleOnce(ctx, Reservation(h), actual, kind, ref)
}

// Release frees the hold.
func (a storeAccount) Release(ctx context.Context, h cost.Hold) error {
	return a.store.Release(ctx, Reservation(h))
}

// Owner returns owner's share of the keyed budget as a cost.ContextAccount,
// so cost.NewContextMeter can drive one owner's durable budget. The account
// is bound to owner, so every reserve, settle and release it makes counts
// against that owner's ceiling and against the store's global ceiling, and
// a hold it hands out can only be settled or released as owner's. It
// reports ErrInvalid when owner is empty and an error matching
// cost.ErrUnknownOwner when no ceiling was set for owner, so a mistyped
// owner stops before a call runs. The check reads the file once, and a
// cancelled context stops it. An owner whose ceiling a later ForgetOwner
// removes makes the account's next reserve refuse with
// cost.ErrUnknownOwner, as KeyedBudget.Reserve does.
//
// The account meets every rule KeyedBudget's own methods document. In
// particular the once pair table is keyed by kind and reference alone, so a
// pair one owner's meter settled also stops another owner's meter from
// booking it again.
func (k *KeyedBudget) Owner(ctx context.Context, owner string) (cost.ContextAccount, error) {
	if owner == "" {
		return nil, emptyOwner("owner account")
	}
	var one int
	err := k.store.db.Reader().QueryRowContext(ctx,
		`SELECT 1 FROM `+k.store.tables.ownerBudget+` WHERE owner = ?`, owner).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlitestore: owner account: owner %q: %w", owner, cost.ErrUnknownOwner)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: owner account: %w", err)
	}
	return ownerAccount{keyed: k, owner: owner}, nil
}

// ownerAccount drives one owner of a durable keyed budget through the
// ContextAccount shape. It stays a value because it holds a shared pointer
// beside a key, so copies share one budget.
type ownerAccount struct {
	keyed *KeyedBudget
	owner string
}

// An ownerAccount satisfies cost.ContextAccount.
var _ cost.ContextAccount = ownerAccount{}

// Denomination reports the store's unit, which every owner shares.
func (a ownerAccount) Denomination() cost.Denomination {
	return a.keyed.store.Denomination()
}

// Reserve takes a hold against the owner's ceiling and the global one.
func (a ownerAccount) Reserve(ctx context.Context, estimate cost.Price) (cost.Hold, error) {
	r, err := a.keyed.Reserve(ctx, a.owner, estimate)
	if err != nil {
		return cost.Hold{}, err
	}
	return cost.Hold(r), nil
}

// Settle books actual against the owner and frees the hold.
func (a ownerAccount) Settle(ctx context.Context, h cost.Hold, actual cost.Price) error {
	return a.keyed.Settle(ctx, a.owner, Reservation(h), actual)
}

// SettleOnce books actual against the owner at most once per kind and
// reference pair and frees the hold.
func (a ownerAccount) SettleOnce(ctx context.Context, h cost.Hold, actual cost.Price, kind, ref string) (bool, error) {
	return a.keyed.SettleOnce(ctx, a.owner, Reservation(h), actual, kind, ref)
}

// Release frees the owner's hold.
func (a ownerAccount) Release(ctx context.Context, h cost.Hold) error {
	return a.keyed.Release(ctx, a.owner, Reservation(h))
}
