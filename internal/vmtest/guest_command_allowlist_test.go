package vmtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGuestCommandsAllowlisted checks that every literal program a VM scenario
// asks the guest agent to run is one the agent permits. Without it, a command
// outside the allowlist fails only when its VM test reaches that step.
func TestGuestCommandsAllowlisted(t *testing.T) {
	// agent_test.go sends disallowed commands to exercise the rejection path.
	const exempt = "agent_test.go"
	allowed := defaultAgentCommands()
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || entry.Name() == exempt {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			for _, program := range guestPrograms(node) {
				checked++
				if !commandAllowed(program.name, allowed) {
					t.Errorf("%s: guest command %q is not allowlisted by the vmtest agent", fset.Position(program.pos), program.name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("found no guest commands; the scan no longer matches how scenarios run them")
	}
}

type guestProgram struct {
	name string
	pos  token.Pos
}

// guestPrograms returns the literal argv[0] of a guest command expressed as a
// guestCommand or guestCommandOutput call or as an Argv field literal.
func guestPrograms(node ast.Node) []guestProgram {
	switch node := node.(type) {
	case *ast.CallExpr:
		name, ok := node.Fun.(*ast.Ident)
		if !ok || (name.Name != "guestCommand" && name.Name != "guestCommandOutput") || len(node.Args) < 5 {
			return nil
		}
		return literalProgram(node.Args[4])
	case *ast.KeyValueExpr:
		key, ok := node.Key.(*ast.Ident)
		if !ok || key.Name != "Argv" {
			return nil
		}
		argv, ok := node.Value.(*ast.CompositeLit)
		if !ok || len(argv.Elts) == 0 {
			return nil
		}
		return literalProgram(argv.Elts[0])
	}
	return nil
}

func literalProgram(expr ast.Expr) []guestProgram {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil
	}
	name, err := strconv.Unquote(literal.Value)
	if err != nil {
		return nil
	}
	return []guestProgram{{name: name, pos: literal.Pos()}}
}
