// Package main checks the Keel Go conventions that gofmt and go vet cannot see.
//
// Four rules are enforced and each breach prints one FAIL line that names the
// rule and the file position:
//
//   - no call to os.Getenv, os.LookupEnv or os.Environ outside test files
//   - no panic or log.Fatal call outside test files
//   - every exported identifier carries a doc comment naming it
//   - no exported identifier in a file the linux/amd64 build context excludes
//
// The checker walks every package under the module root. It exits 1 when it
// finds any breach and stays silent when the tree is clean.
package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	ruleEnv   = "no-env-outside-tests"
	ruleFatal = "no-panic-or-log-fatal-outside-tests"
	ruleDoc   = "exported-doc-comment"
	ruleBuild = "exported-api-outside-build-context"
)

// frozenGOOS and frozenGOARCH name the one build context the two records under
// api/ are generated for. It is also the context CI runs on, so every exported
// identifier a consumer can reach must be declared inside it.
const (
	frozenGOOS   = "linux"
	frozenGOARCH = "amd64"
)

// frozenContext is the build context the api/ records are taken from. MatchFile
// reads each file against it to decide whether that file compiles into those
// records, so a file from another context cannot grow the API surface unseen.
var frozenContext = build.Context{
	GOOS:        frozenGOOS,
	GOARCH:      frozenGOARCH,
	Compiler:    "gc",
	ReleaseTags: build.Default.ReleaseTags,
}

// envReads holds the os functions that read the process environment.
var envReads = map[string]bool{
	"Getenv":    true,
	"LookupEnv": true,
	"Environ":   true,
}

// fatalCalls holds the log functions that end the process.
var fatalCalls = map[string]bool{
	"Fatal":   true,
	"Fatalf":  true,
	"Fatalln": true,
}

// violation is one convention breach, ready to print as a FAIL line.
type violation struct {
	rule   string
	path   string
	line   int
	detail string
}

// String formats the breach as the single FAIL line the gate prints.
func (v violation) String() string {
	return fmt.Sprintf("FAIL %s: %s:%d: %s", v.rule, v.path, v.line, v.detail)
}

// main prints every convention breach and exits 1 when one exists.
func main() {
	violations, err := check(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", "conventions", err)
		os.Exit(1)
	}
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, v)
	}
	if len(violations) > 0 {
		os.Exit(1)
	}
}

// check collects every convention breach under the module root.
func check(ctx context.Context) ([]violation, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	dirs, err := packageDirs(wd)
	if err != nil {
		return nil, err
	}
	var violations []violation
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := checkDir(ctx, wd, dir)
		if err != nil {
			return nil, err
		}
		violations = append(violations, found...)
	}
	return violations, nil
}

// packageDirs lists the absolute directory of every Go package at or below
// root. It walks the tree itself instead of asking the go tool for the
// buildable packages, so a package whose every file a build constraint
// excludes from this platform is still listed and still checked.
//
// It keeps out what the go tool keeps out of the ./... pattern. A nested
// module, a testdata directory, a vendor directory, and a name that begins
// with a dot or an underscore all stay out of scope.
func packageDirs(root string) ([]string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("absolute path for %s: %w", root, err)
	}
	var dirs []string
	err = filepath.WalkDir(abs, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if path != abs {
			if skipDirName(entry.Name()) || hasGoMod(path) {
				return fs.SkipDir
			}
		}
		hasGo, err := hasGoFiles(path)
		if err != nil {
			return err
		}
		if hasGo {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", abs, err)
	}
	return dirs, nil
}

// skipDirName reports whether the go tool keeps a directory of this name out
// of the ./... pattern.
func skipDirName(name string) bool {
	if name == "testdata" || name == "vendor" {
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// hasGoMod reports whether dir holds a go.mod, which marks a nested module.
func hasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// hasGoFiles reports whether dir holds at least one .go file.
func hasGoFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return true, nil
		}
	}
	return false, nil
}

