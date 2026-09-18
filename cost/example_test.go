package cost_test

import (
	"fmt"

	"github.com/nrynss/keel/cost"
)

// ExampleLedger records two charges and reports the whole total and one ref's
// share.
func ExampleLedger() {
	ledger := cost.NewLedger()
	ledger.Add(cost.Charge{Kind: "tts", Units: 1200, UnitPrice: cost.Microdollar, Ref: "job-1"})
	ledger.Add(cost.Charge{Kind: "llm", Units: 1, UnitPrice: cost.USD(0.25), Ref: "job-2"})

	total, err := ledger.Total()
	if err != nil {
		fmt.Println("total failed:", err)
		return
	}
	byRef, err := ledger.TotalForRef("job-1")
	if err != nil {
		fmt.Println("total for ref failed:", err)
		return
	}

	fmt.Println(total)
	fmt.Println(byRef)
	// Output:
	// $0.2512
	// $0.0012
}

// ExampleKeyedBudget gives two owners a share of one dollar and spends one
// owner's share only.
func ExampleKeyedBudget() {
	budget, err := cost.NewKeyedBudget(cost.USD(1.00))
	if err != nil {
		fmt.Println("keyed budget failed:", err)
		return
	}
	if err := budget.SetLimit("team-a", cost.USD(0.40)); err != nil {
		fmt.Println("owner limit failed:", err)
		return
	}
	if err := budget.SetLimit("team-b", cost.USD(0.40)); err != nil {
		fmt.Println("owner limit failed:", err)
		return
	}
	if err := budget.Reserve("team-a", cost.USD(0.10)); err != nil {
		fmt.Println("reserve failed:", err)
		return
	}
	if err := budget.Settle("team-a", cost.USD(0.10), cost.USD(0.08)); err != nil {
		fmt.Println("settle failed:", err)
		return
	}

	forTeamA, err := budget.Remaining("team-a")
	if err != nil {
		fmt.Println("remaining failed:", err)
		return
	}
	forTeamB, err := budget.Remaining("team-b")
	if err != nil {
		fmt.Println("remaining failed:", err)
		return
	}

	fmt.Println(forTeamA)
	fmt.Println(forTeamB)
	// Output:
	// $0.32
	// $0.40
}
