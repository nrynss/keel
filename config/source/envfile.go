package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/nrynss/keel/config"
)

// envFile resolves a secret from a KEY=value file named by path, taking
// the entry named by var.
type envFile struct{}

func (envFile) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	if _, err := startCtx(ctx, "env_file", ref); err != nil {
		return "", err
	}
	if ref.Path() == "" || ref.Var() == "" {
		return "", annotate(ErrMissingLocator, "env_file", ref)
	}
	body, err := readSecretFile(ref.Path(), false)
	if err != nil {
		return "", notFound(err, "env_file", ref)
	}
	vars, err := parseEnvFile([]byte(body))
	if err != nil {
		return "", annotate(err, "env_file", ref)
	}
	val, ok := vars[ref.Var()]
	if !ok {
		return "", annotate(ErrNotFound, "env_file", ref)
	}
	if quoted(val) {
		return "", annotate(fmt.Errorf("%w: var %q: remove the quotes", ErrQuotedValue, ref.Var()), "env_file", ref)
	}
	return val, nil
}

// parseEnvFile reads KEY=value lines. A value is the literal text after
// the first equals. One trailing carriage return is trimmed from each
// line, so a CRLF blank is blank. A data line with no equals is a newline
// in a value, and is refused.
func parseEnvFile(data []byte) (map[string]string, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	out := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trim := strings.TrimLeft(line, " \t")
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		key, val, ok := strings.Cut(trim, "=")
		if !ok {
			return nil, ErrNewline
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.TrimSuffix(val, "\r")
		if strings.ContainsAny(val, "\n\r") {
			return nil, ErrNewline
		}
		out[key] = val
	}
	return out, nil
}

// quoted reports whether s is wrapped in a matching pair of ASCII quotes.
func quoted(s string) bool {
	if len(s) < 2 {
		return false
	}
	q := s[0]
	if q != '"' && q != '\'' {
		return false
	}
	return s[len(s)-1] == q
}
