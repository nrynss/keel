package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nrynss/keel/config"
	"github.com/nrynss/keel/config/source"
)

// Write two strict secret files for the live run.
func TestShippedLiveFileFollowsDisk(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "live.key")
	loosePath := filepath.Join(dir, "loose.key")
	liveFirst := "live-one"
	liveSecond := "live-two"
	looseFirst := "loose-one"
	if err := os.WriteFile(livePath, []byte(liveFirst), 0o600); err != nil {
		t.Fatalf("write live file: %v", err)
	}
	if err := os.Chmod(livePath, 0o600); err != nil {
		t.Fatalf("chmod live file: %v", err)
	}
	if err := os.WriteFile(loosePath, []byte(looseFirst), 0o600); err != nil {
		t.Fatalf("write loose file: %v", err)
	}
	if err := os.Chmod(loosePath, 0o600); err != nil {
		t.Fatalf("chmod loose file: %v", err)
	}
	// Load both secrets through the shipped file source.
	doc := "[live]\nsource = \"file\"\npath = " + strconv.Quote(livePath) + "\nread = \"at_use\"\n" +
		"[loose]\nsource = \"file\"\npath = " + strconv.Quote(loosePath) + "\nread = \"at_use\"\n"
	settingsPath := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(settingsPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	reg := config.NewRegistry()
	if err := source.RegisterDefaults(reg, source.Config{LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		t.Fatalf("RegisterDefaults: %v", err)
	}
	var got struct {
		Live  config.Secret `toml:"live"`
		Loose config.Secret `toml:"loose"`
	}
	if _, err := config.Load(t.Context(), &got, config.Config{
		Path:      settingsPath,
		LookupEnv: func(string) (string, bool) { return "", false },
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if val, err := got.Live.Reveal(); err != nil {
		t.Fatalf("Reveal: %v", err)
	} else if val != liveFirst {
		t.Fatalf("Reveal holds the wrong value")
	}
	if val, err := got.Loose.Reveal(); err != nil {
		t.Fatalf("loose Reveal: %v", err)
	} else if val != looseFirst {
		t.Fatalf("loose Reveal holds the wrong value")
	}
	// Rewrite the first file and check the new value.
	if err := os.WriteFile(livePath, []byte(liveSecond), 0o600); err != nil {
		t.Fatalf("rewrite live file: %v", err)
	}
	if err := os.Chmod(livePath, 0o600); err != nil {
		t.Fatalf("chmod live file: %v", err)
	}
	if val, err := got.Live.Reveal(); err != nil {
		t.Fatalf("Reveal after rewrite: %v", err)
	} else if val != liveSecond {
		t.Fatalf("Reveal missed the rewrite")
	}
	// Delete the first file and require a clean failure.
	if err := os.Remove(livePath); err != nil {
		t.Fatalf("remove live file: %v", err)
	}
	val, err := got.Live.Reveal()
	if err == nil {
		t.Fatalf("Reveal after delete succeeded")
	}
	if val != "" {
		t.Fatalf("Reveal served a stale value")
	}
	if !errors.Is(err, config.ErrResolve) {
		t.Fatalf("delete error misses the resolve marker")
	}
	msg := err.Error()
	if !strings.Contains(msg, livePath) {
		t.Fatalf("delete error misses the file path")
	}
	if strings.Contains(msg, liveFirst) || strings.Contains(msg, liveSecond) {
		t.Fatalf("delete error carries a secret value")
	}
	if _, err := got.Live.Reveal(); err == nil {
		t.Fatalf("second Reveal after delete succeeded")
	}
	// Loosen the second file and require a failure naming the path.
	if err := os.Chmod(loosePath, 0o640); err != nil {
		t.Fatalf("chmod loose file: %v", err)
	}
	val, err = got.Loose.Reveal()
	if err == nil {
		t.Fatalf("loose Reveal succeeded")
	}
	if val != "" {
		t.Fatalf("loose Reveal served a stale value")
	}
	if !errors.Is(err, config.ErrResolve) {
		t.Fatalf("loose error misses the resolve marker")
	}
	msg = err.Error()
	if !strings.Contains(msg, loosePath) {
		t.Fatalf("loose error misses the file path")
	}
	if strings.Contains(msg, looseFirst) {
		t.Fatalf("loose error carries a secret value")
	}
}

// Write two strict files backing the list.
func TestShippedListTracksEachElement(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.key")
	bPath := filepath.Join(dir, "b.key")
	aFirst := "list-a-one"
	aSecond := "list-a-two"
	bFirst := "list-b-one"
	if err := os.WriteFile(aPath, []byte(aFirst), 0o600); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	if err := os.Chmod(aPath, 0o600); err != nil {
		t.Fatalf("chmod first file: %v", err)
	}
	if err := os.WriteFile(bPath, []byte(bFirst), 0o600); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	if err := os.Chmod(bPath, 0o600); err != nil {
		t.Fatalf("chmod second file: %v", err)
	}
	// Load the two element list through shipped sources.
	doc := "cred = [{source = \"file\", path = " + strconv.Quote(aPath) + ", read = \"at_use\"}, {source = \"file\", path = " + strconv.Quote(bPath) + ", read = \"at_use\"}]\n"
	settingsPath := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(settingsPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	reg := config.NewRegistry()
	if err := source.RegisterDefaults(reg, source.Config{LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		t.Fatalf("RegisterDefaults: %v", err)
	}
	var got struct {
		Cred []config.Secret `toml:"cred"`
	}
	if _, err := config.Load(t.Context(), &got, config.Config{
		Path:      settingsPath,
		LookupEnv: func(string) (string, bool) { return "", false },
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Cred) != 2 {
		t.Fatalf("list holds the wrong count")
	}
	if val, err := got.Cred[0].Reveal(); err != nil {
		t.Fatalf("first Reveal: %v", err)
	} else if val != aFirst {
		t.Fatalf("first Reveal holds the wrong value")
	}
	if val, err := got.Cred[1].Reveal(); err != nil {
		t.Fatalf("second Reveal: %v", err)
	} else if val != bFirst {
		t.Fatalf("second Reveal holds the wrong value")
	}
	// Rewrite one file and check only that element moves.
	if err := os.WriteFile(aPath, []byte(aSecond), 0o600); err != nil {
		t.Fatalf("rewrite first file: %v", err)
	}
	if err := os.Chmod(aPath, 0o600); err != nil {
		t.Fatalf("chmod first file: %v", err)
	}
	if val, err := got.Cred[0].Reveal(); err != nil {
		t.Fatalf("first Reveal after rewrite: %v", err)
	} else if val != aSecond {
		t.Fatalf("first Reveal missed the rewrite")
	}
	if val, err := got.Cred[1].Reveal(); err != nil {
		t.Fatalf("second Reveal after rewrite: %v", err)
	} else if val != bFirst {
		t.Fatalf("second Reveal moved without a rewrite")
	}
	// Delete one file and check only that element fails.
	if err := os.Remove(aPath); err != nil {
		t.Fatalf("remove first file: %v", err)
	}
	val, err := got.Cred[0].Reveal()
	if err == nil {
		t.Fatalf("first Reveal after delete succeeded")
	}
	if val != "" {
		t.Fatalf("first Reveal served a stale value")
	}
	if !errors.Is(err, config.ErrResolve) {
		t.Fatalf("first error misses the resolve marker")
	}
	if msg := err.Error(); strings.Contains(msg, aFirst) || strings.Contains(msg, aSecond) {
		t.Fatalf("first error carries a secret value")
	}
	if val, err := got.Cred[1].Reveal(); err != nil {
		t.Fatalf("second Reveal after delete: %v", err)
	} else if val != bFirst {
		t.Fatalf("second Reveal lost its value")
	}
}

// Write two strict files backing the map.
func TestShippedMapTracksEachEntry(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.key")
	bPath := filepath.Join(dir, "b.key")
	aFirst := "map-a-one"
	aSecond := "map-a-two"
	bFirst := "map-b-one"
	if err := os.WriteFile(aPath, []byte(aFirst), 0o600); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	if err := os.Chmod(aPath, 0o600); err != nil {
		t.Fatalf("chmod first file: %v", err)
	}
	if err := os.WriteFile(bPath, []byte(bFirst), 0o600); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	if err := os.Chmod(bPath, 0o600); err != nil {
		t.Fatalf("chmod second file: %v", err)
	}
	// Load the two entry map through shipped sources.
	doc := "[keys.a]\nsource = \"file\"\npath = " + strconv.Quote(aPath) + "\nread = \"at_use\"\n" +
		"[keys.b]\nsource = \"file\"\npath = " + strconv.Quote(bPath) + "\nread = \"at_use\"\n"
	settingsPath := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(settingsPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	reg := config.NewRegistry()
	if err := source.RegisterDefaults(reg, source.Config{LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		t.Fatalf("RegisterDefaults: %v", err)
	}
	var got struct {
		Keys map[string]config.Secret `toml:"keys"`
	}
	if _, err := config.Load(t.Context(), &got, config.Config{
		Path:      settingsPath,
		LookupEnv: func(string) (string, bool) { return "", false },
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("map holds the wrong count")
	}
	if val, err := got.Keys["a"].Reveal(); err != nil {
		t.Fatalf("first Reveal: %v", err)
	} else if val != aFirst {
		t.Fatalf("first Reveal holds the wrong value")
	}
	if val, err := got.Keys["b"].Reveal(); err != nil {
		t.Fatalf("second Reveal: %v", err)
	} else if val != bFirst {
		t.Fatalf("second Reveal holds the wrong value")
	}
	// Rewrite one file and check only that entry moves.
	if err := os.WriteFile(aPath, []byte(aSecond), 0o600); err != nil {
		t.Fatalf("rewrite first file: %v", err)
	}
	if err := os.Chmod(aPath, 0o600); err != nil {
		t.Fatalf("chmod first file: %v", err)
	}
	if val, err := got.Keys["a"].Reveal(); err != nil {
		t.Fatalf("first Reveal after rewrite: %v", err)
	} else if val != aSecond {
		t.Fatalf("first Reveal missed the rewrite")
	}
	if val, err := got.Keys["b"].Reveal(); err != nil {
		t.Fatalf("second Reveal after rewrite: %v", err)
	} else if val != bFirst {
		t.Fatalf("second Reveal moved without a rewrite")
	}
	// Delete one file and check only that entry fails.
	if err := os.Remove(aPath); err != nil {
		t.Fatalf("remove first file: %v", err)
	}
	val, err := got.Keys["a"].Reveal()
	if err == nil {
		t.Fatalf("first Reveal after delete succeeded")
	}
	if val != "" {
		t.Fatalf("first Reveal served a stale value")
	}
	if !errors.Is(err, config.ErrResolve) {
		t.Fatalf("first error misses the resolve marker")
	}
	if msg := err.Error(); strings.Contains(msg, aFirst) || strings.Contains(msg, aSecond) {
		t.Fatalf("first error carries a secret value")
	}
	if val, err := got.Keys["b"].Reveal(); err != nil {
		t.Fatalf("second Reveal after delete: %v", err)
	} else if val != bFirst {
		t.Fatalf("second Reveal lost its value")
	}
}
