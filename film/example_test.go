package film_test

import (
	"fmt"
	"strings"
	"time"

	"github.com/nrynss/keel/film"
)

func ExampleLayoutCaption() {
	got := film.LayoutCaption("Mira opens the garden gate.")
	fmt.Println(got.FontSize)
	fmt.Println(strings.Join(got.Lines, " "))
	// Output:
	// 60
	// Mira opens the garden gate.
}

func ExampleTotal() {
	d, err := film.Total(film.Config{}, []film.Page{{Text: "Hello"}})
	if err != nil {
		fmt.Println("total failed:", err)
		return
	}
	fmt.Println(d == film.DefaultTitleHold+film.DefaultEndHold+4*time.Second)
	// Output:
	// true
}

func ExampleSilentHold() {
	fmt.Println(film.SilentHold("one two three four"))
	fmt.Println(film.SilentHold(strings.Repeat("word ", 40)))
	// Output:
	// 4s
	// 14s
}
