package scriptstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
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

func TestScriptREADMEUsage(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "scripts")
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"codex-role", "hermetic", "vmtest-run", "vmtest-clean", "worktree-clean"} {
		t.Run(script, func(t *testing.T) {
			_, section, found := strings.Cut(string(readme), "\n## `"+script+"`\n")
			if !found {
				t.Fatal("scripts/README.md is missing the script heading")
			}
			section, _, _ = strings.Cut(section, "\n## ")

			// The scripts without a help flag print usage when called with
			// no operands. In particular, the cleanup scripts treat a single
			// argument as a target path.
			args := []string{}
			if script == "vmtest-run" {
				args = append(args, "--help")
			}
			cmd := exec.CommandContext(t.Context(), filepath.Join(dir, script), args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				if _, ok := err.(*exec.ExitError); !ok || len(args) != 0 {
					t.Fatalf("reading usage: %v\n%s", err, output)
				}
			}

			_, usage, found := strings.Cut(string(output), "usage: scripts/"+script+" ")
			if !found {
				t.Fatalf("no usage in output:\n%s", output)
			}
			if len(args) == 0 {
				synopsis, _, _ := strings.Cut(usage, "\n")
				if !strings.Contains(section, "\nscripts/"+script+" "+synopsis+"\n") {
					t.Errorf("scripts/README.md is missing the usage synopsis %q", synopsis)
				}
				return
			}

			_, options, _ := strings.Cut(usage, "\nOptions:\n")
			options, _, _ = strings.Cut(options, "\n\n")
			if options == "" {
				t.Fatalf("no options in output:\n%s", output)
			}
			for line := range strings.SplitSeq(options, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "-") {
					continue
				}
				names, _, _ := strings.Cut(line, "  ")
				for name := range strings.SplitSeq(names, ", ") {
					name, _, _ = strings.Cut(name, "<")
					documented := "`" + name + "`"
					if strings.HasSuffix(name, "=") {
						documented = "`" + name
					}
					if !strings.Contains(section, documented) {
						t.Errorf("scripts/README.md is missing option %q", name)
					}
				}
			}
		})
	}
}
