package cost

import (
	"errors"
	"fmt"
)

// ErrDenominationMismatch reports a charge or a configuration that names a
// denomination the budget or store it reached does not speak. A handler maps
// it to a stable refusal code, because the caller named a unit the account
// does not hold. There is no conversion on any path, so the refusal is final.
var ErrDenominationMismatch = errors.New("cost: denomination mismatch")

// ErrMixedDenomination reports a total that would sum charges naming
// different denominations. Such a sum invents an exchange rate, so the total
// refuses instead of showing the invented figure.
var ErrMixedDenomination = errors.New("cost: mixed denominations")

// Denomination names the unit a Price counts. The zero value names USD
// nanodollars, which is what every budget and charge counted before
// denominations existed, so a caller that never names one keeps the behaviour
// it always had. A name is opaque. The package attaches no meaning to any
// name, two denominations are the same when their names are equal, and
// nothing in the package converts one denomination into another.
type Denomination struct {
	// Name identifies the unit. Empty names USD nanodollars. A credit names
	// itself, for example a provider's own unit, and the same name travels
	// from the budget through the meter to every charge and report.
	Name string
	// Minor is how many minor units make one whole unit, so a report can
	// format an amount without inventing a fraction. A credit that only
	// exists whole carries one. The package never reads it, so it can never
	// move a spending decision.
	Minor int
}

// Conversion records what one minor unit of a credit denomination was worth
// in nanodollars when it was priced. A report uses it to show a credit pool
// beside a dollar one. No budget, account, meter or store accepts one, so a
// conversion can never change what a caller may spend: the type lives
// outside the spend path by construction.
type Conversion struct {
	// Denomination names the credit the conversion prices.
	Denomination Denomination
	// Nanodollars is the worth of one minor unit of that denomination.
	Nanodollars Price
}

// Convert prices amount, counted in the conversion's denomination, in
// nanodollars. It reports ErrOverflow when the exact product leaves the
// int64 range. It is a report helper and nothing else, because no budget or
// store takes a Conversion.
func (c Conversion) Convert(amount Price) (Price, error) {
	p, err := mul(amount, c.Nanodollars)
	if err != nil {
		return 0, fmt.Errorf("cost: convert %d %s: %w", amount, c.Denomination.Name, err)
	}
	return p, nil
}
