package config

import (
	"context"
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
// A loader trial resolves every reference at load and installs the reader
// Reveal uses. The trial winner decides the mode for the whole list. An
// at_boot winner returns the cached trial value. An at_use winner resolves
// the ordered list again on each call, so rotation takes effect without a
// restart.
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

// Reveal returns the secret value. It returns ErrUnresolved when no loader
// resolved this Secret. An at_boot Secret returns the value the load cached.
// An at_use Secret resolves its ordered references again on each call and
// returns the first success. A use time failure returns an error and never
// serves a cached value.
func (s Secret) Reveal() (string, error) {
	if s.reveal == nil {
		return "", ErrUnresolved
	}
	return s.reveal()
}

// bind installs a Reveal that returns value. The loader uses it for a secret
// whose winning reference reads at_boot.
func (s *Secret) bind(value string) {
	s.reveal = func() (string, error) { return value, nil }
}

// bindLive installs a Reveal that resolves the ordered references on each
// call. The loader uses it for a secret whose winning reference reads at_use.
// Each call tries the references in order and returns the first success. A
// call that finds no success returns an error naming the key. It never serves
// a cached value. The reader keeps the load context values through
// context.WithoutCancel. Cancellation and deadline do not survive into the
// call, so load with a context that carries only long lived values.
func (s *Secret) bindLive(key string, refs []Ref, reg *Registry, ctx context.Context) {
	liveRefs := append([]Ref(nil), refs...)
	live := context.Background()
	if ctx != nil {
		live = context.WithoutCancel(ctx)
	}
	s.reveal = func() (string, error) {
		var last error
		var lastRef Ref
		for _, ref := range liveRefs {
			src, ok := reg.Lookup(ref.Source())
			if !ok {
				last = fmt.Errorf("%w: %q", ErrUnknownSource, ref.Source())
				lastRef = ref
				continue
			}
			val, err := src.Resolve(live, ref)
			if err != nil {
				last = err
				lastRef = ref
				continue
			}
			return val, nil
		}
		loc := lastRef.locator()
		if loc == "" {
			loc = sourceNone
		}
		srcName := lastRef.Source()
		if srcName == "" {
			srcName = sourceNone
		}
		if last == nil {
			last = ErrUnresolved
		}
		return "", fmt.Errorf("%w: %s (source %s locator %s): %w", ErrResolve, key, srcName, loc, last)
	}
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