// checkDir reports every convention breach across one package directory.
func checkDir(ctx context.Context, wd, dir string) ([]violation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	var violations []violation
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		isTest := strings.HasSuffix(name, "_test.go")
		excluded, err := outsideBuildContext(dir, name, isTest)
		if err != nil {
			return nil, err
		}
		violations = append(violations, checkFile(fset, relPath(wd, path), file, isTest, excluded)...)
	}
	return violations, nil
}

// outsideBuildContext reports whether the frozen context excludes a file, so the
// file compiles on another platform only. A test file and a file whose name
// starts with an underscore or a dot stay out of scope and are never excluded.
// MatchFile reads the file, so a read error is an error rather than a pass.
func outsideBuildContext(dir, name string, isTest bool) (bool, error) {
	if isTest || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
		return false, nil
	}
	match, err := frozenContext.MatchFile(dir, name)
	if err != nil {
		return false, fmt.Errorf("match %s: %w", filepath.Join(dir, name), err)
	}
	return !match, nil
}

// relPath shortens an absolute path against the working directory.
func relPath(wd, path string) string {
	if wd == "" {
		return path
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil {
		return path
	}
	return rel
}

// checkFile reports every convention breach inside one parsed Go file.
//
// A file the frozen build context excludes is checked for exported identifiers,
// since those declarations reach another platform while the records miss them.
func checkFile(fset *token.FileSet, path string, file *ast.File, isTest, excluded bool) []violation {
	var violations []violation
	add := func(rule string, pos token.Pos, detail string) {
		violations = append(violations, violation{
			rule:   rule,
			path:   path,
			line:   fset.Position(pos).Line,
			detail: detail,
		})
	}
	if excluded {
		checkExcludedExports(file, add)
	}
	if !isTest {
		checkCalls(file, importNames(file), add)
	}
	checkDocs(file, isTest, add)
	return violations
}

// importNames maps each imported path to the local name the file uses for it.
func importNames(file *ast.File) map[string]string {
	names := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[path] = name
	}
	return names
}

// checkCalls reports environment reads and process-ending calls in one file.
func checkCalls(file *ast.File, imports map[string]string, add func(string, token.Pos, string)) {
	osName := imports["os"]
	logName := imports["log"]
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			switch {
			case fn.Name == "panic":
				add(ruleFatal, call.Pos(), "calls panic")
			case osName == "." && envReads[fn.Name]:
				add(ruleEnv, call.Pos(), "calls os."+fn.Name)
			case logName == "." && fatalCalls[fn.Name]:
				add(ruleFatal, call.Pos(), "calls log."+fn.Name)
			}
		case *ast.SelectorExpr:
			pkg, ok := fn.X.(*ast.Ident)
			if !ok {
				return true
			}
			if osName != "" && pkg.Name == osName && envReads[fn.Sel.Name] {
				add(ruleEnv, call.Pos(), "calls os."+fn.Sel.Name)
			}
			if logName != "" && pkg.Name == logName && fatalCalls[fn.Sel.Name] {
				add(ruleFatal, call.Pos(), "calls log."+fn.Sel.Name)
			}
		}
		return true
	})
}

// checkDocs reports exported package-level identifiers whose doc comment is
// missing or does not start with the identifier's own name.
//
// A method on an unexported type is not part of the exported surface, so it is
// skipped. A test, benchmark, fuzz or example function is skipped too, because
// Go reads the comment above those as an instruction of its own.
func checkDocs(file *ast.File, isTest bool, add func(string, token.Pos, string)) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			checkFuncDoc(d, isTest, add)
		case *ast.GenDecl:
			checkGenDeclDoc(d, add)
		}
	}
}

// checkFuncDoc reports one exported function or method with a bad doc comment.
func checkFuncDoc(fn *ast.FuncDecl, isTest bool, add func(string, token.Pos, string)) {
	if !fn.Name.IsExported() {
		return
	}
	if fn.Recv != nil && !ast.IsExported(receiverName(fn.Recv)) {
		return
	}
	if isTest && isTestFuncName(fn.Name.Name) {
		return
	}
	if !docNames(fn.Doc, fn.Name.Name) {
		add(ruleDoc, fn.Pos(), "exported function "+fn.Name.Name+" has no doc comment naming it")
	}
}

