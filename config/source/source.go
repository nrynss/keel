// Package source implements the built-in secret sources an application
// registers on a config.Registry.
//
// The five names are env, env_file, file, dir, and command. An application
// source registers the same way and resolves through the same Lookup path.
// config does not import this package, so the two cannot cycle.
//
// # Env file dialect
//
// An env_file value is the literal text after the first equals. One trailing
// carriage return is trimmed. A value wrapped in matching quotes is refused.
// The error names the key and says to remove them.
//
// Docker --env-file keeps quotes as part of the value. A shell that sources
// the same file strips matching quotes, and systemd strips them too. Those
// two readers disagree, so neither quoting behavior is safe. Refusing is
// the only choice that cannot hand a provider a quoted key or a key with
// its first and last character removed.
//
// A newline inside an env_file value is refused. That format cannot carry
// one, and a reader that accepts it truncates silently.
//
// # Files
//
// A file source trims exactly one trailing newline. Editors add one, and no
// secret ends in it. A secrets file readable by group or other stops the
// load and names the path and its mode.
//
// # Command
//
// A command source never passes a value as an argument. It reads standard
// output, trims one trailing newline, and fails on a non-zero exit with the
// command's standard error, truncated.
//
// # Injection
//
// Sources that read the environment take an injected lookup. Nil means
// os.LookupEnv. The process environment is never read as a hidden global.
//
// # Errors
//
// Every failure names the source and the locator. It never names the value,
// its length, or any part of it. A Source never returns a value alongside
// an error.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nrynss/keel/config"
)

// ErrMissingLocator reports a reference that lacks a field the source needs.
var ErrMissingLocator = errors.New("source: missing locator")

// ErrNotFound reports a variable, key, or file the reference names that
// does not exist.
var ErrNotFound = errors.New("source: not found")

// ErrQuotedValue reports an env file value wrapped in matching quotes.
var ErrQuotedValue = errors.New("source: quoted env file value")

// ErrNewline reports an env file value that contains a newline.
var ErrNewline = errors.New("source: env file value contains a newline")

// ErrLooseFile reports a secrets file readable by group or other.
var ErrLooseFile = errors.New("source: file is group or other readable")

// ErrCommand reports a command source that exited non-zero.
var ErrCommand = errors.New("source: command failed")

// ErrInvalidName reports a directory entry name that is not a single path
// element.
var ErrInvalidName = errors.New("source: invalid entry name")

// credentialsDir is the variable systemd sets to the LoadCredential
// directory.
const credentialsDir = "CREDENTIALS_DIRECTORY"

// looseBits is group-or-other read. A secrets file with either bit set is
// refused.
const looseBits = os.FileMode(0o044)

// Config configures RegisterDefaults. The zero value is usable.
type Config struct {
	// LookupEnv looks up an environment variable. Nil means os.LookupEnv.
	LookupEnv func(name string) (string, bool)
}

// RegisterDefaults registers the five built-in sources on reg. cfg.LookupEnv
// is the lookup that env and dir receive. Nil means os.LookupEnv.
func RegisterDefaults(reg *config.Registry, cfg Config) error {
	if reg == nil {
		return config.ErrNilRegistry
	}
	lookup := cfg.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if err := reg.Register("env", env{lookup: lookup}); err != nil {
		return err
	}
	if err := reg.Register("env_file", envFile{}); err != nil {
		return err
	}
	if err := reg.Register("file", file{}); err != nil {
		return err
	}
	if err := reg.Register("dir", dir{lookup: lookup}); err != nil {
		return err
	}
	if err := reg.Register("command", command{}); err != nil {
		return err
	}
	return nil
}

// lookupEnv returns f, or os.LookupEnv when f is nil.
func lookupEnv(f func(string) (string, bool)) func(string) (string, bool) {
	if f != nil {
		return f
	}
	return os.LookupEnv
}

// startCtx returns ctx, or background when ctx is nil, and the context
// error when it is already done.
func startCtx(ctx context.Context, kind string, ref config.Ref) (context.Context, error) {
	if ctx == nil {
		return context.Background(), nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, annotate(err, kind, ref)
	}
	return ctx, nil
}

// formatLocator returns the locator fields as space separated key=value
// pairs. It never carries a secret value.
func formatLocator(ref config.Ref) string {
	var parts []string
	if p := ref.Path(); p != "" {
		parts = append(parts, "path="+p)
	}
	if n := ref.Name(); n != "" {
		parts = append(parts, "name="+n)
	}
	if v := ref.Var(); v != "" {
		parts = append(parts, "var="+v)
	}
	if c := ref.Command(); c != "" {
		parts = append(parts, "command="+c)
	}
	if args := ref.Args(); len(args) > 0 {
		parts = append(parts, "args="+strings.Join(args, " "))
	}
	return strings.Join(parts, " ")
}

// annotate wraps err with the source name and locator. It never includes
// a secret value.
func annotate(err error, kind string, ref config.Ref) error {
	if err == nil {
		return nil
	}
	loc := formatLocator(ref)
	if loc == "" {
		return fmt.Errorf("%w: %s", err, kind)
	}
	return fmt.Errorf("%w: %s: %s", err, kind, loc)
}

// checkFileMode refuses a file readable by group or other, naming the
// path and the mode.
func checkFileMode(path string, mode os.FileMode) error {
	if mode.Perm()&looseBits != 0 {
		return fmt.Errorf("%w: path %s (mode %04o)", ErrLooseFile, path, mode.Perm())
	}
	return nil
}

// readSecretFile opens path, refuses a loose mode, and returns the
// contents. trim drops exactly one trailing newline.
func readSecretFile(path string, trim bool) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close() // the stat failed, close cannot recover it
		return "", err
	}
	if err := checkFileMode(path, info.Mode()); err != nil {
		_ = f.Close() // the file is refused, close cannot change that
		return "", err
	}
	if info.IsDir() {
		_ = f.Close() // a directory is not a secret file
		return "", errors.New("is a directory")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close() // the read failed, close cannot recover it
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	val := string(data)
	if trim {
		val = trimOneNewline(val)
	}
	return val, nil
}

// trimOneNewline drops exactly one trailing newline. A CRLF pair is one
// newline, so a Windows editor does not leave a carriage return on the
// secret.
func trimOneNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		s = strings.TrimSuffix(s, "\n")
		s = strings.TrimSuffix(s, "\r")
	}
	return s
}

// notFound maps a missing file onto ErrNotFound and annotates any other
// read error.
func notFound(err error, kind string, ref config.Ref) error {
	if errors.Is(err, os.ErrNotExist) {
		return annotate(ErrNotFound, kind, ref)
	}
	return annotate(err, kind, ref)
}
