package workstationtest

import (
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

func TestNoDirectToolLookup(t *testing.T) {
	root := filepath.Join("..", "..")
	err := fs.WalkDir(os.DirFS(root), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != "." {
			name := entry.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
		}
		if entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(root, path), nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, pos := range directToolLookups(file) {
			t.Errorf("%s: use workstationtest.Tool for test prerequisites", fset.Position(pos))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func directToolLookups(file *ast.File) []token.Pos {
	execName := ""
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		if path == "os/exec" {
			execName = "exec"
			if spec.Name != nil {
				execName = spec.Name.Name
			}
			break
		}
	}
	if execName == "" || execName == "_" {
		return nil
	}

	// Inspect references as well as calls, so assigning LookPath to another
	// variable or passing it as a callback cannot bypass the check.
	var positions []token.Pos
	for node := range ast.Preorder(file) {
		switch expr := node.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := expr.X.(*ast.Ident); ok && pkg.Name == execName && expr.Sel.Name == "LookPath" {
				positions = append(positions, expr.Pos())
			}
		case *ast.Ident:
			if execName == "." && expr.Name == "LookPath" {
				positions = append(positions, expr.Pos())
			}
		}
	}
	return positions
}

func TestDirectToolLookups(t *testing.T) {
	cases := map[string]string{
		"direct":         `import "os/exec"; func probe() { _, _ = exec.LookPath("tool") }`,
		"function alias": `import "os/exec"; var lp = exec.LookPath; func probe() { _, _ = lp("tool") }`,
		"import alias":   `import command "os/exec"; func probe() { _, _ = command.LookPath("tool") }`,
		"dot import":     `import . "os/exec"; func probe() { _, _ = LookPath("tool") }`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture_test.go", "package fixture; "+source, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := directToolLookups(file); len(got) != 1 {
				t.Fatalf("got %d violations, want 1", len(got))
			}
		})
	}
}
