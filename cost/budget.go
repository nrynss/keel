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

// ErrEmptyReference reports a settle once without a reference. A once
// settle dedupes on the kind and reference pair, and an empty reference
// would collapse every once settle of a kind onto one pair and drop every
// later charge, so every account refuses it and books nothing.
var ErrEmptyReference = errors.New("cost: empty settle-once reference")

// Budget bounds the total spend of a sequence of paid calls in one
// denomination. Reserve commits an estimate before a call and fails when the
// estimate would pass the limit. Settle books the price the call actually
// cost and frees its reservation. SettleOnce books a price at most once per
// kind and reference pair, for a call another settle may have already
// booked. Release frees a reservation the caller never spent. Build one
// with NewBudget for USD nanodollars or NewBudgetIn for a named
// denomination such as a provider's credit. Every method is safe for
// concurrent use.
type Budget struct {
	mu       sync.Mutex
	denom    Denomination
	limit    Price
	spent    Price
	reserved Price
	once     map[settleRef]struct{}
}

// settleRef names one kind and reference pair a SettleOnce booked. The
// budget keeps the pairs it has settled, so a repeat of a pair never books
// twice.
type settleRef struct {
	kind string
	ref  string
}

// NewBudget returns a Budget that never lets a reservation push spend past
// limit. It counts USD nanodollars, which is the behaviour every budget had
// before denominations existed. It reports ErrNegativeLimit when limit is
// below zero, because a negative ceiling admits no spend and only creates
// misleading headroom.
func NewBudget(limit Price) (*Budget, error) {
	if limit < 0 {
		return nil, fmt.Errorf("cost: budget limit %d: %w", limit, ErrNegativeLimit)
	}
	return &Budget{limit: limit}, nil
}

// NewBudgetIn returns a Budget that counts in denomination and never lets a
// reservation push spend past limit. The zero denomination names USD
// nanodollars, so NewBudget and NewBudgetIn with the zero denomination
// behave the same. It reports ErrNegativeLimit when limit is below zero, as
// NewBudget does.
func NewBudgetIn(denomination Denomination, limit Price) (*Budget, error) {
	if limit < 0 {
		return nil, fmt.Errorf("cost: budget limit %d: %w", limit, ErrNegativeLimit)
	}
	return &Budget{denom: denomination, limit: limit}, nil
}

// Denomination reports the unit this budget's prices count. The zero value
// names USD nanodollars. The denomination never changes, because a budget
// that switched unit mid life would silently reprice its recorded spend.
func (b *Budget) Denomination() Denomination {
	return b.denom
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

// SettleOnce books the price a paid call actually cost at most once per
// kind and reference pair, and releases the reservation the caller made
// for it. The first call for a pair behaves exactly as Settle does and
// reports booked true. A pair that has settled before books nothing and
// reports booked false, because a resume or a retry of the same work must
// not charge the books twice. The repeat still frees its reservation, so a
// deduplicated call holds no budget. A settle without a reference reports
// an error matching ErrEmptyReference and books nothing, because an empty
// reference would collapse every once settle of a kind onto one pair. It
// reports ErrOverflow and commits nothing, not even the pair, when actual
// would push spent outside the int64 range.
func (b *Budget) SettleOnce(reserved, actual Price, kind, ref string) (booked bool, err error) {
	if ref == "" {
		return false, fmt.Errorf("cost: settle once: %w", ErrEmptyReference)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.settledLocked(kind, ref) {
		b.releaseLocked(reserved)
		return false, nil
	}
	spent, err := add(b.spent, actual)
	if err != nil {
		return false, err
	}
	if b.once == nil {
		b.once = make(map[settleRef]struct{})
	}
	b.once[settleRef{kind: kind, ref: ref}] = struct{}{}
	b.releaseLocked(reserved)
	b.spent = spent
	return true, nil
}

// settledLocked reports whether the kind and reference pair already
// settled. The caller holds the lock that guards the set, which is b.mu
// for a standalone budget and the keyed budget's own mutex for an owner
// account, because every path to an owner account holds that mutex first.
func (b *Budget) settledLocked(kind, ref string) bool {
	_, dup := b.once[settleRef{kind: kind, ref: ref}]
	return dup
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
