package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretFixtureValue = "secret-fixture-value"

type loadSettings struct {
	Render struct {
		Model      string `toml:"model"`
		MaxSeconds int    `toml:"max_seconds"`
		Workers    int    `toml:"workers"`
		Format     string `toml:"format"`
		Quality    string `toml:"quality"`
	} `toml:"render"`
	Secrets struct {
		Key Secret `toml:"key"`
	} `toml:"secrets"`
}

func valueRegistry(t *testing.T, value string) *Registry {
	t.Helper()
	reg := NewRegistry()
	src := stubSource{value: value}
	for _, name := range []string{"env", "env_file", "file", "dir", "command"} {
		if err := reg.Register(name, src); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return reg
}

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func fixtureLoad(t *testing.T) (loadSettings, Plan) {
	t.Helper()
	var got loadSettings
	got.Render.Model = "default-model"
	got.Render.MaxSeconds = 1
	got.Render.Workers = 2
	got.Render.Format = "mp4"
	plan, err := Load(t.Context(), &got, Config{
		Path: "testdata/app.toml",
		LookupEnv: mapLookup(map[string]string{
			"RENDER_MAX_SECONDS": "100",
			"RENDER_QUALITY":     "high",
		}),
		Flags:    map[string]string{"render.workers": "8"},
		Registry: valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return got, plan
}

func TestLoadPrecedence(t *testing.T) {
	got, plan := fixtureLoad(t)
	if got.Render.Format != "mp4" {
		t.Errorf("format = %q, want default mp4", got.Render.Format)
	}
	if got.Render.Model != "file-model" {
		t.Errorf("model = %q, want file-model", got.Render.Model)
	}
	if got.Render.MaxSeconds != 100 {
		t.Errorf("max_seconds = %d, want env 100", got.Render.MaxSeconds)
	}
	if got.Render.Workers != 8 {
		t.Errorf("workers = %d, want flag 8", got.Render.Workers)
	}
	if got.Render.Quality != "high" {
		t.Errorf("quality = %q, want env high", got.Render.Quality)
	}
	val, err := got.Secrets.Key.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != secretFixtureValue {
		t.Errorf("secret resolved to the wrong value")
	}
	assertQuiet(t, plan.String(), secretFixtureValue)
}

func TestLoadPlanGolden(t *testing.T) {
	_, plan := fixtureLoad(t)
	got := []byte(plan.String())
	want, err := os.ReadFile("testdata/plan.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("plan does not match golden\n got: %q\nwant: %q", got, want)
	}
}

func TestLoadRequiredMissing(t *testing.T) {
	var got struct {
		Model string `toml:"model" config:"required"`
	}
	_, err := Load(t.Context(), &got, Config{LookupEnv: mapLookup(nil)})
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("err = %v, want ErrRequired", err)
	}
	msg := err.Error()
	for _, part := range []string{"model", "source", "locator", "none"} {
		if !strings.Contains(msg, part) {
			t.Errorf("error missing %q: %v", part, err)
		}
	}
}

func TestLoadRequiredSecretMissing(t *testing.T) {
	var got struct {
		Key Secret `toml:"key" config:"required"`
	}
	_, err := Load(t.Context(), &got, Config{LookupEnv: mapLookup(nil)})
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("err = %v, want ErrRequired", err)
	}
	msg := err.Error()
	for _, part := range []string{"key", "source", "locator", "none"} {
		if !strings.Contains(msg, part) {
			t.Errorf("error missing %q: %v", part, err)
		}
	}
}

func TestLoadRequiredFromEnvWithoutFile(t *testing.T) {
	var got struct {
		Model string `toml:"model" config:"required"`
	}
	plan, err := Load(t.Context(), &got, Config{
		Path:      filepath.Join(t.TempDir(), "missing.toml"),
		LookupEnv: mapLookup(map[string]string{"MODEL": "from-env"}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Model != "from-env" {
		t.Errorf("model = %q, want from-env", got.Model)
	}
	if !strings.Contains(plan.String(), "model\tenv\tvar=MODEL\tresolved") {
		t.Errorf("plan = %q", plan.String())
	}
}

func TestLoadPresentMalformedIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("model = [\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Model string `toml:"model"`
	}
	_, err := Load(t.Context(), &got, Config{Path: path, LookupEnv: mapLookup(nil)})
	if err == nil {
		t.Fatal("Load accepted a malformed file")
	}
	if !errors.Is(err, ErrMalformed) && !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want malformed or a named path", err)
	}
}

func TestLoadPresentDirectoryIsFatal(t *testing.T) {
	var got struct {
		Model string `toml:"model"`
	}
	_, err := Load(t.Context(), &got, Config{Path: t.TempDir(), LookupEnv: mapLookup(nil)})
	if err == nil {
		t.Fatal("Load accepted a directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %v, want directory", err)
	}
}

func TestLoadPathVarChoosesFile(t *testing.T) {
	var got loadSettings
	plan, err := Load(t.Context(), &got, Config{
		PathVar:   "APP_SETTINGS",
		LookupEnv: mapLookup(map[string]string{"APP_SETTINGS": "testdata/app.toml"}),
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Render.Model != "file-model" {
		t.Errorf("model = %q, want file-model", got.Render.Model)
	}
	if !strings.Contains(plan.String(), "render.model\tfile\tpath=testdata/app.toml\tresolved") {
		t.Errorf("plan = %q", plan.String())
	}
}

func TestLoadSearchSkipsMissing(t *testing.T) {
	var got loadSettings
	missing := filepath.Join(t.TempDir(), "nope.toml")
	_, err := Load(t.Context(), &got, Config{
		Search:    []string{missing, "testdata/app.toml"},
		LookupEnv: mapLookup(nil),
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Render.Model != "file-model" {
		t.Errorf("model = %q, want file-model", got.Render.Model)
	}
}

func TestLoadSecretFallback(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("env", stubSource{failure: errors.New("missing")}); err != nil {
		t.Fatalf("register env: %v", err)
	}
	if err := reg.Register("file", stubSource{value: secretFixtureValue}); err != nil {
		t.Fatalf("register file: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	doc := "key = [{source=\"env\", var=\"MISSING\"}, {source=\"file\", path=\"/x\"}]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Key Secret `toml:"key"`
	}
	plan, err := Load(t.Context(), &got, Config{
		Path:      path,
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	val, err := got.Key.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != secretFixtureValue {
		t.Errorf("secret resolved to the wrong value")
	}
	line := plan.String()
	if !strings.Contains(line, "key\tfile\tpath=/x\tresolved") {
		t.Errorf("plan = %q", line)
	}
	assertQuiet(t, line, secretFixtureValue)
}

func TestLoadSecretIgnoresEnvOverride(t *testing.T) {
	var got loadSettings
	_, err := Load(t.Context(), &got, Config{
		Path: "testdata/app.toml",
		LookupEnv: mapLookup(map[string]string{
			"SECRETS_KEY": secretFixtureValue + "-inline",
		}),
		Registry: valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	val, err := got.Secrets.Key.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != secretFixtureValue {
		t.Errorf("secret resolved to the wrong value")
	}
}

func TestLoadSecretResolveFailsByKey(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("file", stubSource{failure: errors.New("missing")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var got loadSettings
	_, err := Load(t.Context(), &got, Config{
		Path:      "testdata/app.toml",
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	})
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("err = %v, want ErrResolve", err)
	}
	msg := err.Error()
	for _, part := range []string{"secrets.key", "source", "file", "locator", "path=/run/secrets/provider_key"} {
		if !strings.Contains(msg, part) {
			t.Errorf("error missing %q: %v", part, err)
		}
	}
	assertQuiet(t, msg, secretFixtureValue)
}

func TestLoadDumpReplacesSecret(t *testing.T) {
	got, plan := fixtureLoad(t)
	raw, err := Dump(&got, plan)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, secretFixtureValue) {
		t.Errorf("dump carries the value")
	}
	if !strings.Contains(text, "path=/run/secrets/provider_key") || !strings.Contains(text, "secrets.key") {
		t.Errorf("dump missing plan line: %s", text)
	}
	if !strings.Contains(text, "file-model") {
		t.Errorf("dump missing file setting: %s", text)
	}
	if !strings.Contains(text, "100") {
		t.Errorf("dump missing env setting: %s", text)
	}
	printed := fmt.Sprintf("%v %#v %s", got, got, plan.String())
	assertQuiet(t, printed, secretFixtureValue)
	assertQuiet(t, text, secretFixtureValue)
}

func TestLoadUnknownFlag(t *testing.T) {
	var got struct {
		Model string `toml:"model"`
	}
	_, err := Load(t.Context(), &got, Config{
		LookupEnv: mapLookup(nil),
		Flags:     map[string]string{"typo": "1"},
	})
	if err == nil {
		t.Fatal("Load accepted an unknown flag")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("err = %v, want typo", err)
	}
}

func TestLoadSecretFlagRefused(t *testing.T) {
	var got struct {
		Key Secret `toml:"key"`
	}
	_, err := Load(t.Context(), &got, Config{
		LookupEnv: mapLookup(nil),
		Flags:     map[string]string{"key": secretFixtureValue},
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err == nil {
		t.Fatal("Load accepted a secret flag")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v, want secret", err)
	}
	assertQuiet(t, err.Error(), secretFixtureValue)
}

func TestLoadBadDestination(t *testing.T) {
	if _, err := Load(t.Context(), loadSettings{}, Config{}); !errors.Is(err, ErrInvalidDest) {
		t.Errorf("value dest err = %v, want ErrInvalidDest", err)
	}
	if _, err := Load(t.Context(), nil, Config{}); !errors.Is(err, ErrInvalidDest) {
		t.Errorf("nil dest err = %v, want ErrInvalidDest", err)
	}
}

func TestLoadOverrideKeyHyphen(t *testing.T) {
	var got struct {
		Max int `toml:"max-seconds"`
	}
	_, err := Load(t.Context(), &got, Config{
		LookupEnv: mapLookup(map[string]string{"MAX_SECONDS": "9"}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Max != 9 {
		t.Errorf("max-seconds = %d, want 9", got.Max)
	}
}

func TestLoadFlagOverrideKey(t *testing.T) {
	var got struct {
		Workers int `toml:"workers"`
	}
	_, err := Load(t.Context(), &got, Config{
		LookupEnv: mapLookup(nil),
		Flags:     map[string]string{"WORKERS": "3"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Workers != 3 {
		t.Errorf("workers = %d, want 3", got.Workers)
	}
}

func TestLoadMapSecretResolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	doc := `
[keys.a]
source = "file"
path = "/run/secrets/a"
[keys.b]
source = "file"
path = "/run/secrets/b"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Keys map[string]Secret `toml:"keys"`
	}
	plan, err := Load(t.Context(), &got, Config{
		Path:      path,
		LookupEnv: mapLookup(nil),
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(got.Keys))
	}
	for _, k := range []string{"a", "b"} {
		val, err := got.Keys[k].Reveal()
		if err != nil {
			t.Fatalf("Reveal %s: %v", k, err)
		}
		if val != secretFixtureValue {
			t.Errorf("secret %s resolved to the wrong value", k)
		}
	}
	assertQuiet(t, plan.String(), secretFixtureValue)
}

func TestLoadMapStructEnvOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte("[providers.openai]\nmodel = \"file-model\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	type provider struct {
		Model string `toml:"model"`
	}
	var got struct {
		Providers map[string]provider `toml:"providers"`
	}
	_, err := Load(t.Context(), &got, Config{
		Path: path,
		LookupEnv: mapLookup(map[string]string{
			"PROVIDERS_OPENAI_MODEL": "env-model",
		}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := got.Providers["openai"]
	if !ok {
		t.Fatal("missing providers.openai")
	}
	if p.Model != "env-model" {
		t.Errorf("model = %q, want env-model", p.Model)
	}

	var gotFlag struct {
		Providers map[string]provider `toml:"providers"`
	}
	_, err = Load(t.Context(), &gotFlag, Config{
		Path:      path,
		LookupEnv: mapLookup(nil),
		Flags:     map[string]string{"providers.openai.model": "flag-model"},
	})
	if err != nil {
		t.Fatalf("Load flag: %v", err)
	}
	p, ok = gotFlag.Providers["openai"]
	if !ok {
		t.Fatal("missing flag providers.openai")
	}
	if p.Model != "flag-model" {
		t.Errorf("flag model = %q, want flag-model", p.Model)
	}
}

func TestLoadNilPointerNestedEnv(t *testing.T) {
	type render struct {
		Model string `toml:"model"`
	}
	var got struct {
		Render *render `toml:"render"`
	}
	_, err := Load(t.Context(), &got, Config{
		LookupEnv: mapLookup(map[string]string{"RENDER_MODEL": "from-env"}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Render == nil {
		t.Fatal("render was not allocated")
	}
	if got.Render.Model != "from-env" {
		t.Errorf("model = %q, want from-env", got.Render.Model)
	}
}

func TestLoadMapOfMapsSecretResolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	doc := `
[groups.prod.a]
source = "file"
path = "/run/secrets/a"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Groups map[string]map[string]Secret `toml:"groups"`
	}
	plan, err := Load(t.Context(), &got, Config{
		Path:      path,
		LookupEnv: mapLookup(nil),
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inner, ok := got.Groups["prod"]
	if !ok {
		t.Fatal("missing groups.prod")
	}
	sec, ok := inner["a"]
	if !ok {
		t.Fatal("missing groups.prod.a")
	}
	val, err := sec.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != secretFixtureValue {
		t.Errorf("secret resolved to the wrong value")
	}
	p := plan.String()
	if !strings.Contains(p, "groups.prod.a") {
		t.Errorf("plan missing nested key:\n%s", p)
	}
	assertQuiet(t, p, secretFixtureValue)
}

func TestLoadSliceOfMapsSecretResolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	doc := `rows = [{a = {source = "file", path = "/run/secrets/a"}}]`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Rows []map[string]Secret `toml:"rows"`
	}
	plan, err := Load(t.Context(), &got, Config{
		Path:      path,
		LookupEnv: mapLookup(nil),
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(got.Rows))
	}
	sec, ok := got.Rows[0]["a"]
	if !ok {
		t.Fatal("missing rows.0.a")
	}
	val, err := sec.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != secretFixtureValue {
		t.Errorf("secret resolved to the wrong value")
	}
	p := plan.String()
	if !strings.Contains(p, "rows.0.a") && !strings.Contains(p, "rows.a") {
		t.Errorf("plan missing nested key:\n%s", p)
	}
	assertQuiet(t, p, secretFixtureValue)
}

func TestLoadMapOfSecretListsResolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	doc := `groups = { prod = [{source = "file", path = "/run/secrets/a"}] }`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Groups map[string][]Secret `toml:"groups"`
	}
	plan, err := Load(t.Context(), &got, Config{
		Path:      path,
		LookupEnv: mapLookup(nil),
		Registry:  valueRegistry(t, secretFixtureValue),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	list, ok := got.Groups["prod"]
	if !ok {
		t.Fatal("missing groups.prod")
	}
	if len(list) != 1 {
		t.Fatalf("len(groups.prod) = %d, want 1", len(list))
	}
	val, err := list[0].Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != secretFixtureValue {
		t.Errorf("secret resolved to the wrong value")
	}
	p := plan.String()
	if !strings.Contains(p, "groups.prod") {
		t.Errorf("plan missing nested key:\n%s", p)
	}
	assertQuiet(t, p, secretFixtureValue)
}

func TestLoadMapOfMapsStructEnvOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte("[groups.prod.openai]\nmodel = \"file-model\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got struct {
		Groups map[string]map[string]struct {
			Model string `toml:"model"`
		} `toml:"groups"`
	}
	_, err := Load(t.Context(), &got, Config{
		Path: path,
		LookupEnv: mapLookup(map[string]string{
			"GROUPS_PROD_OPENAI_MODEL": "env-model",
		}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inner, ok := got.Groups["prod"]
	if !ok {
		t.Fatal("missing groups.prod")
	}
	p, ok := inner["openai"]
	if !ok {
		t.Fatal("missing groups.prod.openai")
	}
	if p.Model != "env-model" {
		t.Errorf("model = %q, want env-model", p.Model)
	}
}

func TestLoadCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var got struct {
		Model string `toml:"model"`
	}
	_, err := Load(ctx, &got, Config{LookupEnv: mapLookup(nil)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want Canceled", err)
	}
}

func assertQuiet(t *testing.T, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Errorf("output carries a secret value")
		}
	}
}
