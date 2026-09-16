package source

import (
	"context"

	"github.com/nrynss/keel/config"
)

// file resolves a secret from a file whose whole content is the value.
type file struct{}

func (file) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	if _, err := startCtx(ctx, "file", ref); err != nil {
		return "", err
	}
	if ref.Path() == "" {
		return "", annotate(ErrMissingLocator, "file", ref)
	}
	val, err := readSecretFile(ref.Path(), true)
	if err != nil {
		return "", notFound(err, "file", ref)
	}
	return val, nil
}
