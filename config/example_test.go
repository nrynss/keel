package config_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nrynss/keel/config"
	"github.com/nrynss/keel/config/source"
)

type exampleAppSettings struct {
	Render struct {
		Model string `toml:"model"`
	} `toml:"render"`
	Secrets struct {
		Key config.Secret `toml:"key"`
	} `toml:"secrets"`
}

// ExampleLoad reads settings and one secret through the shipped sources.
// It prints the plain setting and the plan. It never prints the value.
func ExampleLoad() {
	dir, err := os.MkdirTemp("", "keel-config-example")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	secretPath := filepath.Join(dir, "signing.key")
	if err := os.WriteFile(secretPath, []byte("example-secret\n"), 0o600); err != nil {
		fmt.Println("secret write failed:", err)
		return
	}
	settingsPath := filepath.Join(dir, "app.toml")
	doc := "[render]\nmodel = \"example-model\"\n\n[secrets.key]\nsource = \"file\"\npath = \"" + secretPath + "\"\n"
	if err := os.WriteFile(settingsPath, []byte(doc), 0o600); err != nil {
		fmt.Println("settings write failed:", err)
		return
	}

	reg := config.NewRegistry()
	if err := source.RegisterDefaults(reg, source.Config{}); err != nil {
		fmt.Println("register failed:", err)
		return
	}

	var got exampleAppSettings
	plan, err := config.Load(context.Background(), &got, config.Config{
		Path:      settingsPath,
		LookupEnv: func(string) (string, bool) { return "", false },
		Registry:  reg,
	})
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}
	if _, err := got.Secrets.Key.Reveal(); err != nil {
		fmt.Println("reveal failed:", err)
		return
	}

	fmt.Println(got.Render.Model)
	fmt.Println("secret resolved")
	fmt.Print(strings.ReplaceAll(plan.String(), dir, "<dir>"))
	// Output:
	// example-model
	// secret resolved
	// render.model	file	path=<dir>/app.toml	resolved
	// secrets.key	file	path=<dir>/signing.key	resolved
}
