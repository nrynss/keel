package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// ErrInlineSecret reports a secret setting that carries a literal value where
// a reference belongs. The loader names the key, so a value cannot be
// committed to a settings file by accident.
var ErrInlineSecret = errors.New("config: a secret setting must be a reference, not a literal value")

// ErrUnresolved reports an attempt to read a Secret that no loader resolved.
var ErrUnresolved = errors.New("config: secret was never resolved")

// redacted replaces every secret value on a formatting path. The same fixed
// bytes stay in place of the value, so a caller learns nothing from the
// output beyond the fact that a secret sits there.
const redacted = "<secret>"

// Secret is one secret setting. It decodes from a reference table in the
// settings file, holding the ordered list of references that name the value.
// A loader resolves it and replaces the references with the resolved value.
//
// No formatting path prints the value. String, Format, GoString, the JSON
// and text marshallers, and the slog value all print a fixed placeholder.
// Reveal is the one way to read the value, and it reads as a deliberate act
// at the call site.
//
// The zero value is usable. Its Reveal returns ErrUnresolved.
type Secret struct {
	refs   []Ref
	reveal func() (string, error)
	scan   refScan
}

// UnmarshalTOML decodes a reference, or an ordered list of them, from a TOML
// table, an inline table, or an inline array. go-toml calls it only when the
// decoder enabled the unmarshaler interface. A bare value is refused.
//
// An array of tables arrives as one call per element, so the references
// accumulate in document order. The scan context recorded by Decode lets a
// failure name the setting key and report the position in the file.
func (s *Secret) UnmarshalTOML(data []byte) error {
	scan := s.scan
	scan.fragLine, scan.fragCol, scan.found = locateFragment(scan, data)
	refs, err := parseRefs(data, scan)
	if err != nil {
		return err
	}
	s.refs = append(s.refs, refs...)
	return nil
}

// Reveal returns the resolved value. It returns ErrUnresolved when no loader
// resolved this Secret, and it returns the resolution failure when an at-use
// reference cannot be read.
func (s Secret) Reveal() (string, error) {
	if s.reveal == nil {
		return "", ErrUnresolved
	}
	return s.reveal()
}

// String returns the placeholder, never the value.
func (s Secret) String() string { return redacted }

// Format writes the placeholder for every verb, never the value.
func (s Secret) Format(f fmt.State, verb rune) {
	// The write cannot fail for a fmt.State, and there is no value to lose.
	_, _ = io.WriteString(f, redacted)
}

// GoString returns the placeholder, never the value.
func (s Secret) GoString() string { return redacted }

// MarshalJSON returns the placeholder as a JSON string, never the value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(redacted)), nil }

// MarshalText returns the placeholder, never the value.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue returns the placeholder, so a logged Secret carries no value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
