package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nrynss/keel/config"
	"github.com/nrynss/keel/config/source"
)

const extSecretValue = "fixture-plain-ok"

type extSettings struct {
	Model  string        `toml:"model"`
	Secret config.Secret `toml:"secret"`
}

func TestLoadResolvesEnvFileSource(t *testing.T) {
	dir := t.TempDir()
	envPath := copyMode(t, filepath.Join("..", "testdata", "config", "env_file.plain"), dir, 0o600)
	doc := "model = \"ok\"\n\n[secret]\nsource = \"env_file\"\npath = \"" + envPath + "\"\nvar = \"PROVIDER_API_KEY\"\n"
	settingsPath := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(settingsPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	reg := config.NewRegistry()
	if err := source.RegisterDefaults(reg, source.Config{LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		t.Fatalf("RegisterDefaults: %v", err)
	}

	var got extSettings
	plan, err := config.Load(t.Context(), &got, config.Config{
		Path:      settingsPath,
		LookupEnv: func(string) (string, bool) { return "", false },
		Registry:  reg,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	val, err := got.Secret.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != extSecretValue {
		t.Errorf("resolved value does not match")
	}
	if got.Model != "ok" {
		t.Errorf("model = %q, want ok", got.Model)
	}

	dump, err := config.Dump(&got, plan)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	outputs := []string{plan.String(), string(dump), errString(err)}
	for _, out := range outputs {
		if strings.Contains(out, extSecretValue) {
			t.Errorf("output carries a secret value")
		}
	}
	if !strings.Contains(plan.String(), "secret\tenv_file\t") {
		t.Errorf("plan = %q", plan.String())
	}
	if !strings.Contains(plan.String(), "var=PROVIDER_API_KEY") {
		t.Errorf("plan missing locator: %q", plan.String())
	}
}

func copyMode(t *testing.T, src, dir string, mode os.FileMode) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return dst
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
