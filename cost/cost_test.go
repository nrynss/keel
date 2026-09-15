package cost

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPriceArithmetic(t *testing.T) {
	if USD(1.0) != Dollar {
		t.Errorf("USD(1.0) = %d, want %d", USD(1.0), Dollar)
	}

	want := 30 * Microdollar
	if got := USD(0.00003); got != want {
		t.Errorf("USD(0.00003) = %d, want %d", got, want)
	}

	dollars := USD(1.50).Dollars()
	if math.Abs(dollars-1.50) > 1e-9 {
		t.Errorf("USD(1.50).Dollars() = %v, want 1.50", dollars)
	}

	chirpCost := Price(1000) * USD(30.0) / 1_000_000
	if chirpCost != USD(0.03) {
		t.Errorf("Chirp 1000 chars cost = %d, want %d", chirpCost, USD(0.03))
	}

	geminiCost := Price(1000) * USD(0.10) / 1_000_000
	if geminiCost == 0 {
		t.Error("Gemini 1000 chars cost is zero")
	}
	if want := Price(100_000); geminiCost != want {
		t.Errorf("Gemini 1000 chars cost = %d, want %d", geminiCost, want)
	}
}

func TestPriceString(t *testing.T) {
	tests := []struct {
		name string
		p    Price
		want string
	}{
		{"zero", 0, "$0.00"},
		{"dollar", Dollar, "$1.00"},
		{"fifty cents", 50 * Cent, "$0.50"},
		{"micro-cost", 100_000, "$0.0001"},
		{"nano-cost", 100, "$0.0000001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Errorf("String() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestChargeTotalOverflow checks that a charge whose product leaves the int64
// range reports ErrOverflow and that a charge inside the range does not.
func TestChargeTotalOverflow(t *testing.T) {
	huge := Charge{Kind: "huge", Units: int(math.MaxInt64), UnitPrice: 2}
	if _, err := huge.Total(); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Total() error = %v, want ErrOverflow", err)
	}

	fine := Charge{Kind: "fine", Units: 1000, UnitPrice: Microdollar}
	got, err := fine.Total()
	if err != nil {
		t.Fatalf("Total() error = %v, want nil", err)
	}
	if want := Price(1_000_000); got != want {
		t.Errorf("Total() = %d, want %d", got, want)
	}

	if got, err := (Charge{Units: 0, UnitPrice: Dollar}).Total(); err != nil || got != 0 {
		t.Errorf("zero-unit Total() = %d, %v, want 0, nil", got, err)
	}

	// Negating the most negative price overflows, and the sign check alone
	// cannot see it.
	neg := Charge{Kind: "neg", Units: -1, UnitPrice: minInt64}
	if _, err := neg.Total(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Total() of -1 times the most negative price error = %v, want ErrOverflow", err)
	}
}

// TestLedgerTotalOverflow checks that a ledger whose charge overflows reports
// ErrOverflow from every total.
func TestLedgerTotalOverflow(t *testing.T) {
	l := NewLedger()
	l.Add(Charge{Kind: "huge", Units: int(math.MaxInt64), UnitPrice: 2, Ref: "a"})

	for name, total := range map[string]func() (Price, error){
		"Total":       l.Total,
		"TotalByKind": func() (Price, error) { return l.TotalByKind("huge") },
		"TotalForRef": func() (Price, error) { return l.TotalForRef("a") },
	} {
		if _, err := total(); !errors.Is(err, ErrOverflow) {
			t.Errorf("%s() error = %v, want ErrOverflow", name, err)
		}
	}
}

// TestLedgerConcurrentAdds drives many goroutines through one ledger and reads
// back every total. It runs under -race and proves the read and write paths do
// not race.
func TestLedgerConcurrentAdds(t *testing.T) {
	const writers = 50
	const perWriter = 40
	l := NewLedger()
	l.Add(Charge{Kind: "other", Units: 1, UnitPrice: Dollar, Ref: "job-b"})

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				l.Add(Charge{Kind: "synth", Units: 10, UnitPrice: Cent, Ref: "job-a"})
			}
		}()
	}
	wg.Wait()

	const synthTotal = Price(writers * perWriter * 10 * Cent)

	total, err := l.Total()
	if err != nil {
		t.Fatalf("Total() error = %v, want nil", err)
	}
	if want := synthTotal + Dollar; total != want {
		t.Errorf("Total() = %d, want %d", total, want)
	}

	byKind, err := l.TotalByKind("synth")
	if err != nil {
		t.Fatalf("TotalByKind() error = %v, want nil", err)
	}
	if byKind != synthTotal {
		t.Errorf("TotalByKind(synth) = %d, want %d", byKind, synthTotal)
	}

	byRef, err := l.TotalForRef("job-a")
	if err != nil {
		t.Fatalf("TotalForRef() error = %v, want nil", err)
	}
	if byRef != synthTotal {
		t.Errorf("TotalForRef(job-a) = %d, want %d", byRef, synthTotal)
	}

	other, err := l.TotalByKind("other")
	if err != nil {
		t.Fatalf("TotalByKind(other) error = %v, want nil", err)
	}
	if other != Dollar {
		t.Errorf("TotalByKind(other) = %d, want %d", other, Dollar)
	}

	missing, err := l.TotalByKind("missing")
	if err != nil {
		t.Fatalf("TotalByKind(missing) error = %v, want nil", err)
	}
	if missing != 0 {
		t.Errorf("TotalByKind(missing) = %d, want 0", missing)
	}

	if got := len(l.Charges()); got != writers*perWriter+1 {
		t.Errorf("len(Charges()) = %d, want %d", got, writers*perWriter+1)
	}
}

