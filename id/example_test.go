package id_test

import (
	"fmt"

	"github.com/nrynss/keel/id"
)

// ExampleNew generates an id and shows Valid accepting it and rejecting a
// string that is not one.
func ExampleNew() {
	newID, err := id.New()
	if err != nil {
		fmt.Println("generate failed:", err)
		return
	}

	fmt.Println(len(newID))
	fmt.Println(id.Valid(newID))
	fmt.Println(id.Valid("not-an-id"))
	// Output:
	// 32
	// true
	// false
}
