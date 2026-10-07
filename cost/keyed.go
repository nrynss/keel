package cost

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownOwner reports an operation that names an owner the budget holds
// no ceiling for.
var ErrUnknownOwner = errors.New("cost: unknown budget owner")

// KeyedBudget bounds the spend of several owners that share one pool in one
// denomination. Every owner carries a ceiling of its own, and one global
// ceiling bounds the sum of what all owners spend and hold together. An owner
// that exhausts its share therefore never touches another owner's headroom.
// The owner key is whatever string the caller uses to name an owner. Build
// one with NewKeyedBudget for USD nanodollars or NewKeyedBudgetIn for a named
// denomination. Every method is safe for concurrent use.
type KeyedBudget struct {
	mu     sync.Mutex
	global *Budget
	owners map[string]*Budget
}

// NewKeyedBudget returns a KeyedBudget whose global ceiling is limit. It
// counts USD nanodollars, which is the behaviour every keyed budget had
// before denominations existed. It reports ErrNegativeLimit when limit is
// below zero, as NewBudget does, because a negative ceiling admits no spend
// and only creates misleading headroom.
func NewKeyedBudget(limit Price) (*KeyedBudget, error) {
	global, err := NewBudget(limit)
	if err != nil {
		return nil, err
	}
	return &KeyedBudget{global: global, owners: make(map[string]*Budget)}, nil
}

// NewKeyedBudgetIn returns a KeyedBudget that counts in denomination, with a
// global ceiling of limit. The zero denomination names USD nanodollars, so
// NewKeyedBudget and NewKeyedBudgetIn with the zero denomination behave the
// same. It reports ErrNegativeLimit when limit is below zero, as NewBudget
// does.
func NewKeyedBudgetIn(denomination Denomination, limit Price) (*KeyedBudget, error) {
	global, err := NewBudgetIn(denomination, limit)
	if err != nil {
		return nil, err
	}
	return &KeyedBudget{global: global, owners: make(map[string]*Budget)}, nil
}

// Denomination reports the unit every owner of this budget counts in. The
// zero value names USD nanodollars. The denomination never changes, because
// a budget that switched unit mid life would silently reprice its recorded
// spend.
func (k *KeyedBudget) Denomination() Denomination {
	return k.global.Denomination()
}

// Limit returns the global ceiling every owner shares.
func (k *KeyedBudget) Limit() Price {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.global.Limit()
}

// SetLimit gives owner a ceiling of its own, or replaces the ceiling it has.
// The owner's booked spend, outstanding holds and settled once pairs
// survive the change. It reports ErrNegativeLimit when limit is below zero.
func (k *KeyedBudget) SetLimit(owner string, limit Price) error {
	if limit < 0 {
		return fmt.Errorf("cost: owner %q limit %d: %w", owner, limit, ErrNegativeLimit)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	current, ok := k.owners[owner]
	if !ok {
		k.owners[owner] = &Budget{limit: limit}
		return nil
	}
	// Every path to an owner account holds k.mu first, so its accumulators
	// and its settled pair set can be read and carried over without taking
	// the account's own lock. The denomination is not carried, because an
	// owner account reports the keyed budget's denomination and the field
	// is unused here.
	k.owners[owner] = &Budget{limit: limit, spent: current.spent, reserved: current.reserved, once: current.once}
	return nil
}

// Reserve commits estimate against owner's ceiling and against the global
// ceiling. It reports ErrUnknownOwner when no ceiling was set for owner, and
// behaves as Budget.Reserve otherwise. The checks and the commits share one
// critical section, so concurrent callers never overspend either bound, and
// a reservation the global ceiling refuses frees the owner hold it took
// first.
func (k *KeyedBudget) Reserve(owner string, estimate Price) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	o, ok := k.owners[owner]
	if !ok {
		return fmt.Errorf("cost: reserve for owner %q: %w", owner, ErrUnknownOwner)
	}
	if err := o.Reserve(estimate); err != nil {
		return err
	}
	if err := k.global.Reserve(estimate); err != nil {
		// No other operation can reach the owner account while k.mu is
		// held, so the hold taken above is still its last one and this
		// frees exactly that hold.
		o.Release(estimate)
		return err
	}
	return nil
}

// Settle books the price owner's call actually cost and frees its
// reservation. It reports ErrUnknownOwner when no ceiling was set for owner,
// and behaves as Budget.Settle otherwise. The global pool books first,
// because an owner's booked spend never exceeds the global one, so once the
// global settle has committed the owner settle cannot overflow.
func (k *KeyedBudget) Settle(owner string, reserved, actual Price) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	o, ok := k.owners[owner]
	if !ok {
		return fmt.Errorf("cost: settle for owner %q: %w", owner, ErrUnknownOwner)
	}
	if err := k.global.Settle(reserved, actual); err != nil {
		return err
	}
	return o.Settle(reserved, actual)
}

