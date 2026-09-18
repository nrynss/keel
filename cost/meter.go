package cost

import (
	"context"
	"errors"
)

// ErrNilAccount reports a meter built without a budget account.
var ErrNilAccount = errors.New("cost: nil meter account")

// ErrNilLedger reports a meter built without a ledger.
var ErrNilLedger = errors.New("cost: nil meter ledger")

// Account is the budget side a meter drives. A Budget is an account on its
// own, and a KeyedBudget hands out one account per owner through Owner. The
// three methods mean exactly what they mean on Budget, so both kinds of
// budget drive a meter identically.
type Account interface {
	// Reserve commits estimate against the account and fails when the
	// estimate would pass its ceiling.
	Reserve(estimate Price) error
	// Settle books the price the call actually cost and frees its
	// reservation.
	Settle(reserved, actual Price) error
	// Release frees a reservation the call never spent.
	Release(reserved Price)
}

// Usage is the price one paid call reports for itself. A work function
// returns one when its call finishes, and Call returns the usage it settled
// at. Measured separates a price the call measured from the reservation
// estimate that stands in when it cannot measure one.
type Usage struct {
	// Price is what the call cost.
	Price Price
	// Measured reports that Price came from the call's own usage report
	// rather than from the reservation estimate.
	Measured bool
}

// Work is the body of one paid call. The meter runs it after reserving and
// settles what it reports. A work observes its context and stops with the
// context error when the context is done. An error discards the usage, so a
// work that fails returns the zero usage beside its error.
type Work func(context.Context) (Usage, error)

// Meter runs paid calls against one account and records what each call
// settles at. It reserves the estimate before the call runs, settles the
// measured price after it, and frees the reservation on every failure path,
// so a failed, cancelled or panicking call never holds budget. Build one
// with NewMeter. Call is safe for concurrent use, because every method it
// drives is.
type Meter struct {
	account Account
	ledger  *Ledger
}

// NewMeter returns a Meter that bounds every call by account and records
// every settled charge in ledger. It reports ErrNilAccount or ErrNilLedger
// when one of them is nil, because a meter that could not bound or record a
// call would only pretend to.
func NewMeter(account Account, ledger *Ledger) (*Meter, error) {
	if account == nil {
		return nil, ErrNilAccount
	}
	if ledger == nil {
		return nil, ErrNilLedger
	}
	return &Meter{account: account, ledger: ledger}, nil
}

// Call runs work as one paid call against the meter's account. It commits
// estimate first, and it returns the refusal without running work when the
// reservation fails or the context is already done. It then runs work and
// settles the price the usage reports. A usage with Measured false settles
// at the estimate, and the usage Call returns says so the same way.
//
// A returned error frees the reservation. So does a context that finishes
// mid-call, which surfaces as the error the work returns. A panic in work
// frees the reservation while it unwinds and continues past Call with its
// own value and stack, because the panic belongs to the caller's code. No
// failed call books spend or records a charge. A settle the account refuses
// frees the reservation and reports the error, so a call that books nothing
// leaves nothing held.
func (m *Meter) Call(ctx context.Context, estimate Price, kind, ref string, work Work) (Usage, error) {
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}
	if err := m.account.Reserve(estimate); err != nil {
		return Usage{}, err
	}
	// booked lifts the release off the one path that already settled, so
	// the settle consuming the reservation is never followed by a second
	// release that would eat another caller's hold. A panic skips the
	// assignment below, so unwinding runs the release and the panic
	// continues past it on its own.
	booked := false
	defer func() {
		if !booked {
			m.account.Release(estimate)
		}
	}()
	usage, err := work(ctx)
	if err != nil {
		return Usage{}, err
	}
	price := usage.Price
	if !usage.Measured {
		price = estimate
	}
	if err := m.account.Settle(estimate, price); err != nil {
		return Usage{}, err
	}
	m.ledger.Add(Charge{Kind: kind, Units: 1, UnitPrice: price, Ref: ref})
	booked = true
	return Usage{Price: price, Measured: usage.Measured}, nil
}