// TestBudgetConcurrentReservationsNeverOverspend runs 1000 concurrent
// reservations against a budget that admits 500 of them and checks the budget
// never commits more than its limit.
func TestBudgetConcurrentReservationsNeverOverspend(t *testing.T) {
	const unit = 1 * Cent
	const capacity = 500
	const attempts = 1000
	b := mustBudget(t, capacity*unit)

	var wg sync.WaitGroup
	var granted atomic.Int64
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := b.Reserve(unit)
			switch {
			case err == nil:
				granted.Add(1)
			case !errors.Is(err, ErrOverBudget):
				t.Errorf("Reserve() error = %v, want nil or ErrOverBudget", err)
			}
		}()
	}
	wg.Wait()

	if got := granted.Load(); got != capacity {
		t.Fatalf("granted reservations = %d, want %d", got, capacity)
	}
	if got := b.Reserved(); got != capacity*unit {
		t.Errorf("Reserved() = %s, want %s", got, (capacity * unit).String())
	}
	if got := mustRemaining(t, b); got != 0 {
		t.Errorf("Remaining() = %s, want $0.00", got)
	}
	if err := b.Reserve(unit); !errors.Is(err, ErrOverBudget) {
		t.Errorf("Reserve() past the limit error = %v, want ErrOverBudget", err)
	}
}

// TestBudgetSettleAndRelease checks that a settled call keeps its actual price
// and frees its reservation and that a released reservation returns its
// headroom.
func TestBudgetSettleAndRelease(t *testing.T) {
	b := mustBudget(t, 100*Cent)
	if got := b.Limit(); got != 100*Cent {
		t.Fatalf("Limit() = %d, want %d", got, 100*Cent)
	}

	if err := b.Reserve(30 * Cent); err != nil {
		t.Fatalf("Reserve(30c) error = %v, want nil", err)
	}
	if got := mustRemaining(t, b); got != 70*Cent {
		t.Fatalf("Remaining() = %d, want %d", got, 70*Cent)
	}

	// The call cost less than the estimate, so the headroom grows.
	if err := b.Settle(30*Cent, 25*Cent); err != nil {
		t.Fatalf("Settle(30c, 25c) error = %v, want nil", err)
	}
	if got := b.Spent(); got != 25*Cent {
		t.Errorf("Spent() = %d, want %d", got, 25*Cent)
	}
	if got := b.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got := mustRemaining(t, b); got != 75*Cent {
		t.Errorf("Remaining() = %d, want %d", got, 75*Cent)
	}

	if err := b.Reserve(50 * Cent); err != nil {
		t.Fatalf("Reserve(50c) error = %v, want nil", err)
	}
	b.Release(50 * Cent)
	if got := b.Reserved(); got != 0 {
		t.Errorf("Reserved() after Release = %d, want 0", got)
	}
	if got := mustRemaining(t, b); got != 75*Cent {
		t.Errorf("Remaining() after Release = %d, want %d", got, 75*Cent)
	}

	// A release larger than the outstanding total frees only what remains.
	if err := b.Reserve(10 * Cent); err != nil {
		t.Fatalf("Reserve(10c) error = %v, want nil", err)
	}
	b.Release(20 * Cent)
	if got := b.Reserved(); got != 0 {
		t.Errorf("Reserved() after over-release = %d, want 0", got)
	}
}

