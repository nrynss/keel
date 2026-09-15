// Package cost records what paid API calls spend. A Price counts nanodollars
// in an int64, so sums stay exact to the cent. A Ledger keeps charges and
// totals them by kind and by reference. A Budget bounds the spend of a
// sequence of calls. Rate cards stay in each application.
package cost

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
)

// Price represents a cost in nanodollars (billionths of a dollar).
type Price int64

const (
	// Nanodollar is one billionth of a dollar.
	Nanodollar Price = 1
	// Microdollar is one millionth of a dollar.
	Microdollar Price = 1_000
	// Millicent is one thousandth of a cent.
	Millicent Price = 10_000
	// Cent is one hundredth of a dollar.
	Cent Price = 10_000_000
	// Dollar is one dollar.
	Dollar Price = 1_000_000_000
)

// USD converts a dollar amount into a Price in nanodollars.
func USD(dollars float64) Price {
	return Price(dollars * float64(Dollar))
}

// Dollars converts a Price in nanodollars back into a dollar amount.
func (p Price) Dollars() float64 {
	return float64(p) / float64(Dollar)
}

// String formats the Price as a dollar amount. Non-zero costs never display as $0.00.
func (p Price) String() string {
	s := fmt.Sprintf("$%.9f", p.Dollars())
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "00"
	} else if len(s)-strings.Index(s, ".") == 2 {
		s += "0"
	}
	return s
}

// ErrOverflow reports a price product, sum, or difference that falls outside
// the int64 range.
var ErrOverflow = errors.New("cost: price overflows int64")

// minInt64 is the most negative price. Negating it overflows, so mul guards
// that one factor pair by hand.
const minInt64 Price = -1 << 63

// mul multiplies two Prices. It reports ErrOverflow when the exact product
// falls outside the int64 range, so a wrapped cost never reaches a caller.
// The check uses integer division only and never floating point.
func mul(a, b Price) (Price, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if (a == -1 && b == minInt64) || (b == -1 && a == minInt64) {
		return 0, fmt.Errorf("cost: multiply %d by %d: %w", a, b, ErrOverflow)
	}
	p := a * b
	if p/b != a {
		return 0, fmt.Errorf("cost: multiply %d by %d: %w", a, b, ErrOverflow)
	}
	return p, nil
}

// add sums two Prices. It reports ErrOverflow when the exact sum falls outside
// the int64 range, so a wrapped total never reaches a caller. The check uses
// integer arithmetic only and never floating point.
func add(a, b Price) (Price, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("cost: add %d and %d: %w", a, b, ErrOverflow)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("cost: add %d and %d: %w", a, b, ErrOverflow)
	}
	return a + b, nil
}

// sub subtracts b from a. It reports ErrOverflow when the exact difference
// falls outside the int64 range, so a wrapped headroom never reaches a caller.
// The check uses integer arithmetic only and never floating point.
func sub(a, b Price) (Price, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, fmt.Errorf("cost: subtract %d and %d: %w", a, b, ErrOverflow)
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, fmt.Errorf("cost: subtract %d and %d: %w", a, b, ErrOverflow)
	}
	return a - b, nil
}

// Charge is one billed operation. Kind names the operation, Units counts the
// billed units, UnitPrice prices one unit, and Ref ties the charge back to the
// caller's own record, for example a job or a request id.
type Charge struct {
	// Kind names the billed operation.
	Kind string
	// Units counts the billed units.
	Units int
	// UnitPrice is the price of one unit.
	UnitPrice Price
	// Ref ties the charge to the caller's own record.
	Ref string
}

// Total returns the price of the charge. It reports ErrOverflow when Units
// times UnitPrice falls outside the int64 range. A ledger total sums the
// charges' exact products, so it can still fit when one charge alone does not.
func (c Charge) Total() (Price, error) {
	return mul(Price(c.Units), c.UnitPrice)
}

// Ledger records charges and totals them by kind and by reference. Every
// method is safe for concurrent use.
type Ledger struct {
	mu      sync.RWMutex
	charges []Charge
}

// NewLedger returns an empty Ledger ready for use.
func NewLedger() *Ledger {
	return &Ledger{}
}

// Add appends c to the ledger.
func (l *Ledger) Add(c Charge) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.charges = append(l.charges, c)
}

// Charges returns a copy of every recorded charge. A later Add never changes
// the returned slice.
func (l *Ledger) Charges() []Charge {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c := make([]Charge, len(l.charges))
	copy(c, l.charges)
	return c
}

// Total returns the sum of every charge in the ledger. Each charge contributes
// its exact product, so a charge whose own price overflows is still summed
// exactly. It reports ErrOverflow only when that exact sum falls outside the
// int64 range, never for a running partial sum that leaves the range on the
// way.
func (l *Ledger) Total() (Price, error) {
	return l.total(func(Charge) bool { return true })
}

// TotalByKind returns the sum of the charges whose Kind equals kind. Each
// matching charge contributes its exact product, so it reports ErrOverflow
// under the same rule as Total.
func (l *Ledger) TotalByKind(kind string) (Price, error) {
	return l.total(func(c Charge) bool { return c.Kind == kind })
}

// TotalForRef returns the sum of the charges whose Ref equals ref. Each
// matching charge contributes its exact product, so it reports ErrOverflow
// under the same rule as Total.
func (l *Ledger) TotalForRef(ref string) (Price, error) {
	return l.total(func(c Charge) bool { return c.Ref == ref })
}

// total sums every charge that match selects. It holds the read lock across
// the whole walk, so a concurrent Add never races it. It adds the exact
// products on the int64 fast path and switches to a big integer accumulator
// once a product or the running sum first leaves the range. It therefore
// returns the exact sum of the selected charges' exact products, and reports
// ErrOverflow only when that exact sum is unrepresentable.
func (l *Ledger) total(match func(Charge) bool) (Price, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var sum Price
	var exact *big.Int
	for _, c := range l.charges {
		if !match(c) {
			continue
		}
		amount, err := c.Total()
		if err != nil {
			u := new(big.Int).Mul(big.NewInt(int64(c.Units)), big.NewInt(int64(c.UnitPrice)))
			if exact == nil {
				exact = big.NewInt(int64(sum))
			}
			exact.Add(exact, u)
			continue
		}
		if exact != nil {
			exact.Add(exact, big.NewInt(int64(amount)))
			continue
		}
		next, err := add(sum, amount)
		if err != nil {
			exact = new(big.Int).Add(big.NewInt(int64(sum)), big.NewInt(int64(amount)))
			continue
		}
		sum = next
	}
	if exact != nil {
		if !exact.IsInt64() {
			return 0, fmt.Errorf("cost: ledger total %s: %w", exact, ErrOverflow)
		}
		return Price(exact.Int64()), nil
	}
	return sum, nil
}
