package cost

import (
	"context"
	"time"
)

// Hold is one outstanding reservation a ContextAccount handed out. Reserve
// returns it, and the caller passes the same value back to exactly one of
// Settle, SettleOnce or Release. The account alone gives ID its meaning, so
// a caller never builds a Hold of its own and never edits one it was given.
// The fields are exported so a durable account can carry its row identity
// and expiry through the value, and so a test can read what a reserve
// committed.
type Hold struct {
	// ID identifies the hold within its account. An account that needs no
	// identity, such as the in-memory Budget, leaves it empty.
	ID string

	// Amount is the estimate Reserve committed.
	Amount Price

	// ExpiresAt is the instant after which a durable account stops counting
	// the hold. The zero time means the hold never lapses on its own, which
	// is how every in-memory account reports it.
	ExpiresAt time.Time
}

// ContextAccount is the budget side a meter drives. Every spending method
// takes the call's context, and Reserve hands back a Hold the meter returns
// to the settle or the release, so a durable budget that stores each hold
// as its own row backs a meter as directly as an in-memory one. The
// cost/sqlitestore Store offers one through its Account method, and its
// KeyedBudget offers one per owner through Owner. The in-memory Budget and
// KeyedBudget offer the same shape through Budget.Account and
// KeyedBudget.OwnerAccount. Every method must be safe for concurrent use,
// because concurrent Calls drive one account.
//
// The methods mean exactly what the Budget methods of the same names mean,
// with the hold in place of the reserved amount. NewContextMeter builds the
// Meter that drives one, and the Meter owns the settle rules: it releases on
// every failure, settles once per pair under Once, and settles and releases
// on a context detached from the call's cancellation.
type ContextAccount interface {
	// Denomination reports the unit the account's prices count. The meter
	// stamps every charge it records with it, so a report never shows a
	// unit the account does not speak.
	Denomination() Denomination
	// Reserve commits estimate against the account and returns the hold.
	// It fails when the estimate would pass the account's ceiling, and a
	// failed reserve holds nothing.
	Reserve(ctx context.Context, estimate Price) (Hold, error)
	// Settle books the price the call actually cost and frees the hold.
	Settle(ctx context.Context, h Hold, actual Price) error
	// SettleOnce books the price the call actually cost at most once per
	// kind and reference pair, and frees the hold. A pair that settled
	// before books nothing, frees the hold, and reports booked false. A
	// settle without a reference reports an error matching
	// ErrEmptyReference and frees nothing, because an empty reference
	// would collapse a kind's once settles onto one pair.
	SettleOnce(ctx context.Context, h Hold, actual Price, kind, ref string) (booked bool, err error)
	// Release frees a hold the call never spent. An error means the hold
	// may still stand until the account's own expiry frees it. A durable
	// account that names each hold frees nothing for a hold that is
	// already gone, and that is not an error. An in-memory account counts
	// its holds as one total and cannot tell, which is why each hold goes
	// back exactly once.
	Release(ctx context.Context, h Hold) error
}

// Account returns the budget as a ContextAccount, for NewContextMeter or any
// code written against the context shape. The account counts against this
// budget, so its holds and its bookings show in Spent, Reserved and
// Remaining exactly as those made through the budget's own methods do. The
// context is not consulted, because an in-memory booking does no I/O, and
// every Hold the account returns carries an empty ID and the zero expiry.
func (b *Budget) Account() ContextAccount {
	return memoryAccount{account: b}
}

// OwnerAccount returns one owner's account in the ContextAccount shape. It
// reports ErrUnknownOwner when no ceiling was set for owner, so a mistyped
// owner stops before a call runs. The account is bound to owner, so every
// reserve, settle and release it makes counts against that owner's ceiling
// and the global one, exactly as Owner's account does.
func (k *KeyedBudget) OwnerAccount(owner string) (ContextAccount, error) {
	account, err := k.Owner(owner)
	if err != nil {
		return nil, err
	}
	return memoryAccount{account: account}, nil
}

// memoryAccount drives an in-memory Account through the ContextAccount
// shape. The context is ignored because the account does no I/O, and the
// hold carries only the amount the account frees or books against.
type memoryAccount struct {
	account Account
}

// A memoryAccount satisfies ContextAccount, so NewMeter and Budget.Account
// share the path NewContextMeter builds.
var _ ContextAccount = memoryAccount{}

// Denomination reports the wrapped account's unit.
func (a memoryAccount) Denomination() Denomination {
	return a.account.Denomination()
}

// Reserve commits estimate against the wrapped account and returns a hold
// that names only the amount.
func (a memoryAccount) Reserve(_ context.Context, estimate Price) (Hold, error) {
	if err := a.account.Reserve(estimate); err != nil {
		return Hold{}, err
	}
	return Hold{Amount: estimate}, nil
}

// Settle books actual and frees the amount h holds.
func (a memoryAccount) Settle(_ context.Context, h Hold, actual Price) error {
	return a.account.Settle(h.Amount, actual)
}

// SettleOnce books actual at most once per kind and reference pair and
// frees the amount h holds.
func (a memoryAccount) SettleOnce(_ context.Context, h Hold, actual Price, kind, ref string) (bool, error) {
	return a.account.SettleOnce(h.Amount, actual, kind, ref)
}

// Release frees the amount h holds. An in-memory release cannot fail.
func (a memoryAccount) Release(_ context.Context, h Hold) error {
	a.account.Release(h.Amount)
	return nil
}