// TestLedgerTotalSumOverflow checks that a ledger whose charges each fit the
// int64 range but whose sum leaves it reports ErrOverflow from every total.
func TestLedgerTotalSumOverflow(t *testing.T) {
	half := Charge{Kind: "big", Units: math.MaxInt64 / 2, UnitPrice: 2, Ref: "big"}
	if _, err := half.Total(); err != nil {
		t.Fatalf("one charge Total() error = %v, want nil", err)
	}

	l := NewLedger()
	l.Add(half)
	l.Add(half)

	for name, total := range map[string]func() (Price, error){
		"Total":       l.Total,
		"TotalByKind": func() (Price, error) { return l.TotalByKind("big") },
		"TotalForRef": func() (Price, error) { return l.TotalForRef("big") },
	} {
		if _, err := total(); !errors.Is(err, ErrOverflow) {
			t.Errorf("%s() error = %v, want ErrOverflow", name, err)
		}
	}
}

// TestBudgetSettleOverflow checks that Settle reports ErrOverflow and books
// nothing when the actual price would push spent past either int64 edge.
func TestBudgetSettleOverflow(t *testing.T) {
	high := mustBudget(t, math.MaxInt64)
	if err := high.Settle(0, math.MaxInt64); err != nil {
		t.Fatalf("Settle(max) error = %v, want nil", err)
	}
	if err := high.Settle(0, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Settle past max error = %v, want ErrOverflow", err)
	}
	if got := high.Spent(); got != math.MaxInt64 {
		t.Errorf("Spent() after overflow = %d, want %d", got, math.MaxInt64)
	}

	low := mustBudget(t, 0)
	if err := low.Settle(0, math.MinInt64); err != nil {
		t.Fatalf("Settle(min) error = %v, want nil", err)
	}
	if err := low.Settle(0, -1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Settle below min error = %v, want ErrOverflow", err)
	}
	if got := low.Spent(); got != math.MinInt64 {
		t.Errorf("Spent() after underflow = %d, want %d", got, math.MinInt64)
	}
}

// mustBudget builds a budget or ends the test.
func mustBudget(t *testing.T, limit Price) *Budget {
	t.Helper()
	b, err := NewBudget(limit)
	if err != nil {
		t.Fatalf("NewBudget(%d) error = %v, want nil", limit, err)
	}
	return b
}

// mustRemaining reads the headroom or ends the test.
func mustRemaining(t *testing.T, b *Budget) Price {
	t.Helper()
	got, err := b.Remaining()
	if err != nil {
		t.Fatalf("Remaining() error = %v, want nil", err)
	}
	return got
}

// TestBudgetNegativeLimitRejected checks that a budget refuses a limit below
// zero, so no caller can build a budget whose headroom wraps.
func TestBudgetNegativeLimitRejected(t *testing.T) {
	if _, err := NewBudget(-1); !errors.Is(err, ErrNegativeLimit) {
		t.Errorf("NewBudget(-1) error = %v, want ErrNegativeLimit", err)
	}
	if _, err := NewBudget(math.MinInt64); !errors.Is(err, ErrNegativeLimit) {
		t.Errorf("NewBudget(min) error = %v, want ErrNegativeLimit", err)
	}
}

