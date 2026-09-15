package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Example_check runs the checker over a temporary module tree and prints each
// breach it reports.
func Example_check() {
	root, err := os.MkdirTemp("", "keel-conventions-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(root)

	pkgDir := filepath.Join(root, "app")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		fmt.Println("mkdir failed:", err)
		return
	}
	source := "package app\n\nimport \"os\"\n\n// Config carries the mode.\nvar Config = os.Getenv(\"APP_MODE\")\n\nfunc Enabled() bool { return true }\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "app.go"), []byte(source), 0o644); err != nil {
		fmt.Println("write failed:", err)
		return
	}

	dirs, err := packageDirs(root)
	if err != nil {
		fmt.Println("walk failed:", err)
		return
	}
	var violations []violation
	for _, dir := range dirs {
		found, err := checkDir(context.Background(), root, dir)
		if err != nil {
			fmt.Println("check failed:", err)
			return
		}
		violations = append(violations, found...)
	}

	for _, v := range violations {
		fmt.Println(v)
	}
	// Output:
	// FAIL no-env-outside-tests: app/app.go:6: calls os.Getenv
	// FAIL exported-doc-comment: app/app.go:8: exported function Enabled has no doc comment naming it
}
