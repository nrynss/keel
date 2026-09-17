package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ErrRequired reports a required setting that is still zero after every
// layer. The message names the key, its source, and its locator.
var ErrRequired = errors.New("config: required key has no value")

// ErrResolve reports a secret whose references were tried in order and
// all failed. The message names the key, the last source, and its locator.
var ErrResolve = errors.New("config: cannot resolve secret")

// layer names recorded on the plan. Secret lines use the reference source
// instead of a layer name.
const (
	sourceDefault = "default"
	sourceFile    = "file"
	sourceEnv     = "env"
	sourceFlag    = "flag"
	sourceNone    = "none"
)

// Config configures Load. The zero value is usable: no file, no flags, and
// a nil lookup that means os.LookupEnv. A nil Registry refuses every secret
// reference, the same as Decode.
type Config struct {
	// Path is the settings file to load. Empty means consult PathVar, then
	// Search.
	Path string
	// PathVar names the environment variable whose value is the settings
	// file path. Empty means the lookup is skipped. The value is used only
	// when Path is empty.
	PathVar string
	// Search is tried in order when Path and PathVar yield no path. The
	// first existing file wins. A missing candidate is skipped.
	Search []string
	// LookupEnv looks up an environment variable. Nil means os.LookupEnv.
	LookupEnv func(name string) (string, bool)
	// Flags holds explicit overrides. Keys are field paths such as
	// render.max_seconds, or the derived override key RENDER_MAX_SECONDS.
	// They win last. A flag cannot set a secret.
	Flags map[string]string
	// Registry holds the secret sources. The caller registers them and
	// passes the registry here.
	Registry *Registry
}

// Load fills dst from defaults, the settings file, environment overrides,
// and flags, in that order. dst is a pointer to the application's settings
// struct. Values already in dst are the defaults.
//
// Only non-secret settings take an environment override. The override key
// is the field path in upper case, with dots and hyphens turned into
// underscores, so render.max_seconds reads RENDER_MAX_SECONDS.
//
// A missing file is not an error. A present file that cannot be read or
// parsed is fatal. The loader trial resolves every secret at load, including
// a reference marked at_use. An at_boot Reveal returns the cached trial
// value, and an at_use Reveal resolves again on each call. The trial winner
// decides the mode for the whole list. An at_boot winner stays cached even
// when a later reference reads at_use. An at_use winner re-resolves the full
// ordered list on each Reveal, including references that read at_boot.
//
// The live reader keeps the load context values through context.WithoutCancel.
// Cancellation and deadline do not survive into Reveal. Load with a context
// that carries only long lived values.
//
// Do not mutate the registry after load while an at_use secret lives. Each
// Reveal looks sources up again, so a late Register races with Reveal.
//
// A field tagged config:"required" must be non-zero after every layer. A
// secret with references must resolve. Failures name the key, the source,
// and the locator, and never carry a value.
func Load(ctx context.Context, dst any, cfg Config) (Plan, error) {
	el, err := destStruct(dst)
	if err != nil {
		return Plan{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, fmt.Errorf("config: %w", err)
	}
	lookup := lookupEnv(cfg.LookupEnv)
	origins := make(map[string]origin)
	secretKeys := make(map[string]bool)
	if err := collectKeys(el, el.Type(), nil, reflect.StructField{}, origins, secretKeys); err != nil {
		return Plan{}, err
	}

	path := choosePath(cfg, lookup)
	if path != "" {
		data, ok, err := readSettings(path)
		if err != nil {
			return Plan{}, err
		}
		if ok {
			if err := Decode(data, dst, cfg.Registry); err != nil {
				return Plan{}, err
			}
			if err := collectKeys(el, el.Type(), nil, reflect.StructField{}, origins, secretKeys); err != nil {
				return Plan{}, err
			}
			var tree map[string]any
			if err := toml.Unmarshal(data, &tree); err != nil {
				return Plan{}, fmt.Errorf("%w: %s", ErrMalformed, trimErr(err))
			}
			markFileOrigins(el.Type(), tree, nil, path, origins)
		}
	}

	if err := applyEnv(el, el.Type(), nil, lookup, origins); err != nil {
		return Plan{}, err
	}
	if err := applyFlags(el, el.Type(), nil, cfg.Flags, origins, secretKeys); err != nil {
		return Plan{}, err
	}
	secretPlans, err := resolveSecrets(ctx, el, el.Type(), nil, reflect.StructField{}, cfg.Registry)
	if err != nil {
		return Plan{}, err
	}
	if err := checkRequired(el, el.Type(), nil, reflect.StructField{}, origins, secretPlans); err != nil {
		return Plan{}, err
	}
	return buildPlan(el, el.Type(), nil, reflect.StructField{}, origins, secretPlans), nil
}

// destStruct reports the struct value behind a pointer destination.
func destStruct(dst any) (reflect.Value, error) {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, ErrInvalidDest
	}
	el := rv.Elem()
	if el.Kind() != reflect.Struct {
		return reflect.Value{}, ErrInvalidDest
	}
	return el, nil
}

// lookupEnv returns f, or os.LookupEnv when f is nil.
func lookupEnv(f func(string) (string, bool)) func(string) (string, bool) {
	if f != nil {
		return f
	}
	return os.LookupEnv
}

// choosePath picks the settings file. An explicit Path wins. PathVar is
// looked up next. Search is the last resort. A missing search candidate is
// skipped. A candidate that exists is returned even if it cannot be read,
// so the caller can fail it as a present file.
func choosePath(cfg Config, lookup func(string) (string, bool)) string {
	if cfg.Path != "" {
		return cfg.Path
	}
	if cfg.PathVar != "" {
		if v, ok := lookup(cfg.PathVar); ok && v != "" {
			return v
		}
	}
	for _, p := range cfg.Search {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return p
		}
		if st.IsDir() {
			return p
		}
		return p
	}
	return ""
}

// readSettings reads a settings file. A missing path is not an error. A
// present directory or an unreadable file is fatal.
func readSettings(path string) ([]byte, bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("config: cannot stat %s: %w", path, err)
	}
	if st.IsDir() {
		return nil, false, fmt.Errorf("config: settings file %s is a directory", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("config: cannot read %s: %w", path, err)
	}
	return data, true, nil
}

// trimErr strips a leading "toml: " so a wrapped parse failure stays short.
func trimErr(err error) string {
	return strings.TrimPrefix(err.Error(), "toml: ")
}
