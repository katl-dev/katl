package kubeadmconfig

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

// ownershipViolations reports where a source file names the kubeadm input
// tree or renders an input file at the path the input carries, which only
// this package may do.
func ownershipViolations(fset *token.FileSet, file *ast.File) []string {
	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				break
			}
			if value, err := strconv.Unquote(node.Value); err == nil && strings.Contains(value, "etc/katl/kubeadm") {
				violations = append(violations, fset.Position(node.Pos()).String()+": literal names the kubeadm input tree")
			}
		case *ast.CompositeLit:
			selector, ok := node.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "NativeEtcFile" {
				break
			}
			for _, element := range node.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := pair.Key.(*ast.Ident)
				path, _ := pair.Value.(*ast.SelectorExpr)
				if key != nil && key.Name == "Path" && path != nil && path.Sel.Name == "RenderPath" {
					violations = append(violations, fset.Position(node.Pos()).String()+": NativeEtcFile written at an input RenderPath")
				}
			}
		}
		return true
	})
	return violations
}

func TestOwnershipViolationsDetection(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		want         int
	}{
		{name: "literal", source: `package p; var x = "/etc/katl/kubeadm/" + ref`, want: 1},
		{name: "raw literal", source: "package p; var x = `etc/katl/kubeadm`", want: 1},
		{name: "joined literal", source: `package p; var x = filepath.Join("/etc/katl/kubeadm", ref)`, want: 1},
		{name: "render path", source: `package p; var x = confext.NativeEtcFile{Path: input.RenderPath, Content: "x"}`, want: 1},
		{name: "computed path", source: `package p; var x = confext.NativeEtcFile{Path: kubeadmconfig.BootstrapInitConfigPath}`},
		{name: "other path", source: `package p; var x = "/etc/katl/bootstrap/kubeadm-init.yaml"`},
		{name: "comment", source: "package p\n// /etc/katl/kubeadm names the tree\nvar x = 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", tt.source, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			if got := ownershipViolations(fset, file); len(got) != tt.want {
				t.Fatalf("violations = %v, want %d", got, tt.want)
			}
		})
	}
}

// Only this package writes under /etc/katl/kubeadm. Other code asks it for
// paths and for the files of a NodeInput. Tests and documentation may name the
// tree to describe fixtures and expectations.
func TestOnlyKubeadmconfigOwnsKubeadmInputTree(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("repository root not found")
		}
		root = parent
	}
	owner := filepath.Join(root, "internal", "installer", "kubeadmconfig")

	var violations []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == owner || path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		violations = append(violations, ownershipViolations(fset, file)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(strings.TrimPrefix(violation, root+string(filepath.Separator)))
	}
}
