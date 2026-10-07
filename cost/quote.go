package cost

import (
	"errors"
	"fmt"
	"time"
)

// The wire codes a quote Run refuses with. Every code is a stable
// snake_case identifier an HTTP handler passes to the error envelope
// unchanged.
const (
	// CodeQuoteExpired names a quote whose confirm window has passed.
	// The refusal carries a fresh quote for the same action.
	CodeQuoteExpired = "quote_expired"
	// CodeQuotePriceMoved names a quote whose current price moved past
	// the configured tolerance from the quoted one. The refusal carries
	// a fresh quote priced at the current price.
	CodeQuotePriceMoved = "quote_price_moved"
	// CodeQuoteUnknown names a quote id no row backs, because it never
	// existed or a sweep removed it after expiry.
	CodeQuoteUnknown = "quote_unknown"
	// CodeQuotePending names a quote another runner still claims. The
	// claim may finish, so the client asks again rather than re-runs.
	CodeQuotePending = "quote_pending"
)

// ErrQuoteExpired reports a Run of a quote whose confirm window passed
// before the claim.
var ErrQuoteExpired = errors.New("cost: quote expired")

// ErrQuotePriceMoved reports a Run whose current price moved past the
// configured tolerance from the quoted one.
var ErrQuotePriceMoved = errors.New("cost: quote price moved")

// ErrQuoteUnknown reports a Run of a quote id no row backs.
var ErrQuoteUnknown = errors.New("cost: quote unknown")

// ErrQuotePending reports a Run of a quote another runner still claims.
var ErrQuotePending = errors.New("cost: quote pending")

// Quote is the price of one named action that a person confirms before it
// runs. It holds no budget: the reservation is taken when the action runs,
// so a quote that is never confirmed costs nothing. A store hands one back
// from a quote request, and a refusal carries a fresh one so a client can
// re-confirm against it without a second round trip.
type Quote struct {
	// ID identifies the quote. A Run names the id, so the same id never
	// spends twice.
	ID string
	// Owner names the account the run will reserve against.
	Owner string
	// Price is the confirmed amount in minor units.
	Price Price
	// Denomination names the unit Price counts. The zero value names USD
	// nanodollars.
	Denomination Denomination
	// ExpiresAt is the instant after which the quote refuses and a fresh
	// one is issued instead.
	ExpiresAt time.Time
}

// QuoteRefusal is the error a quote Run refuses with. Code is the stable
// wire code a handler passes to the error envelope, and Quote is the fresh
// quote a client can re-confirm. Quote is the zero value when there is
// nothing to re-confirm, which is the case for the unknown and pending
// codes. Err is the condition the refusal names, so errors.Is matches it
// against the ErrQuote sentinels. Errors.As reaches the refusal itself.
type QuoteRefusal struct {
	// Code is one of the CodeQuote constants.
	Code string
	// Quote is the fresh quote to re-confirm, or the zero Quote when the
	// refusal carries none.
	Quote Quote
	// Err is the condition the refusal names.
	Err error
}

// Error returns the refusal message. The code comes first, so a log line
// always names the branch a client took.
func (r *QuoteRefusal) Error() string {
	return fmt.Sprintf("%s: %s", r.Code, r.Err.Error())
}

// Unwrap returns the condition the refusal names, so errors.Is matches a
// refusal against the ErrQuote sentinels.
func (r *QuoteRefusal) Unwrap() error {
	return r.Err
}
