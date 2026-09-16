package source_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nrynss/keel/config"
	"github.com/nrynss/keel/config/source"
)

const fixtureDir = "../../testdata/config"

const (
	plainValue    = "fixture-plain-ok"
	quotedInner   = "fixture-quoted-inner"
	newlineFirst  = "fixture-newline-first"
	newlineSecond = "fixture-newline-second"
	fileValue     = "fixture-file-ok"
	fileLineOne   = "fixture-line-one"
	fileLineTwo   = "fixture-line-two"
	dirValue      = "fixture-dir-token"
	cmdValue      = "fixture-cmd-ok"
	cmdSecret     = "fixture-cmd-secret"
	cmdFailMsg    = "command-failed-message"
)

func fixturePath(name string) string {
	return filepath.Join(fixtureDir, name)
}

func copyMode(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(name))
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return dst
}

func copyDirEntry(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	data, err := os.ReadFile(fixturePath(filepath.Join("dir", name)))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return dir
}

func copyCmd(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, data, 0o700); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Chmod(dst, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return dst
}

func registry(t *testing.T, lookup func(string) (string, bool)) *config.Registry {
	t.Helper()
	reg := config.NewRegistry()
	if err := source.RegisterDefaults(reg, source.Config{LookupEnv: lookup}); err != nil {
		t.Fatalf("RegisterDefaults: %v", err)
	}
	return reg
}

func resolve(t *testing.T, name string, ref config.Ref, lookup func(string) (string, bool)) (string, error) {
	t.Helper()
	src, ok := registry(t, lookup).Lookup(name)
	if !ok {
		t.Fatalf("source %q missing", name)
	}
	return src.Resolve(t.Context(), ref)
}

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func assertValue(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("resolved value does not match")
	}
}

func assertQuiet(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			t.Errorf("error names a secret")
			return
		}
	}
}

func TestRegisterDefaultsRegistersFiveSources(t *testing.T) {
	reg := registry(t, nil)
	for _, name := range []string{"env", "env_file", "file", "dir", "command"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("source %q missing", name)
		}
	}
}

func TestRegisterDefaultsNilRegistry(t *testing.T) {
	err := source.RegisterDefaults(nil, source.Config{})
	if !errors.Is(err, config.ErrNilRegistry) {
		t.Errorf("err = %v, want ErrNilRegistry", err)
	}
}

func TestRegisterDefaultsDuplicate(t *testing.T) {
	reg := registry(t, nil)
	err := source.RegisterDefaults(reg, source.Config{})
	if !errors.Is(err, config.ErrDuplicateSource) {
		t.Errorf("err = %v, want ErrDuplicateSource", err)
	}
}

func TestNewRefCopiesArgsAndDefaultsRead(t *testing.T) {
	args := []string{"read", "op://vault/item/field"}
	ref := config.NewRef(config.RefConfig{
		Source:  "command",
		Command: "op",
		Args:    args,
	})
	args[0] = "mutated"
	got := ref.Args()
	if len(got) != 2 || got[0] != "read" || got[1] != "op://vault/item/field" {
		t.Errorf("NewRef did not copy args")
	}
	if ref.Read() != config.ReadAtBoot {
		t.Errorf("empty Read did not default to at_boot")
	}
	if ref.Source() != "command" || ref.Command() != "op" {
		t.Errorf("NewRef dropped locator fields")
	}
}