// checkGenDeclDoc reports exported types, consts and vars with a bad doc.
func checkGenDeclDoc(decl *ast.GenDecl, add func(string, token.Pos, string)) {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if s.Name.IsExported() && !docNames(docForSpec(s.Doc, decl), s.Name.Name) {
				add(ruleDoc, s.Pos(), "exported type "+s.Name.Name+" has no doc comment naming it")
			}
		case *ast.ValueSpec:
			doc := docForSpec(s.Doc, decl)
			for _, name := range s.Names {
				if name.IsExported() && !docNames(doc, name.Name) {
					add(ruleDoc, name.Pos(), "exported "+decl.Tok.String()+" "+name.Name+" has no doc comment naming it")
				}
			}
		}
	}
}

// checkExcludedExports reports every exported package-level identifier declared
// in a file the frozen build context excludes. The identifier compiles on
// another platform, so it reaches that platform's consumers, while the records
// under api/ never list it. A file whose declarations are all unexported stays
// clean, because a platform implementation seam is legitimate.
func checkExcludedExports(file *ast.File, add func(string, token.Pos, string)) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			checkExcludedFunc(d, add)
		case *ast.GenDecl:
			checkExcludedGenDecl(d, add)
		}
	}
}

// checkExcludedFunc reports one exported function or method declared outside
// the frozen build context. A method on an unexported receiver is not part of
// the exported surface, so it is skipped.
func checkExcludedFunc(fn *ast.FuncDecl, add func(string, token.Pos, string)) {
	if !fn.Name.IsExported() {
		return
	}
	if fn.Recv != nil && !ast.IsExported(receiverName(fn.Recv)) {
		return
	}
	addExcludedExport(add, fn.Name.Pos(), "function", fn.Name.Name)
}

// checkExcludedGenDecl reports exported types, consts and vars declared outside
// the frozen build context.
func checkExcludedGenDecl(decl *ast.GenDecl, add func(string, token.Pos, string)) {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if s.Name.IsExported() {
				addExcludedExport(add, s.Name.Pos(), "type", s.Name.Name)
			}
		case *ast.ValueSpec:
			for _, name := range s.Names {
				if name.IsExported() {
					addExcludedExport(add, name.Pos(), decl.Tok.String(), name.Name)
				}
			}
		}
	}
}

// addExcludedExport records one exported identifier declared outside the frozen
// build context. The detail names the consequence and the way out.
func addExcludedExport(add func(string, token.Pos, string), pos token.Pos, kind, name string) {
	add(ruleBuild, pos, fmt.Sprintf("exported %s %s is excluded from the %s/%s build context, so the records under api/ never cover it. Compile it inside that context or unexport it.", kind, name, frozenGOOS, frozenGOARCH))
}

// docForSpec returns the comment that documents one spec. A declaration that
// holds a single spec may carry its doc on the GenDecl instead of the spec.
func docForSpec(spec *ast.CommentGroup, decl *ast.GenDecl) *ast.CommentGroup {
	if spec != nil {
		return spec
	}
	if len(decl.Specs) == 1 {
		return decl.Doc
	}
	return nil
}

// receiverName returns the bare type name of a method receiver.
func receiverName(recv *ast.FieldList) string {
	if len(recv.List) == 0 {
		return ""
	}
	name := ""
	ast.Inspect(recv.List[0].Type, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		if id, ok := n.(*ast.Ident); ok {
			name = id.Name
			return false
		}
		return true
	})
	return name
}

// isTestFuncName reports whether name is one of the functions Go treats as a
// test entry point, whose leading comment is not a doc comment.
func isTestFuncName(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// docNames reports whether doc carries a comment that starts with name.
func docNames(doc *ast.CommentGroup, name string) bool {
	if doc == nil {
		return false
	}
	text := strings.TrimSpace(doc.Text())
	if !strings.HasPrefix(text, name) {
		return false
	}
	rest := text[len(name):]
	return rest == "" || !isWordByte(rest[0])
}

// isWordByte reports whether c can continue a Go identifier.
func isWordByte(c byte) bool {
	return c == '_' ||
		c >= '0' && c <= '9' ||
		c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= 0x80
}
