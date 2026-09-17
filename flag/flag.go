// Package flag declares runtime flags an operator flips while the process
// runs. A flag has a name, a boolean or a small text value, and the time it
// last changed. Every read goes through to the store, because a flag check
// is one indexed row and a cache adds an invalidation nobody would test. A
// flag missing from the store reads as its declared default, so a row that
// does not exist yet is never a silent off.
package flag

import (
	"context"
	"errors"
	"time"
)

// ErrInvalid is returned for a declaration this package cannot honour, such
// as an empty name.
var ErrInvalid = errors.New("flag: invalid declaration")

// ErrKind is returned when the stored value of a flag does not match the
// kind its declaration states, for example a text value read as a bool.
var ErrKind = errors.New("flag: value kind does not match the declaration")

// Bool declares one boolean runtime flag. The zero value is not useful, a
// declaration always carries a name.
type Bool struct {
	// Name identifies the flag. It must not be empty.
	Name string

	// Default is what the flag reads as before anyone has set it.
	Default bool

	// Help explains what the flag switches, for operator tooling.
	Help string
}

// Text declares one small-text runtime flag. The zero value is not useful, a
// declaration always carries a name.
type Text struct {
	// Name identifies the flag. It must not be empty.
	Name string

	// Default is what the flag reads as before anyone has set it. It may
	// be the empty string.
	Default string

	// Help explains what the flag switches, for operator tooling.
	Help string
}

// Store reads and writes flag values through to their persistence. The
// sqlitestore package provides one over SQLite, and a test can supply its
// own. Every read resolves the value now, and a name with no stored value
// reads as the declaration's default with the zero time.
type Store interface {
	// Bool reads f and reports its value and the time it last changed.
	Bool(ctx context.Context, f Bool) (bool, time.Time, error)

	// SetBool stores value for f and reports the change time. A cancelled
	// context stops the write, so the flag keeps its old value.
	SetBool(ctx context.Context, f Bool, value bool) (time.Time, error)

	// Text reads f and reports its value and the time it last changed.
	Text(ctx context.Context, f Text) (string, time.Time, error)

	// SetText stores value for f and reports the change time. A cancelled
	// context stops the write, so the flag keeps its old value.
	SetText(ctx context.Context, f Text, value string) (time.Time, error)
}