func TestEnvResolvesFromLookup(t *testing.T) {
	ref := config.NewRef(config.RefConfig{Source: "env", Var: "PROVIDER_API_KEY"})
	got, err := resolve(t, "env", ref, mapLookup(map[string]string{
		"PROVIDER_API_KEY": plainValue,
	}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, plainValue)
}

func TestEnvDefaultLookupUsesProcess(t *testing.T) {
	t.Setenv("KEEL_SOURCE_TEST_VAR", plainValue)
	ref := config.NewRef(config.RefConfig{Source: "env", Var: "KEEL_SOURCE_TEST_VAR"})
	got, err := resolve(t, "env", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, plainValue)
}

func TestEnvMissingFailsByVar(t *testing.T) {
	ref := config.NewRef(config.RefConfig{Source: "env", Var: "MISSING_VAR"})
	_, err := resolve(t, "env", ref, mapLookup(nil))
	if !errors.Is(err, source.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	assertQuiet(t, err)
	if !strings.Contains(err.Error(), "MISSING_VAR") {
		t.Errorf("error does not name the var")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Errorf("error does not name the source")
	}
}

func TestEnvMissingLocator(t *testing.T) {
	ref := config.NewRef(config.RefConfig{Source: "env"})
	_, err := resolve(t, "env", ref, mapLookup(nil))
	if !errors.Is(err, source.ErrMissingLocator) {
		t.Errorf("err = %v, want ErrMissingLocator", err)
	}
}

func TestEnvFileResolvesPlain(t *testing.T) {
	path := copyMode(t, "env_file.plain", 0o600)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "PROVIDER_API_KEY",
	})
	got, err := resolve(t, "env_file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, plainValue)
}

func TestEnvFileMatchingQuotesRefuseByKey(t *testing.T) {
	path := copyMode(t, "env_file.quoted", 0o600)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "QUOTED_KEY",
	})
	_, err := resolve(t, "env_file", ref, nil)
	if !errors.Is(err, source.ErrQuotedValue) {
		t.Errorf("err = %v, want ErrQuotedValue", err)
	}
	assertQuiet(t, err, quotedInner)
	if !strings.Contains(err.Error(), "QUOTED_KEY") {
		t.Errorf("error does not name the key")
	}
	if !strings.Contains(err.Error(), "remove the quotes") {
		t.Errorf("error does not say to remove the quotes")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the path")
	}
}

func TestEnvFileQuotedNeighborDoesNotBlockPlain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "PLAIN_KEY=" + plainValue + "\nQUOTED_KEY=\"" + quotedInner + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	plain := config.NewRef(config.RefConfig{Source: "env_file", Path: path, Var: "PLAIN_KEY"})
	got, err := resolve(t, "env_file", plain, nil)
	if err != nil {
		t.Fatalf("plain Resolve: %v", err)
	}
	assertValue(t, got, plainValue)
	quoted := config.NewRef(config.RefConfig{Source: "env_file", Path: path, Var: "QUOTED_KEY"})
	_, err = resolve(t, "env_file", quoted, nil)
	if !errors.Is(err, source.ErrQuotedValue) {
		t.Errorf("quoted err = %v, want ErrQuotedValue", err)
	}
	assertQuiet(t, err, quotedInner, plainValue)
}

func TestEnvFileSameBytesWithoutQuotesResolve(t *testing.T) {
	path := copyMode(t, "env_file.plain", 0o600)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "QUOTED_KEY",
	})
	got, err := resolve(t, "env_file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, quotedInner)
}

func TestEnvFileNewlineRefused(t *testing.T) {
	path := copyMode(t, "env_file.newline", 0o600)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "PROVIDER_API_KEY",
	})
	_, err := resolve(t, "env_file", ref, nil)
	if !errors.Is(err, source.ErrNewline) {
		t.Errorf("err = %v, want ErrNewline", err)
	}
	assertQuiet(t, err, newlineFirst, newlineSecond)
}

func TestEnvFileSingleQuotesRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "QUOTED_KEY='fixture-quoted-inner'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "env_file", Path: path, Var: "QUOTED_KEY"})
	_, err := resolve(t, "env_file", ref, nil)
	if !errors.Is(err, source.ErrQuotedValue) {
		t.Errorf("err = %v, want ErrQuotedValue", err)
	}
	assertQuiet(t, err, quotedInner)
}

func TestEnvFileTrimsTrailingCR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "PROVIDER_API_KEY=" + plainValue + "\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "env_file", Path: path, Var: "PROVIDER_API_KEY"})
	got, err := resolve(t, "env_file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, plainValue)
}

func TestEnvFileCRLFBlankLineResolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	const other = "fixture-other-ok"
	body := "PROVIDER_API_KEY=" + plainValue + "\r\n\r\nOTHER=" + other + "\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	first := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "PROVIDER_API_KEY",
	})
	got, err := resolve(t, "env_file", first, nil)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	assertValue(t, got, plainValue)
	second := config.NewRef(config.RefConfig{Source: "env_file", Path: path, Var: "OTHER"})
	got, err = resolve(t, "env_file", second, nil)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	assertValue(t, got, other)
}

func TestEnvFileMissingKeyFails(t *testing.T) {
	path := copyMode(t, "env_file.plain", 0o600)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "MISSING_KEY",
	})
	_, err := resolve(t, "env_file", ref, nil)
	if !errors.Is(err, source.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	assertQuiet(t, err, plainValue, quotedInner)
}