// TestBudgetRemainingSubtractOverflow checks that headroom arithmetic which
// leaves the int64 range reports ErrOverflow instead of a wrapped value.
func TestBudgetRemainingSubtractOverflow(t *testing.T) {
	// A settled negative price drives limit minus spent past math.MaxInt64, so
	// the difference cannot stay in range.
	low := mustBudget(t, 0)
	if err := low.Settle(0, math.MinInt64); err != nil {
		t.Fatalf("Settle(min) error = %v, want nil", err)
	}
	if _, err := low.Remaining(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Remaining() past max error = %v, want ErrOverflow", err)
	}
	if err := low.Reserve(1); !errors.Is(err, ErrOverflow) {
		t.Errorf("Reserve() over unrepresentable headroom error = %v, want ErrOverflow", err)
	}
	if got := low.Reserved(); got != 0 {
		t.Errorf("Reserved() after refused reserve = %d, want 0", got)
	}

	// A settled spend that overtakes outstanding reservations drives the
	// subtraction below math.MinInt64, so it cannot stay in range either.
	high := mustBudget(t, 0)
	if err := high.Settle(0, -2); err != nil {
		t.Fatalf("Settle(-2) error = %v, want nil", err)
	}
	if err := high.Reserve(2); err != nil {
		t.Fatalf("Reserve(2) error = %v, want nil", err)
	}
	if err := high.Settle(0, math.MaxInt64); err != nil {
		t.Fatalf("Settle(max) error = %v, want nil", err)
	}
	if err := high.Settle(0, 2); err != nil {
		t.Fatalf("Settle(2) error = %v, want nil", err)
	}
	if _, err := high.Remaining(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Remaining() below min error = %v, want ErrOverflow", err)
	}
}

// TestBudgetRemainingExactHeadroom checks that a representable headroom is
// reported exactly and can be reserved, even when the limit minus spent alone
// leaves the int64 range. The outstanding reservation pulls the difference
// back into range, so subtracting spent first would report a spurious error.
func TestBudgetRemainingExactHeadroom(t *testing.T) {
	b := mustBudget(t, 1)
	if err := b.Settle(0, -2); err != nil {
		t.Fatalf("Settle(-2) error = %v, want nil", err)
	}
	if err := b.Reserve(3); err != nil {
		t.Fatalf("Reserve(3) error = %v, want nil", err)
	}
	if err := b.Settle(0, math.MinInt64+2); err != nil {
		t.Fatalf("Settle(min+2) error = %v, want nil", err)
	}
	want := Price(math.MaxInt64 - 1)
	if got := mustRemaining(t, b); got != want {
		t.Errorf("Remaining() = %d, want %d", got, want)
	}
	if err := b.Reserve(1); err != nil {
		t.Errorf("Reserve(1) error = %v, want nil", err)
	}
	if got := b.Reserved(); got != 4 {
		t.Errorf("Reserved() after granted reserve = %d, want 4", got)
	}
}

// TestBudgetReserveAccumulatorOverflow checks that Reserve refuses an estimate
// whose commit would push the outstanding total past the int64 range, even
// when the headroom admits the estimate, and that it stores nothing.
func TestBudgetReserveAccumulatorOverflow(t *testing.T) {
	b := mustBudget(t, 1)
	if err := b.Settle(0, -2); err != nil {
		t.Fatalf("Settle(-2) error = %v, want nil", err)
	}
	if err := b.Reserve(3); err != nil {
		t.Fatalf("Reserve(3) error = %v, want nil", err)
	}
	if err := b.Settle(0, math.MinInt64+2); err != nil {
		t.Fatalf("Settle(min+2) error = %v, want nil", err)
	}
	// The headroom is math.MaxInt64-1, so the range check admits the estimate,
	// but three outstanding plus it cannot fit the int64 range.
	if err := b.Reserve(math.MaxInt64 - 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("Reserve(max-1) error = %v, want ErrOverflow", err)
	}
	if got := b.Reserved(); got != 3 {
		t.Errorf("Reserved() after refused reserve = %d, want 3", got)
	}
}

