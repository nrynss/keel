package cost

import (
	"context"
	"errors"
	"math"
	"testing"
)

// credit names the denomination the denomination tests speak. The name is
// opaque and the minor unit is one, because the credit only exists whole.
var credit = Denomination{Name: "provider.unit", Minor: 1}

// TestZeroDenominationIsUSD pins that the zero value names USD nanodollars,
// so a caller that never names a denomination keeps the behaviour it always
// had.
func TestZeroDenominationIsUSD(t *testing.T) {
	var unset Denomination
	if unset != (Denomination{}) {
		t.Fatalf("zero denomination = %+v, want the zero struct", unset)
	}
	budget := mustBudget(t, 100*Cent)
	if got := budget.Denomination(); got != (Denomination{}) {
		t.Errorf("NewBudget denomination = %+v, want the zero value", got)
	}
	if got := mustKeyed(t, Dollar).Denomination(); got != (Denomination{}) {
		t.Errorf("NewKeyedBudget denomination = %+v, want the zero value", got)
	}
}

// TestNewBudgetInCarriesTheDenomination pins that a budget built in a named
// credit reports it, and that a negative limit is refused exactly as
// NewBudget refuses one.
func TestNewBudgetInCarriesTheDenomination(t *testing.T) {
	budget, err := NewBudgetIn(credit, 1000)
	if err != nil {
		t.Fatalf("NewBudgetIn: %v", err)
	}
	if got := budget.Denomination(); got != credit {
		t.Errorf("Denomination() = %+v, want %+v", got, credit)
	}
	keyed, err := NewKeyedBudgetIn(credit, 1000)
	if err != nil {
		t.Fatalf("NewKeyedBudgetIn: %v", err)
	}
	if got := keyed.Denomination(); got != credit {
		t.Errorf("keyed Denomination() = %+v, want %+v", got, credit)
	}
	mustOwnerLimit(t, keyed, "a", 500)
	account, err := keyed.Owner("a")
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}
	if got := account.Denomination(); got != credit {
		t.Errorf("owner account Denomination() = %+v, want %+v", got, credit)
	}
	if _, err := NewBudgetIn(credit, -Cent); !errors.Is(err, ErrNegativeLimit) {
		t.Errorf("NewBudgetIn below zero error = %v, want ErrNegativeLimit", err)
	}
	if _, err := NewKeyedBudgetIn(credit, -Cent); !errors.Is(err, ErrNegativeLimit) {
		t.Errorf("NewKeyedBudgetIn below zero error = %v, want ErrNegativeLimit", err)
	}
}

// TestBudgetInSpendsInItsOwnUnit runs a reserve and a settle against a credit
// budget and pins the exact unit arithmetic. A credit pool is bounded by its
// own count and never by a dollar figure.
func TestBudgetInSpendsInItsOwnUnit(t *testing.T) {
	budget, err := NewBudgetIn(credit, 1000)
	if err != nil {
		t.Fatalf("NewBudgetIn: %v", err)
	}
	if err := budget.Reserve(400); err != nil {
		t.Fatalf("Reserve(400): %v", err)
	}
	if err := budget.Settle(400, 250); err != nil {
		t.Fatalf("Settle(400, 250): %v", err)
	}
	if got := budget.Spent(); got != 250 {
		t.Errorf("Spent() = %d, want 250", got)
	}
	if got, err := budget.Remaining(); err != nil || got != 750 {
		t.Errorf("Remaining() = %d, %v, want 750", got, err)
	}
	if err := budget.Reserve(751); !errors.Is(err, ErrOverBudget) {
		t.Errorf("Reserve past the pool error = %v, want ErrOverBudget", err)
	}
}

// TestMeterStampsChargesWithTheAccountDenomination runs a meter over a credit
// budget and pins that the charge its sink receives names the credit, so a
// report can never show the pool as dollars.
func TestMeterStampsChargesWithTheAccountDenomination(t *testing.T) {
	budget, err := NewBudgetIn(credit, 1000)
	if err != nil {
		t.Fatalf("NewBudgetIn: %v", err)
	}
	ledger := NewLedger()
	meter, err := NewMeter(budget, ledger)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	usage, err := meter.Call(context.Background(), 20, "check", "job-9",
		func(context.Context) (Usage, error) {
			return Usage{Price: 15, Measured: true}, nil
		})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if usage.Price != 15 {
		t.Errorf("Call usage = %d, want 15", usage.Price)
	}
	charges := ledger.Charges()
	if len(charges) != 1 {
		t.Fatalf("Charges() = %d rows, want 1", len(charges))
	}
	if got := charges[0].Denomination; got != credit {
		t.Errorf("charge denomination = %+v, want %+v", got, credit)
	}
	if got, err := ledger.Total(); err != nil || got != 15 {
		t.Errorf("Total() = %d, %v, want 15", got, err)
	}
}

// TestLedgerTotalRefusesMixedDenominations pins that a total whose matched
// charges name different denominations reports ErrMixedDenomination instead
// of an invented figure. A total whose matched charges all name one
// denomination still sums, even on a ledger that holds other denominations
// too.
func TestLedgerTotalRefusesMixedDenominations(t *testing.T) {
	l := NewLedger()
	addCharge(t, l, Charge{Kind: "gen", Units: 1, UnitPrice: Dollar, Ref: "r"})
	addCharge(t, l, Charge{Kind: "check", Units: 15, UnitPrice: 1, Ref: "r", Denomination: credit})
	addCharge(t, l, Charge{Kind: "gen", Units: 2, UnitPrice: Cent, Denomination: credit})

	if _, err := l.Total(); !errors.Is(err, ErrMixedDenomination) {
		t.Errorf("Total() over a mixed ledger error = %v, want ErrMixedDenomination", err)
	}
	if _, err := l.TotalForRef("r"); !errors.Is(err, ErrMixedDenomination) {
		t.Errorf("TotalForRef() over a mixed set error = %v, want ErrMixedDenomination", err)
	}
	if got, err := l.TotalByKind("check"); err != nil || got != 15 {
		t.Errorf("TotalByKind(check) = %d, %v, want 15 in credits", got, err)
	}
}

// TestConversionPricesACreditForReports pins the report conversion and its
// overflow refusal. No budget or store takes a Conversion, so the figure can
// decorate a report and never move a spending decision.
func TestConversionPricesACreditForReports(t *testing.T) {
	conversion := Conversion{Denomination: credit, Nanodollars: USD(0.01)}
	got, err := conversion.Convert(500)
	if err != nil {
		t.Fatalf("Convert(500): %v", err)
	}
	if want := USD(5.00); got != want {
		t.Errorf("Convert(500) = %d, want %d", got, want)
	}
	if got, err := (Conversion{}).Convert(15); err != nil || got != 0 {
		t.Errorf("a zero conversion of 15 = %d, %v, want 0, nil", got, err)
	}
	huge := Conversion{Denomination: credit, Nanodollars: Price(math.MaxInt64)}
	if _, err := huge.Convert(2); !errors.Is(err, ErrOverflow) {
		t.Errorf("Convert past the range error = %v, want ErrOverflow", err)
	}
}