func TestEnvFileLooseFileRefused(t *testing.T) {
	path := copyMode(t, "env_file.plain", 0o644)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "PROVIDER_API_KEY",
	})
	_, err := resolve(t, "env_file", ref, nil)
	if !errors.Is(err, source.ErrLooseFile) {
		t.Errorf("err = %v, want ErrLooseFile", err)
	}
	assertQuiet(t, err, plainValue)
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the path")
	}
	if !strings.Contains(err.Error(), "0644") {
		t.Errorf("error does not name the mode")
	}
}

func TestFileTrimsOneTrailingNewline(t *testing.T) {
	path := copyMode(t, "file.trailing-newline", 0o600)
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	got, err := resolve(t, "file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, fileValue)
}

func TestFileTrimsExactlyOneTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(fileValue+"\n\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	got, err := resolve(t, "file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, fileValue+"\n")
}

func TestFileKeepsInternalNewline(t *testing.T) {
	path := copyMode(t, "file.internal-newline", 0o600)
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	got, err := resolve(t, "file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, fileLineOne+"\n"+fileLineTwo)
}

func TestFile0600Resolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(fileValue+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	got, err := resolve(t, "file", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, fileValue)
}

func TestFile0644RefusedByPathAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(fileValue+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	_, err := resolve(t, "file", ref, nil)
	if !errors.Is(err, source.ErrLooseFile) {
		t.Errorf("err = %v, want ErrLooseFile", err)
	}
	assertQuiet(t, err, fileValue)
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the path")
	}
	if !strings.Contains(err.Error(), "0644") {
		t.Errorf("error does not name the mode")
	}
	if !strings.Contains(err.Error(), "file") {
		t.Errorf("error does not name the source")
	}
}

func TestFileGroupReadableRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(fileValue), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	_, err := resolve(t, "file", ref, nil)
	if !errors.Is(err, source.ErrLooseFile) {
		t.Errorf("err = %v, want ErrLooseFile", err)
	}
	if !strings.Contains(err.Error(), "0640") {
		t.Errorf("error does not name the mode")
	}
	assertQuiet(t, err, fileValue)
}

func TestFileMissingFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	_, err := resolve(t, "file", ref, nil)
	if !errors.Is(err, source.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the path")
	}
}

func TestDirResolvesNamedEntry(t *testing.T) {
	dir := copyDirEntry(t, "token", 0o600)
	ref := config.NewRef(config.RefConfig{Source: "dir", Path: dir, Name: "token"})
	got, err := resolve(t, "dir", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, dirValue)
}

func TestDirUsesCredentialsDirectory(t *testing.T) {
	dir := copyDirEntry(t, "token", 0o600)
	ref := config.NewRef(config.RefConfig{Source: "dir", Name: "token"})
	got, err := resolve(t, "dir", ref, mapLookup(map[string]string{
		"CREDENTIALS_DIRECTORY": dir,
	}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, dirValue)
}

func TestDirExplicitPathSkipsLookup(t *testing.T) {
	dir := copyDirEntry(t, "token", 0o600)
	lookup := func(string) (string, bool) {
		t.Error("lookup called with an explicit path")
		return "", false
	}
	ref := config.NewRef(config.RefConfig{Source: "dir", Path: dir, Name: "token"})
	got, err := resolve(t, "dir", ref, lookup)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, dirValue)
}

func TestDirDefaultLookupUsesProcess(t *testing.T) {
	dir := copyDirEntry(t, "token", 0o600)
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	ref := config.NewRef(config.RefConfig{Source: "dir", Name: "token"})
	got, err := resolve(t, "dir", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, dirValue)
}

func TestDirLooseFileRefused(t *testing.T) {
	dir := copyDirEntry(t, "token", 0o644)
	ref := config.NewRef(config.RefConfig{Source: "dir", Path: dir, Name: "token"})
	_, err := resolve(t, "dir", ref, nil)
	if !errors.Is(err, source.ErrLooseFile) {
		t.Errorf("err = %v, want ErrLooseFile", err)
	}
	assertQuiet(t, err, dirValue)
	joined := filepath.Join(dir, "token")
	if !strings.Contains(err.Error(), joined) {
		t.Errorf("error does not name the path")
	}
	if !strings.Contains(err.Error(), "0644") {
		t.Errorf("error does not name the mode")
	}
}

