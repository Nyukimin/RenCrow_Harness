package client

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The architecture of pkg/client, held by tests: it is a public package that other
// repositories import, so what it depends on, what it reads from its caller's process and
// what it documents are contracts and not habits.

func modulePath(t *testing.T) string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("no module line in go.mod")
	return ""
}

// productionFiles parses the non-test Go files of dir.
func productionFiles(t *testing.T, dir string, mode parser.Mode) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, mode)
		if err != nil {
			return err
		}
		out[path] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func imports(f *ast.File) []string {
	var out []string
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		out = append(out, p)
	}
	return out
}

func TestTheClientDependsOnTheStandardLibraryAndPackageProtocolOnly(t *testing.T) {
	mod := modulePath(t)
	files := productionFiles(t, ".", parser.ImportsOnly)
	if len(files) < 5 {
		t.Fatalf("only %d source files found", len(files))
	}
	for name, f := range files {
		for _, p := range imports(f) {
			first, _, _ := strings.Cut(p, "/")
			stdlib := !strings.Contains(first, ".")
			if !stdlib && p != mod+"/pkg/protocol" {
				t.Errorf("%s imports %s: pkg/client may import the standard library and %s/pkg/protocol only", name, p, mod)
			}
			if strings.Contains(p, "/internal/") || strings.HasSuffix(p, "/internal") {
				t.Errorf("%s imports an internal package: %s", name, p)
			}
		}
	}
}

func TestNothingInTheEngineOrTheCommandImportsTheClient(t *testing.T) {
	mod := modulePath(t)
	for _, root := range []string{filepath.Join("..", "..", "internal"), filepath.Join("..", "..", "cmd"), filepath.Join("..", "protocol")} {
		for name, f := range productionFiles(t, root, parser.ImportsOnly) {
			for _, p := range imports(f) {
				if p == mod+"/pkg/client" {
					t.Errorf("%s imports pkg/client: the client is for the callers of the Harness, not for the Harness", name)
				}
			}
		}
	}
}

func TestTheClientNeverReadsTheEnvironmentOrTheDirectoryOfItsCallersProcess(t *testing.T) {
	banned := map[string]bool{
		"Environ": true, "Getenv": true, "LookupEnv": true, "ExpandEnv": true, "Expand": true,
		"Getwd": true, "UserHomeDir": true, "UserConfigDir": true, "UserCacheDir": true, "TempDir": true,
	}
	for name, f := range productionFiles(t, ".", 0) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && banned[sel.Sel.Name] {
				t.Errorf("%s uses os.%s: everything the child gets is the Config's", name, sel.Sel.Name)
			}
			return true
		})
	}
}

func TestEveryExportedIdentifierIsDocumented(t *testing.T) {
	for name, f := range productionFiles(t, ".", parser.ParseComments) {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv != nil {
					// A method of an unexported type is not part of the API.
					if !receiverExported(d.Recv) {
						continue
					}
				}
				if d.Doc == nil || strings.TrimSpace(d.Doc.Text()) == "" {
					t.Errorf("%s: %s has no doc comment", name, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() && d.Doc == nil && s.Doc == nil {
							t.Errorf("%s: type %s has no doc comment", name, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if id.IsExported() && d.Doc == nil && s.Doc == nil {
								t.Errorf("%s: %s has no doc comment", name, id.Name)
							}
						}
					}
				}
			}
		}
	}
}

func receiverExported(r *ast.FieldList) bool {
	if r == nil || len(r.List) == 0 {
		return false
	}
	switch x := r.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.IsExported()
		}
	case *ast.Ident:
		return x.IsExported()
	}
	return false
}
