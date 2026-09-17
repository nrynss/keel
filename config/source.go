package config

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrUnknownSource reports a reference whose source name no registry holds.
var ErrUnknownSource = errors.New("config: unknown secret source")

// ErrInvalidSourceName reports a Register call whose name is empty or is not
// a plain identifier.
var ErrInvalidSourceName = errors.New("config: invalid source name")

// ErrNilSource reports a Register call that carries no source.
var ErrNilSource = errors.New("config: nil source")

// ErrDuplicateSource reports a Register call for a name that is already
// taken.
var ErrDuplicateSource = errors.New("config: duplicate source")

// ErrNilRegistry reports a Register call on a nil Registry.
var ErrNilRegistry = errors.New("config: nil registry")

// Source resolves a reference to the secret value it names. The built-in
// sources cover the mechanisms an operator already uses. An application
// implements Source for a mechanism of its own and registers it by name.
//
// A Source returns the value alone. It never returns an error alongside a
// value, and its errors name the reference without carrying the value.
type Source interface {
	// Resolve reads the secret ref locates and returns its value.
	Resolve(ctx context.Context, ref Ref) (string, error)
}

// Registry maps a source name to the Source that resolves it. Create one
// with NewRegistry, register the built-in and application sources, and pass
// it to the loader. Do not mutate a registry after load while an at_use
// secret lives. Each Reveal looks sources up again, so a late Register
// races with Reveal.
//
// The zero value is usable and holds no sources. A nil *Registry is usable
// too, holds no sources, and refuses every registration.
type Registry struct {
	sources map[string]Source
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{sources: make(map[string]Source)}
}

// Register binds a source to a name. It refuses an empty name, a nil source,
// a name that is not a plain identifier, and a name already taken. Binding a
// name twice is a mistake worth naming, so it fails rather than replaces.
func (r *Registry) Register(name string, s Source) error {
	if r == nil {
		return ErrNilRegistry
	}
	if name == "" || !isSourceName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidSourceName, name)
	}
	if s == nil {
		return fmt.Errorf("%w for %q", ErrNilSource, name)
	}
	if _, ok := r.sources[name]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateSource, name)
	}
	r.sources[name] = s
	return nil
}

// Lookup returns the source registered under name.
func (r *Registry) Lookup(name string) (Source, bool) {
	if r == nil {
		return nil, false
	}
	s, ok := r.sources[name]
	return s, ok
}

// names returns the registered source names in sorted order.
func (r *Registry) names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.sources))
	for name := range r.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isSourceName reports whether name is a plain identifier. A reference names
// its source this way, so a stray character fails at registration.
func isSourceName(name string) bool {
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		case c >= 'A' && c <= 'Z':
		default:
			return false
		}
		if i == 0 && c >= '0' && c <= '9' {
			return false
		}
	}
	return true
}
