package cost

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// ErrNilAccount reports a meter built without a budget account.
var ErrNilAccount = errors.New("cost: nil meter account")

// ErrNilSink reports a meter built without a charge sink.
var ErrNilSink = errors.New("cost: nil meter sink")

// ErrNegativePrice reports a call that measured a price below zero.
var ErrNegativePrice = errors.New("cost: negative measured price")

// ErrUnrecordedCharge reports that a settled charge did not land in the
// meter's sink. The call itself ran and its spend is booked against the
// account, so the condition is a missing record, not an unrun call.
var ErrUnrecordedCharge = errors.New("cost: unrecorded charge")

// Account is the budget side a meter drives. A Budget is an account on its
// own, and a KeyedBudget hands out one account per owner through Owner. The
// spending methods mean exactly what they mean on Budget, so both kinds of
// budget drive a meter identically.
type Account interface {
	// Denomination reports the unit the account's prices count. The meter
	// stamps every charge it records with it, so a report never shows a
	// unit the account does not speak.
	Denomination() Denomination
	// Reserve commits estimate against the account and fails when the
	// estimate would pass its ceiling.
	Reserve(estimate Price) error
	// Settle books the price the call actually cost and frees its
	// reservation.
	Settle(reserved, actual Price) error
	// SettleOnce books the price the call actually cost at most once per
	// kind and reference pair, and frees its reservation. A pair that
	// settled before books nothing, frees the reservation, and reports
	// booked false. A settle without a reference reports an error matching
	// ErrEmptyReference on every account, because an empty reference would
	// collapse a kind's once settles onto one pair.
	SettleOnce(reserved, actual Price, kind, ref string) (booked bool, err error)
	// Release frees a reservation the call never spent.
	Release(reserved Price)
}

// ChargeSink is where a settled charge is recorded. The in-memory Ledger is
// one sink, and the cost/sqlitestore Store is another. An application that
// keeps its spend record somewhere else carries its own one-method sink and
// hands it to NewMeter. Add must be safe for concurrent use, because
// concurrent Calls drive one sink.
type ChargeSink interface {
	// Add records one charge before it returns. It reports the error
	// when the charge did not land, so the caller never reports a figure
	// the sink cannot back.
	Add(ctx context.Context, c Charge) error
}

// A Ledger satisfies ChargeSink, so a meter can record into the in-memory
// ledger out of the box.
var _ ChargeSink = (*Ledger)(nil)

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

// callSettings carries the options one Call received.
type callSettings struct {
	// once settles the call at most once per kind and reference.
	once bool
}

// CallOption tunes one Call. The empty option list keeps Call's default
// behaviour, which settles every call.
type CallOption func(*callSettings)

// Once settles the call at most once per kind and reference pair. The
// account settles through SettleOnce, and the charge reaches the sink only
// when the account booked this settle. A call whose pair a previous settle
// already booked returns the zero usage and records nothing, so a resume
// or a retry of the same work never books it twice. A charge the sink
// refuses after a once settle stays unrecorded, because the pair is booked
// and no repeat of the reference reaches the sink again. Pass it for a
// reference that names one charge, such as an idempotency key. A reference
// that legitimately carries several charges calls Call without Once.
func Once() CallOption {
	return func(s *callSettings) { s.once = true }
}

// Meter runs paid calls against one account and records what each call
// settles at in its sink. It reserves the estimate before the call runs and
// settles the measured price after it. It frees the reservation on every
// failure up to and including the settle, so a failed, cancelled or
// panicking call never holds budget. Every charge it records carries the
// account's denomination, so a credit pool never lands in a report as
// dollars. A charge the sink cannot record is the one failure past the
// settle, and Call reports it as ErrUnrecordedCharge. A Call that passes
// Once settles at most once per kind and reference pair, which is how a
// resume of settled work books nothing again. Build one with
// NewMeter. Call is safe for concurrent use, because every method it drives
// is.
type Meter struct {
	account Account
	sink    ChargeSink
	denom   Denomination
}

// isNilValue reports whether v is nil or an interface holding a nil pointer.
// A typed nil such as (*Ledger)(nil) passes a plain == nil check and would
// panic at first use, so the constructor checks the pointer itself.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	return r.Kind() == reflect.Pointer && r.IsNil()
}