func TestDirRejectsParentName(t *testing.T) {
	dir := copyDirEntry(t, "token", 0o600)
	ref := config.NewRef(config.RefConfig{Source: "dir", Path: dir, Name: "../token"})
	_, err := resolve(t, "dir", ref, nil)
	if !errors.Is(err, source.ErrInvalidName) {
		t.Errorf("err = %v, want ErrInvalidName", err)
	}
	assertQuiet(t, err, dirValue)
}

func TestDirMissingCredentialsDirectory(t *testing.T) {
	ref := config.NewRef(config.RefConfig{Source: "dir", Name: "token"})
	_, err := resolve(t, "dir", ref, mapLookup(nil))
	if !errors.Is(err, source.ErrMissingLocator) {
		t.Errorf("err = %v, want ErrMissingLocator", err)
	}
}

func TestCommandResolvesStdout(t *testing.T) {
	bin := copyCmd(t, "command-ok.sh")
	ref := config.NewRef(config.RefConfig{Source: "command", Command: bin})
	got, err := resolve(t, "command", ref, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, cmdValue)
}

func TestCommandExitOneFailsWithMessage(t *testing.T) {
	bin := copyCmd(t, "command-fail.sh")
	ref := config.NewRef(config.RefConfig{Source: "command", Command: bin})
	_, err := resolve(t, "command", ref, nil)
	if !errors.Is(err, source.ErrCommand) {
		t.Errorf("err = %v, want ErrCommand", err)
	}
	assertQuiet(t, err, cmdSecret)
	if !strings.Contains(err.Error(), cmdFailMsg) {
		t.Errorf("error does not carry the command message")
	}
	if !strings.Contains(err.Error(), bin) {
		t.Errorf("error does not name the command")
	}
}

func TestCommandTruncatesStderr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "long.sh")
	script := "#!/bin/sh\ndd if=/dev/zero bs=2048 count=1 2>/dev/null | tr '\\0' 'e' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	ref := config.NewRef(config.RefConfig{Source: "command", Command: bin})
	_, err := resolve(t, "command", ref, nil)
	if !errors.Is(err, source.ErrCommand) {
		t.Errorf("err = %v, want ErrCommand", err)
	}
	msg := err.Error()
	if strings.Count(msg, "e") >= 2048 {
		t.Errorf("error keeps the full stderr")
	}
	if !strings.Contains(msg, "truncated") {
		t.Errorf("error does not say it truncated")
	}
}

func TestCommandMissingLocator(t *testing.T) {
	ref := config.NewRef(config.RefConfig{Source: "command"})
	_, err := resolve(t, "command", ref, nil)
	if !errors.Is(err, source.ErrMissingLocator) {
		t.Errorf("err = %v, want ErrMissingLocator", err)
	}
}

func TestCancelledContextFailsBeforeRead(t *testing.T) {
	path := copyMode(t, "file.trailing-newline", 0o600)
	ref := config.NewRef(config.RefConfig{Source: "file", Path: path})
	src, ok := registry(t, nil).Lookup("file")
	if !ok {
		t.Fatal("file source missing")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := src.Resolve(ctx, ref)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	assertQuiet(t, err, fileValue)
}

func TestApplicationSourceResolvesThroughRegistry(t *testing.T) {
	reg := registry(t, nil)
	const appName = "memory"
	const appValue = "fixture-app-ok"
	err := reg.Register(appName, staticSource{value: appValue})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	src, ok := reg.Lookup(appName)
	if !ok {
		t.Fatal("application source missing")
	}
	ref := config.NewRef(config.RefConfig{Source: appName, Name: "token"})
	got, err := src.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertValue(t, got, appValue)
}

func TestResolveNeverReturnsValueWithError(t *testing.T) {
	path := copyMode(t, "env_file.quoted", 0o600)
	ref := config.NewRef(config.RefConfig{
		Source: "env_file",
		Path:   path,
		Var:    "QUOTED_KEY",
	})
	got, err := resolve(t, "env_file", ref, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got != "" {
		t.Errorf("error path returned a value")
	}
	assertQuiet(t, err, quotedInner)
}

// staticSource is an application Source that returns a fixed value.
type staticSource struct {
	value string
}

func (s staticSource) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.value, nil
}
