package main

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCheckFile pins all three rules against representative sources. The cases
// cover the exemptions the checker must honour and the breaches it must catch.
func TestCheckFile(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		isTest bool
		want   []string
	}{
		{
			name: "env read in code",
			src:  "package p\n\nimport \"os\"\n\n// F reads env.\nfunc F() string { return os.Getenv(\"X\") }\n",
			want: []string{ruleEnv},
		},
		{
			name:   "env read in test",
			src:    "package p\n\nimport \"os\"\n\n// F reads env.\nfunc F() string { return os.Getenv(\"X\") }\n",
			isTest: true,
		},
		{
			name: "aliased env read",
			src:  "package p\n\nimport o \"os\"\n\n// F reads env.\nfunc F() string { return o.Getenv(\"X\") }\n",
			want: []string{ruleEnv},
		},
		{
			name: "dot imported env read",
			src:  "package p\n\nimport . \"os\"\n\n// F reads env.\nfunc F() string { return Getenv(\"X\") }\n",
			want: []string{ruleEnv},
		},
		{
			name: "dot imported fatal call",
			src:  "package p\n\nimport . \"log\"\n\n// F ends the process.\nfunc F() { Fatal(\"x\") }\n",
			want: []string{ruleFatal},
		},
		{
			name: "environ and lookup env",
			src:  "package p\n\nimport \"os\"\n\n// F reads env.\nfunc F() {\n\t_ = os.Environ()\n\t_, _ = os.LookupEnv(\"X\")\n}\n",
			want: []string{ruleEnv, ruleEnv},
		},
		{
			name: "log fatal and panic",
			src:  "package p\n\nimport \"log\"\n\n// F ends the process.\nfunc F() { log.Fatalf(\"x\") }\n\n// G panics.\nfunc G() { panic(\"x\") }\n",
			want: []string{ruleFatal, ruleFatal},
		},
		{
			name:   "log fatal in test",
			src:    "package p\n\nimport \"log\"\n\n// F ends the process.\nfunc F() { log.Fatal(\"x\") }\n",
			isTest: true,
		},
		{
			name: "documented exported function",
			src:  "package p\n\n// F does.\nfunc F() {}\n",
		},
		{
			name: "undocumented exported function",
			src:  "package p\n\nfunc F() {}\n",
			want: []string{ruleDoc},
		},
		{
			name: "doc that does not name the function",
			src:  "package p\n\n// Elsewhere does.\nfunc F() {}\n",
			want: []string{ruleDoc},
		},
		{
			name: "method on unexported type",
			src:  "package p\n\ntype t struct{}\n\nfunc (t) F() {}\n",
		},
		{
			name: "method on exported type",
			src:  "package p\n\n// T does.\ntype T struct{}\n\nfunc (T) F() {}\n",
			want: []string{ruleDoc},
		},
		{
			name: "grouped consts each documented",
			src:  "package p\n\nconst (\n\t// A does.\n\tA = 1\n\t// B does.\n\tB = 2\n)\n",
		},
		{
			name: "grouped const with one undocumented",
			src:  "package p\n\nconst (\n\t// A does.\n\tA = 1\n\tB = 2\n)\n",
			want: []string{ruleDoc},
		},
		{
			name: "single const carries its doc on the declaration",
			src:  "package p\n\n// A does.\nconst A = 1\n",
		},
		{
			name: "single type carries its doc on the declaration",
			src:  "package p\n\n// T does.\ntype T struct{}\n",
		},
		{
			name: "undocumented exported type",
			src:  "package p\n\ntype T struct{}\n",
			want: []string{ruleDoc},
		},
		{
			name: "undocumented exported var",
			src:  "package p\n\nvar V = 1\n",
			want: []string{ruleDoc},
		},
		{
			name:   "undocumented test entry point",
			src:    "package p\n\nfunc TestF(t *testing.T) {}\n",
			isTest: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "src.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse source: %v", err)
			}
			got := rules(checkFile(fset, "src.go", file, tc.isTest))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rules = %v, want %v", got, tc.want)
			}
		})
	}
}

// rules reduces violations to their rule names in the order they were found.
func rules(violations []violation) []string {
	names := make([]string, 0, len(violations))
	for _, v := range violations {
		names = append(names, v.rule)
	}
	return names
}

// TestPackageDirs pins the package set the checker walks. A package whose
// every file a build constraint excludes from this platform is still listed,
// while the directories the go tool keeps out of the ./... pattern stay out.
func TestPackageDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/scratch\n\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, "plain", "plain.go"), "package plain\n")
	writeFile(t, filepath.Join(root, "winonly", "only_windows.go"),
		"//go:build windows\n\npackage winonly\n\nimport \"os\"\n\n// Only reads the environment.\nfunc Only() string { return os.Getenv(\"X\") }\n")
	writeFile(t, filepath.Join(root, "ignoreonly", "gen.go"),
		"//go:build ignore\n\npackage ignoreonly\n")
	writeFile(t, filepath.Join(root, "testdata", "inner", "inner.go"), "package inner\n")
	writeFile(t, filepath.Join(root, "nested", "go.mod"), "module example.com/nested\n\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, "nested", "nested.go"), "package nested\n")
	writeFile(t, filepath.Join(root, "_under", "under.go"), "package under\n")
	writeFile(t, filepath.Join(root, ".hidden", "hidden.go"), "package hidden\n")
	writeFile(t, filepath.Join(root, "vendor", "vendored", "v.go"), "package vendored\n")

	dirs, err := packageDirs(root)
	if err != nil {
		t.Fatalf("packageDirs: %v", err)
	}
	got := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		got[dir] = true
	}
	for _, want := range []string{
		filepath.Join(root, "plain"),
		filepath.Join(root, "winonly"),
		filepath.Join(root, "ignoreonly"),
	} {
		if !got[want] {
			t.Errorf("packageDirs missed %s", want)
		}
	}
	for _, unwanted := range []string{
		filepath.Join(root, "testdata", "inner"),
		filepath.Join(root, "nested"),
		filepath.Join(root, "_under"),
		filepath.Join(root, ".hidden"),
		filepath.Join(root, "vendor", "vendored"),
	} {
		if got[unwanted] {
			t.Errorf("packageDirs walked %s", unwanted)
		}
	}
}

// TestCheckReportsExcludedPackage pins the checker end to end. A package whose
// only file carries a build constraint that excludes this platform must still
// report a breach, since a build on that platform would compile the call.
func TestCheckReportsExcludedPackage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/scratch\n\ngo 1.27.1\n")
	writeFile(t, filepath.Join(root, "winonly", "only_windows.go"),
		"//go:build windows\n\npackage winonly\n\nimport \"os\"\n\n// Only reads the environment.\nfunc Only() string { return os.Getenv(\"X\") }\n")
	t.Chdir(root)

	violations, err := check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, v := range violations {
		if v.rule == ruleEnv && strings.HasSuffix(v.path, "only_windows.go") {
			return
		}
	}
	t.Fatalf("check reported %v, want a %s breach in only_windows.go", rules(violations), ruleEnv)
}

// writeFile writes content to path and creates any missing parent directory.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