// TestBudgetReserveNegativeEstimate checks that a reservation with a negative
// estimate is refused and never grows the reserved accumulator.
func TestBudgetReserveNegativeEstimate(t *testing.T) {
	b := mustBudget(t, 0)
	if err := b.Reserve(-math.MaxInt64); !errors.Is(err, ErrNegativeEstimate) {
		t.Fatalf("Reserve(-MaxInt64) error = %v, want ErrNegativeEstimate", err)
	}
	if got := b.Reserved(); got != 0 {
		t.Errorf("Reserved() after refused reserve = %d, want 0", got)
	}

	// A caller that hands a negative reservation to Release or Settle must not
	// grow the accumulator either.
	b.Release(-math.MaxInt64)
	b.Settle(-math.MaxInt64, 0)
	if got := b.Reserved(); got != 0 {
		t.Errorf("Reserved() after negative releases = %d, want 0", got)
	}
	if got := mustRemaining(t, b); got != 0 {
		t.Errorf("Remaining() after negative releases = %d, want 0", got)
	}
}

// TestLedgerTotalExactSumOrders checks that a ledger total depends only on
// its charges and not on the order they were added, even when a running
// partial sum leaves the int64 range. These three charges have an exact total
// of MaxInt64, which fits, so every accessor must return it.
func TestLedgerTotalExactSumOrders(t *testing.T) {
	pos := Charge{Kind: "k", Units: 1, UnitPrice: math.MaxInt64, Ref: "r"}
	neg := Charge{Kind: "k", Units: 1, UnitPrice: -math.MaxInt64, Ref: "r"}
	want := Price(math.MaxInt64)
	orders := map[string][]Charge{
		"positive first": {pos, pos, neg},
		"negative first": {neg, pos, pos},
	}
	for name, order := range orders {
		l := NewLedger()
		for _, c := range order {
			l.Add(c)
		}
		for accessor, total := range map[string]func() (Price, error){
			"Total":       l.Total,
			"TotalByKind": func() (Price, error) { return l.TotalByKind("k") },
			"TotalForRef": func() (Price, error) { return l.TotalForRef("r") },
		} {
			got, err := total()
			if err != nil || got != want {
				t.Errorf("%s %s() = %d, %v, want %d, nil", name, accessor, got, err, want)
			}
		}
	}
}

// TestLedgerTotalExactSumOverflow checks that a sum which does not fit int64
// still reports ErrOverflow from every accessor, including when the big
// integer accumulator holds it.
func TestLedgerTotalExactSumOverflow(t *testing.T) {
	huge := Charge{Kind: "k", Units: 1, UnitPrice: math.MaxInt64, Ref: "r"}
	l := NewLedger()
	l.Add(huge)
	l.Add(huge)
	l.Add(huge)
	for accessor, total := range map[string]func() (Price, error){
		"Total":       l.Total,
		"TotalByKind": func() (Price, error) { return l.TotalByKind("k") },
		"TotalForRef": func() (Price, error) { return l.TotalForRef("r") },
	} {
		if _, err := total(); !errors.Is(err, ErrOverflow) {
			t.Errorf("%s() error = %v, want ErrOverflow", accessor, err)
		}
	}
}

// TestLedgerTotalFastPathAllocatesNothing checks that an ordinary in-range
// total stays on the int64 path and allocates nothing, so the big integer
// accumulator is reached only when the exact sum leaves the range.
func TestLedgerTotalFastPathAllocatesNothing(t *testing.T) {
	l := NewLedger()
	l.Add(Charge{Kind: "k", Units: 1, UnitPrice: Cent, Ref: "r"})
	l.Add(Charge{Kind: "k", Units: 2, UnitPrice: Cent, Ref: "r"})
	for accessor, total := range map[string]func() (Price, error){
		"Total":       l.Total,
		"TotalByKind": func() (Price, error) { return l.TotalByKind("k") },
		"TotalForRef": func() (Price, error) { return l.TotalForRef("r") },
	} {
		allocs := testing.AllocsPerRun(100, func() {
			if _, err := total(); err != nil {
				t.Fatalf("%s() error = %v, want nil", accessor, err)
			}
		})
		if allocs != 0 {
			t.Errorf("%s() allocations = %v, want 0", accessor, allocs)
		}
	}
}

