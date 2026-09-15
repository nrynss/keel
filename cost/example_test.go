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
