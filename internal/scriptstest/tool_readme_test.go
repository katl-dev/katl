package scriptstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestToolREADMECommands(t *testing.T) {
	repo := repoRoot(t)
	for _, tool := range []struct {
		name     string
		selector string
	}{
		{"katl-queue", "command"},
		{"katl-release", "args[0]"},
	} {
		t.Run(tool.name, func(t *testing.T) {
			dir := filepath.Join(repo, "cmd", tool.name)
			source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "main.go"), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
			if err != nil {
				t.Fatal(err)
			}

			// Read dispatch directly so a command omitted from both --help and
			// the README still fails this check.
			commands := map[string]bool{}
			for _, decl := range source.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
					for node := range ast.Preorder(fn.Body) {
						switch node := node.(type) {
						case *ast.SwitchStmt:
							if node.Tag != nil && types.ExprString(node.Tag) == tool.selector {
								for _, stmt := range node.Body.List {
									for _, choice := range stmt.(*ast.CaseClause).List {
										recordToolCommand(commands, choice)
									}
								}
							}
						case *ast.BinaryExpr:
							if (node.Op == token.EQL || node.Op == token.NEQ) && types.ExprString(node.X) == tool.selector {
								recordToolCommand(commands, node.Y)
							}
						}
					}
				}
			}
			if len(commands) == 0 {
				t.Fatal("no commands found in run dispatch")
			}
			headings := strings.Split(string(readme), "\n")
			for command := range commands {
				heading := "### `" + command + "`"
				if !slices.Contains(headings, heading) {
					t.Errorf("README.md is missing the %q command heading", command)
				}
			}
		})
	}
}

func recordToolCommand(commands map[string]bool, expr ast.Expr) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return
	}
	if command, err := strconv.Unquote(literal.Value); err == nil && !strings.HasPrefix(command, "-") {
		commands[command] = true
	}
}
