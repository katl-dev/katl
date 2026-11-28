package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCICostRepository(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checkout bool
	}{
		{name: "outside a checkout"},
		{name: "another checkout", checkout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.checkout {
				repo := fixtureRepo{t: t, dir: dir}
				repo.git("init", "--quiet")
				repo.git("remote", "add", "origin", "https://github.com/elsewhere/other.git")
			}

			fixture := filepath.Join(t.TempDir(), "runs.json")
			if err := os.WriteFile(fixture, []byte(`[{"workflow_runs":[]}]`), 0o600); err != nil {
				t.Fatal(err)
			}
			const endpoint = "repos/katl-dev/katl/actions/workflows/ci.yml/runs?"
			log := fakeGH(t, map[string]string{endpoint + "*": fixture}, "", 0, "")
			t.Setenv("GH_CONFIG_DIR", t.TempDir())
			t.Setenv("GH_REPO", "elsewhere/other")
			t.Chdir(dir)

			err := run(t.Context(), []string{"ci-cost", "--workflow", "ci.yml"}, io.Discard, io.Discard)
			raw, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatalf("read gh calls: %v (ci-cost: %v)", readErr, err)
			}
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(calls) != 1 || !strings.HasPrefix(calls[0], "api --paginate --slurp "+endpoint) ||
				!strings.HasSuffix(calls[0], " --hostname github.com") {
				t.Errorf("gh calls = %q, want one GitHub request for %s (ci-cost: %v)", calls, endpoint, err)
			}
			if err != nil {
				t.Fatalf("ci-cost: %v", err)
			}
		})
	}
}

func TestQueueObjectsRepository(t *testing.T) {
	upstream := fixtureRepo{t: t, dir: t.TempDir()}
	upstream.git("init", "--quiet")
	upstream.git("commit", "--quiet", "--allow-empty", "-m", "upstream")
	base := upstream.git("rev-parse", "HEAD")

	local := fixtureRepo{t: t, dir: t.TempDir()}
	local.git("init", "--quiet")
	local.git("remote", "add", "origin", filepath.Join(local.dir, "missing-origin"))
	local.git("config", "url."+upstream.dir+".insteadOf", "https://github.com/katl-dev/katl.git")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Chdir(local.dir)

	objects, err := newGH().queueObjects(t.Context(), queueSnapshot{BaseOid: base})
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	if _, err := objects.git(t.Context(), "cat-file", "-e", base+"^{commit}"); err != nil {
		t.Fatalf("Katl commit unavailable for simulation: %v", err)
	}
}

func TestGHRepositoryOwnership(t *testing.T) {
	files := token.NewFileSet()
	packages, err := parser.ParseDir(files, ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				constructor := ok && fn.Recv == nil && fn.Name.Name == "newGH"
				for node := range ast.Preorder(decl) {
					switch node := node.(type) {
					case *ast.CompositeLit:
						if name, ok := node.Type.(*ast.Ident); ok && name.Name == "gh" && !constructor {
							t.Errorf("%s: construct the GitHub client with newGH", files.Position(node.Pos()))
						}
						checkRepoView(t, files, node.Elts)
					case *ast.CallExpr:
						if name, ok := node.Fun.(*ast.Ident); ok && name.Name == "new" && len(node.Args) == 1 && !constructor {
							if typ, ok := node.Args[0].(*ast.Ident); ok && typ.Name == "gh" {
								t.Errorf("%s: construct the GitHub client with newGH", files.Position(node.Pos()))
							}
						}
						checkRepoView(t, files, node.Args)
					}
				}
			}
		}
	}
}

func checkRepoView(t *testing.T, files *token.FileSet, args []ast.Expr) {
	t.Helper()
	previous := ""
	for _, arg := range args {
		value := ""
		if literal, ok := arg.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value, _ = strconv.Unquote(literal.Value)
		}
		if previous == "repo" && value == "view" {
			t.Errorf("%s: repository discovery through gh repo view", files.Position(arg.Pos()))
		}
		previous = value
	}
}
