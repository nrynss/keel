package source

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/nrynss/keel/config"
)

// dir resolves a secret from a named entry in a directory. An empty path
// uses CREDENTIALS_DIRECTORY from the injected lookup, which is how
// systemd LoadCredential presents credentials.
type dir struct {
	lookup func(string) (string, bool)
}

func (d dir) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	if _, err := startCtx(ctx, "dir", ref); err != nil {
		return "", err
	}
	name := ref.Name()
	if !validEntryName(name) {
		if name == "" {
			return "", annotate(ErrMissingLocator, "dir", ref)
		}
		return "", annotate(ErrInvalidName, "dir", ref)
	}
	base := ref.Path()
	if base == "" {
		dirPath, ok := lookupEnv(d.lookup)(credentialsDir)
		if !ok || dirPath == "" {
			return "", annotate(ErrMissingLocator, "dir", ref)
		}
		base = dirPath
	}
	path := filepath.Join(base, name)
	val, err := readSecretFile(path, true)
	if err != nil {
		return "", notFound(err, "dir", ref)
	}
	return val, nil
}

// validEntryName reports whether name is a single directory entry. A
// separator or a parent reference would leave the credentials directory.
func validEntryName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return false
	}
	if filepath.IsAbs(name) {
		return false
	}
	return filepath.Base(name) == name
}