// NewMeter returns a Meter that bounds every call by account and records
// every settled charge in sink. It reports ErrNilAccount or ErrNilSink when
// one of them is nil. Nil covers an interface holding a nil pointer as well
// as a plain nil, because a meter that could not bound or record a call
// would only pretend to.
func NewMeter(account Account, sink ChargeSink) (*Meter, error) {
	if isNilValue(account) {
		return nil, ErrNilAccount
	}
	if isNilValue(sink) {
		return nil, ErrNilSink
	}
	// The denomination is fixed for the life of the account, so the meter
	// reads it once and stamps every charge with the same unit.
	return &Meter{account: account, sink: sink, denom: account.Denomination()}, nil
}

// Call runs work as one paid call against the meter's account. It commits
// estimate first, and it returns the refusal without running work when the
// reservation fails or the context is already done. It then runs work and
// settles the price the usage reports. A usage with Measured false settles
// at the estimate, and the usage Call returns says so the same way. A
// measured price below zero reports ErrNegativePrice before any booking,
// because a refund is a separate charge and never a negative booking.
//
// With Once among the options, the account settles at most once per kind
// and reference pair. The first settle behaves exactly as the default one
// does. A pair that has settled before frees this call's reservation,
// books nothing, reaches no sink, and returns the zero usage, because this
// call booked nothing. A resume or a retry that runs the same work under
// the same reference therefore leaves the books exactly as the first run
// wrote them. Under Once a charge the sink refuses after the settle stays
// unrecorded, because the pair is booked and no repeat of the reference
// reaches the sink. The ErrUnrecordedCharge that Call then reports is
// final for that reference. Without Once, Call is unchanged, so a
// reference that legitimately carries several charges keeps booking every
// one.
// legitimately carries several charges keeps booking every one.
//
// Every failure on the way to the settle frees the reservation, so a call
// that books nothing leaves nothing held. A context that finishes mid-call
// surfaces as the error the work returns and frees the reservation the
// same way. A panic in work frees the reservation while it unwinds and
// continues past Call with its own value and stack, because the panic
// belongs to the caller's code. A settle the account refuses frees the
// reservation and reports the error, so no failed call books spend or
// records a charge on any of those paths. A charge the sink then refuses
// to record is the one error past the settle. The call ran and its spend
// is booked, so the reservation stays consumed. Call reports
// ErrUnrecordedCharge with the sink's error and the zero usage, because a
// figure no record backs is not a result.
func (m *Meter) Call(ctx context.Context, estimate Price, kind, ref string, work Work, opts ...CallOption) (Usage, error) {
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}
	var settings callSettings
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if err := m.account.Reserve(estimate); err != nil {
		return Usage{}, err
	}
	// booked lifts the release off the one path that already settled, so
	// the settle consuming the reservation is never followed by a second
	// release that would eat another caller's hold. A panic in work skips
	// the assignment below, so unwinding runs the release and the panic
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
	if usage.Measured && price < 0 {
		return Usage{}, fmt.Errorf("cost: measured price %d: %w", price, ErrNegativePrice)
	}
	// settled says the account booked this call's spend. A once settle of a
	// repeated pair answers false and has already freed the reservation, so
	// the flag below still lifts the deferred release off a consumed hold.
	var settled bool
	if settings.once {
		settled, err = m.account.SettleOnce(estimate, price, kind, ref)
	} else {
		err = m.account.Settle(estimate, price)
		settled = err == nil
	}
	if err != nil {
		return Usage{}, err
	}
	booked = true
	if !settled {
		// The pair settled before, so this call books nothing and the sink
		// never sees a second charge for it. The zero usage reports the
		// fact, the same shape a joiner reports for work it did not settle.
		return Usage{}, nil
	}
	if err := m.sink.Add(ctx, Charge{Kind: kind, Units: 1, UnitPrice: price, Ref: ref, Denomination: m.denom}); err != nil {
		return Usage{}, fmt.Errorf("%w: %w", ErrUnrecordedCharge, err)
	}
	return Usage{Price: price, Measured: usage.Measured}, nil
}
