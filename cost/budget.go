package cost

import (
	"errors"
	"fmt"
	"sync"
)

// ErrOverBudget reports a reservation that would push committed spend past the
// budget limit.
var ErrOverBudget = errors.New("cost: reservation exceeds budget")

// ErrNegativeLimit reports a budget built with a limit below zero.
var ErrNegativeLimit = errors.New("cost: negative budget limit")

// ErrNegativeEstimate reports a reservation with an estimate below zero.
var ErrNegativeEstimate = errors.New("cost: negative reservation estimate")

// Budget bounds the total spend of a sequence of paid calls. Reserve commits
// an estimate before a call and fails when the estimate would pass the limit.
// Settle books the price the call actually cost and frees its reservation.
// Release frees a reservation the caller never spent. Build one with NewBudget.
// Every method is safe for concurrent use.
type Budget struct {
	mu       sync.Mutex
	limit    Price
	spent    Price
	reserved Price
}

// NewBudget returns a Budget that never lets a reservation push spend past
// limit. It reports ErrNegativeLimit when limit is below zero, because a
// negative ceiling admits no spend and only creates misleading headroom.
func NewBudget(limit Price) (*Budget, error) {
	if limit < 0 {
		return nil, fmt.Errorf("cost: budget limit %d: %w", limit, ErrNegativeLimit)
	}
	return &Budget{limit: limit}, nil
}

// Limit returns the ceiling the budget was built with.
func (b *Budget) Limit() Price {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// Spent returns the sum of the prices that Settle booked.
func (b *Budget) Spent() Price {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Reserved returns the sum of the reservations that are still outstanding.
func (b *Budget) Reserved() Price {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserved
}

// Remaining returns the headroom left for new reservations. It is the limit
// minus booked spend minus outstanding reservations. A booking that overshoots
// the limit can push it below zero. It reports ErrOverflow when that difference
// falls outside the int64 range.
func (b *Budget) Remaining() (Price, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remainingLocked()
}

// remainingLocked computes the headroom still allowed. The caller holds the
// lock. It subtracts the outstanding reservation first, because the limit and
// the reservation are both non-negative, so that difference cannot leave the
// int64 range. Subtracting spent last then yields the exact headroom, so it
// reports ErrOverflow only when that headroom itself is unrepresentable.
func (b *Budget) remainingLocked() (Price, error) {
	left, err := sub(b.limit, b.reserved)
	if err != nil {
		return 0, err
	}
	return sub(left, b.spent)
}

// Reserve commits estimate against the budget. It reports ErrNegativeEstimate
// when estimate is below zero. It fails with ErrOverBudget when spent,
// outstanding reservations and estimate together would pass the limit, and it
// commits nothing in that case. It reports ErrOverflow when the headroom or the
// committed total leaves the int64 range. The check and the commit share one
// critical section, so concurrent callers never overspend.
func (b *Budget) Reserve(estimate Price) error {
	if estimate < 0 {
		return fmt.Errorf("cost: reserve %d: %w", estimate, ErrNegativeEstimate)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining, err := b.remainingLocked()
	if err != nil {
		return err
	}
	if estimate > remaining {
		return fmt.Errorf("cost: reserve %s over remaining %s: %w", estimate, remaining, ErrOverBudget)
	}
	committed, err := add(b.reserved, estimate)
	if err != nil {
		return err
	}
	b.reserved = committed
	return nil
}

// Settle books the price a paid call actually cost and releases the reservation
// the caller made for it. reserved is the estimate Reserve committed and
// actual is the true price. A reserved value larger than the outstanding total
// frees only what remains, so the budget never reports a negative reservation.
// It reports ErrOverflow and commits nothing when actual would push spent
// outside the int64 range.
func (b *Budget) Settle(reserved, actual Price) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	spent, err := add(b.spent, actual)
	if err != nil {
		return err
	}
	b.releaseLocked(reserved)
	b.spent = spent
	return nil
}

// Release returns a reservation the caller never spent. A value larger than
// the outstanding total frees only what remains.
func (b *Budget) Release(reserved Price) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseLocked(reserved)
}

// releaseLocked drops amount from the outstanding reservations and clamps the
// result at zero. A negative amount frees nothing, so a bad input never grows
// the accumulator past the int64 range. The caller holds the lock.
func (b *Budget) releaseLocked(amount Price) {
	if amount > b.reserved {
		amount = b.reserved
	}
	if amount < 0 {
		amount = 0
	}
	b.reserved -= amount
}
