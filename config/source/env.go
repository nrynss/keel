package source

import (
	"context"

	"github.com/nrynss/keel/config"
)

// env resolves a secret from an environment variable named by the
// reference's var field.
type env struct {
	lookup func(string) (string, bool)
}

func (e env) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	if _, err := startCtx(ctx, "env", ref); err != nil {
		return "", err
	}
	name := ref.Var()
	if name == "" {
		return "", annotate(ErrMissingLocator, "env", ref)
	}
	val, ok := lookupEnv(e.lookup)(name)
	if !ok {
		return "", annotate(ErrNotFound, "env", ref)
	}
	return val, nil
}
