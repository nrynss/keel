package cost

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownOwner reports an operation that names an owner the budget holds
// no ceiling for.
var ErrUnknownOwner = errors.New("cost: unknown budget owner")

// KeyedBudget bounds the spend of several owners that share one pool. Every
// owner carries a ceiling of its own, and one global ceiling bounds the sum
// of what all owners spend and hold together, so an owner that exhausts its
// share never touches another owner's headroom. The owner key is whatever
// string the caller uses to name an owner. Build one with NewKeyedBudget.
// Every method is safe for concurrent use.
type KeyedBudget struct {
	mu     sync.Mutex
	global *Budget
	owners map[string]*Budget
}

// NewKeyedBudget returns a KeyedBudget whose global ceiling is limit. It
// reports ErrNegativeLimit when limit is below zero, as NewBudget does,
// because a negative ceiling admits no spend and only creates misleading
// headroom.
func NewKeyedBudget(limit Price) (*KeyedBudget, error) {
	global, err := NewBudget(limit)
	if err != nil {
		return nil, err
	}
	return &KeyedBudget{global: global, owners: make(map[string]*Budget)}, nil
}

// Limit returns the global ceiling every owner shares.
func (k *KeyedBudget) Limit() Price {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.global.Limit()
}

// SetLimit gives owner a ceiling of its own, or replaces the ceiling it has.
// The owner's booked spend and outstanding holds survive the change. It
// reports ErrNegativeLimit when limit is below zero.
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
	// can be read and carried over without taking the account's own lock.
	k.owners[owner] = &Budget{limit: limit, spent: current.spent, reserved: current.reserved}
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
