package cost

import (
	"errors"
	"testing"
)

func TestQuoteRefusalWrapsSentinelAndCarriesCode(t *testing.T) {
	refusal := &QuoteRefusal{Code: CodeQuoteExpired, Err: ErrQuoteExpired}
	err := error(refusal)
	if !errors.Is(err, ErrQuoteExpired) {
		t.Fatalf("errors.Is(expired): got %v", err)
	}
	if errors.Is(err, ErrQuotePriceMoved) {
		t.Fatalf("errors.Is matched the wrong sentinel: %v", err)
	}
	if got, want := refusal.Error(), "quote_expired: cost: quote expired"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	var got *QuoteRefusal
	if !errors.As(err, &got) {
		t.Fatalf("errors.As found no QuoteRefusal in %v", err)
	}
	if got.Code != CodeQuoteExpired {
		t.Fatalf("Code = %q, want %q", got.Code, CodeQuoteExpired)
	}
}
