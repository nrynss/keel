package id

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// TestNewDrawsDistinctValidIds pins the guarantee URLs rest on: every
// draw is 32 lowercase hex characters, and 100000 draws collide with
// nothing. 128 bits make a collision a non-event, so a repeat here
// means the entropy path is broken, not unlucky.
func TestNewDrawsDistinctValidIds(t *testing.T) {
	const n = 100000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if len(id) != idLen {
			t.Fatalf("New returned %d characters, want %d", len(id), idLen)
		}
		if !Valid(id) {
			t.Fatalf("New returned %q, which fails Valid", id)
		}
		if seen[id] {
			t.Fatalf("New repeated id %q", id)
		}
		seen[id] = true
	}
}

// TestValid pins the shape check a caller trusts before it looks an
// id up: exactly 32 lowercase hex characters and nothing else.
func TestValid(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"canonical", "0123456789abcdef0123456789abcdef", true},
		{"empty", "", false},
		{"half length", "0123456789abcdef", false},
		{"one short", "0123456789abcdef0123456789abcde", false},
		{"one long", "0123456789abcdef0123456789abcdef0", false},
		{"uppercase", "0123456789ABCDEF0123456789ABCDEF", false},
		{"non hex", "0123456789abcdef0123456789abcdeg", false},
		{"path separator", "0123456789abcdef/0123456789abcdef", false},
		{"whitespace", " 0123456789abcdef0123456789abcdef", false},
		{"traversal", "../../../../etc/passwd\x00\x00\x00\x00\x00aa", false},
	}
	for _, c := range cases {
		if got := Valid(c.id); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestNewFromEncodesDrawnBytes pins the encoding path against a
// known source: exactly 16 bytes are drawn and hex-encoded
// lowercase.
func TestNewFromEncodesDrawnBytes(t *testing.T) {
	src := bytes.Repeat([]byte{0xa5}, 16)
	id, err := newFrom(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("newFrom: %v", err)
	}
	if want := hex.EncodeToString(src); id != want {
		t.Fatalf("newFrom = %q, want %q", id, want)
	}
}

// errSourceDied stands in for an entropy source that broke.
var errSourceDied = errors.New("entropy source died")

// dyingReader delivers n bytes, then fails every later Read. It
// models both a source dead on arrival and one cut off mid draw.
type dyingReader struct {
	n   int
	err error
}

func (r *dyingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, r.err
	}
	give := min(r.n, len(p))
	for i := range give {
		p[i] = byte(i)
	}
	r.n -= give
	return give, nil
}

// TestNewFromFailingReaderReturnsError pins the failure path: a
// broken or short entropy source returns a wrapped error, never a
// panic and never an id alongside the error.
func TestNewFromFailingReaderReturnsError(t *testing.T) {
	for _, n := range []int{0, 4, 15} {
		id, err := newFrom(&dyingReader{n: n, err: errSourceDied})
		if id != "" {
			t.Fatalf("newFrom returned %q alongside an error", id)
		}
		if err == nil {
			t.Fatalf("newFrom succeeded with %d of 16 bytes delivered", n)
		}
		if !errors.Is(err, errSourceDied) {
			t.Fatalf("err = %v, want it to wrap the source failure", err)
		}
	}
}