// settleOnce books the price owner's call actually cost at most once per
// kind and reference pair, and frees its reservation. It backs the owner
// accounts' SettleOnce, so a meter driving one owner stops a repeated
// reference from booking twice. A pair that settled before frees both
// holds the reserve took, the owner's and the global one, and books
// nothing. The pair check and the bookings share the keyed mutex, so
// concurrent settles of one pair answer exactly one booked true. It
// reports ErrUnknownOwner when no ceiling was set for owner, and behaves
// as KeyedBudget.Settle otherwise.
func (k *KeyedBudget) settleOnce(owner string, reserved, actual Price, kind, ref string) (bool, error) {
	if ref == "" {
		// The refusal runs before any booking, because a guard that fired
		// after the global settle would leave one ceiling booked and the
		// other refused.
		return false, fmt.Errorf("cost: settle once: %w", ErrEmptyReference)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	o, ok := k.owners[owner]
	if !ok {
		return false, fmt.Errorf("cost: settle once for owner %q: %w", owner, ErrUnknownOwner)
	}
	// Every path to an owner account holds k.mu first, so the pair check
	// below and the booking that follows it are one atomic step against
	// every other reserve, settle and release of this budget.
	if o.settledLocked(kind, ref) {
		o.Release(reserved)
		k.global.Release(reserved)
		return false, nil
	}
	// The global pool books first, as Settle books it, because an owner's
	// booked spend never exceeds the global one, so once the global settle
	// has committed the owner settle cannot overflow.
	if err := k.global.Settle(reserved, actual); err != nil {
		return false, err
	}
	if _, err := o.SettleOnce(reserved, actual, kind, ref); err != nil {
		return false, err
	}
	return true, nil
}

// Release returns a reservation owner never spent. It reports ErrUnknownOwner
// when no ceiling was set for owner. A value larger than the outstanding
// total frees only what remains, as Budget.Release does.
func (k *KeyedBudget) Release(owner string, reserved Price) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	o, ok := k.owners[owner]
	if !ok {
		return fmt.Errorf("cost: release for owner %q: %w", owner, ErrUnknownOwner)
	}
	o.Release(reserved)
	k.global.Release(reserved)
	return nil
}

// Remaining returns the headroom owner still has under its own ceiling. It is
// the owner's ceiling minus what its settles booked minus its outstanding
// reservations. It reports ErrUnknownOwner when no ceiling was set for owner,
// and ErrOverflow when that difference leaves the int64 range. A reserve can
// still fail with ErrOverBudget when the global ceiling binds first.
func (k *KeyedBudget) Remaining(owner string) (Price, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	o, ok := k.owners[owner]
	if !ok {
		return 0, fmt.Errorf("cost: remaining for owner %q: %w", owner, ErrUnknownOwner)
	}
	return o.Remaining()
}

// Owner returns one owner's account, shaped for NewMeter. It reports
// ErrUnknownOwner when no ceiling was set for owner, so a mistyped owner
// stops before a call runs. The account reserves and settles against the
// owner's ceiling and against the global one, exactly as this budget's own
// keyed methods do.
func (k *KeyedBudget) Owner(owner string) (Account, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.owners[owner]; !ok {
		return nil, fmt.Errorf("cost: account for owner %q: %w", owner, ErrUnknownOwner)
	}
	return ownerAccount{keyed: k, owner: owner}, nil
}

// ownerAccount drives one owner of a keyed budget through the Account shape
// the meter consumes. It stays a value because it holds a shared pointer
// beside a key, so copies share one budget, while Meter stays a pointer
// because it owns the booked flag its deferred release reads.
type ownerAccount struct {
	keyed *KeyedBudget
	owner string
}

// Denomination reports the unit this owner's prices count, which is the one
// the keyed budget was built with.
func (a ownerAccount) Denomination() Denomination {
	return a.keyed.Denomination()
}

func (a ownerAccount) Reserve(estimate Price) error {
	return a.keyed.Reserve(a.owner, estimate)
}

func (a ownerAccount) Settle(reserved, actual Price) error {
	return a.keyed.Settle(a.owner, reserved, actual)
}

// SettleOnce books the price the call actually cost at most once per kind
// and reference pair, and frees the reservation. A pair that settled
// before books nothing, frees this call's hold from both ceilings, and
// reports booked false, exactly as the keyed budget's own accounts settle.
func (a ownerAccount) SettleOnce(reserved, actual Price, kind, ref string) (bool, error) {
	return a.keyed.settleOnce(a.owner, reserved, actual, kind, ref)
}

func (a ownerAccount) Release(reserved Price) {
	_ = a.keyed.Release(a.owner, reserved) // the owners map only grows, so the refusal cannot fire for a built account
}