// TestLedgerTotalCompensatingProductOverflow checks that a ledger whose exact
// total fits int64 is not refused when a single charge's own product does not
// fit. The overflow charge bills two units at MaxInt64, the credit cancels it,
// and the exact total is MaxInt64.
func TestLedgerTotalCompensatingProductOverflow(t *testing.T) {
	over := Charge{Kind: "k", Units: 2, UnitPrice: math.MaxInt64, Ref: "r"}
	credit := Charge{Kind: "k", Units: 1, UnitPrice: -math.MaxInt64, Ref: "r"}
	want := Price(math.MaxInt64)
	orders := map[string][]Charge{
		"overflow first": {over, credit},
		"credit first":   {credit, over},
	}
	for name, order := range orders {
		l := NewLedger()
		for _, c := range order {
			l.Add(c)
		}
		checkTotal(t, name, l, want, false)
	}
}

// TestLedgerTotalExactProductSumOverflow checks that the exact-sum contract
// still reports ErrOverflow when a charge's product overflows and the exact
// total does not fit either.
func TestLedgerTotalExactProductSumOverflow(t *testing.T) {
	l := NewLedger()
	l.Add(Charge{Kind: "k", Units: 2, UnitPrice: math.MaxInt64, Ref: "r"})
	l.Add(Charge{Kind: "k", Units: 1, UnitPrice: math.MaxInt64, Ref: "r"})
	checkTotal(t, "product and sum overflow", l, 0, true)
}

// checkTotal runs every ledger accessor and checks the result. It reports
// ErrOverflow as required when wantOverflow holds, and otherwise the exact
// value with a nil error.
func checkTotal(t *testing.T, name string, l *Ledger, want Price, wantOverflow bool) {
	t.Helper()
	for accessor, total := range map[string]func() (Price, error){
		"Total":       l.Total,
		"TotalByKind": func() (Price, error) { return l.TotalByKind("k") },
		"TotalForRef": func() (Price, error) { return l.TotalForRef("r") },
	} {
		got, err := total()
		if wantOverflow {
			if !errors.Is(err, ErrOverflow) {
				t.Errorf("%s %s() error = %v, want ErrOverflow", name, accessor, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s %s() = %d, %v, want %d, nil", name, accessor, got, err, want)
		}
	}
}

// TestLedgerTotalMatchesExactModel drives random multisets of charges through
// every accessor in both insertion orders and compares each result against the
// exact sum computed with a big integer. The pools cover mixed signs, peak
// magnitudes, zero-unit charges and product overflows that cancel.
func TestLedgerTotalMatchesExactModel(t *testing.T) {
	units := []int{0, 1, -1, 2, -2, 3, -3, math.MaxInt64, math.MinInt64, 1 << 40, -(1 << 40)}
	prices := []Price{0, 1, -1, 2, -2, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1, math.MaxInt64 / 2, -(math.MaxInt64 / 2), 1 << 40, -(1 << 40), 3037000499, -3037000499, 3037000500, -3037000500}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		n := rng.Intn(5)
		charges := make([]Charge, 0, n)
		for j := 0; j < n; j++ {
			charges = append(charges, Charge{
				Kind:      "k",
				Units:     units[rng.Intn(len(units))],
				UnitPrice: prices[rng.Intn(len(prices))],
				Ref:       "r",
			})
		}
		exact := new(big.Int)
		for _, c := range charges {
			product := new(big.Int).Mul(big.NewInt(int64(c.Units)), big.NewInt(int64(c.UnitPrice)))
			exact.Add(exact, product)
		}
		wantOverflow := !exact.IsInt64()
		var want Price
		if !wantOverflow {
			want = Price(exact.Int64())
		}
		for _, order := range [][]Charge{charges, reverseCharges(charges)} {
			l := NewLedger()
			for _, c := range order {
				l.Add(c)
			}
			checkTotal(t, fmt.Sprintf("case %d", i), l, want, wantOverflow)
		}
	}
}

func reverseCharges(charges []Charge) []Charge {
	out := make([]Charge, len(charges))
	for i := range charges {
		out[len(charges)-1-i] = charges[i]
	}
	return out
}
