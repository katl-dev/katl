package generation

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// referenceViolations reports each reference to one of names outside its own
// declaration, as "position: function: uses name", and each declaration of a
// function named in declared.
func referenceViolations(names, declared []string) func(*token.FileSet, *ast.File) []string {
	matches := func(name string, set []string) bool {
		for _, candidate := range set {
			if candidate == name {
				return true
			}
		}
		return false
	}
	return func(fset *token.FileSet, file *ast.File) []string {
		var violations []string
		for _, decl := range file.Decls {
			function := "package scope"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				function = fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) == 1 {
					function = receiverName(fn.Recv.List[0].Type) + "." + function
				}
				if matches(fn.Name.Name, declared) {
					violations = append(violations, fmt.Sprintf("%s: declares %s", fset.Position(fn.Pos()), fn.Name.Name))
				}
			}
			declaring := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.FuncDecl:
					declaring[node.Name] = true
				case *ast.ValueSpec:
					for _, name := range node.Names {
						declaring[name] = true
					}
				case *ast.SelectorExpr:
					declaring[node.Sel] = true
					if matches(node.Sel.Name, names) {
						violations = append(violations, fmt.Sprintf("%s: %s: uses %s", fset.Position(node.Pos()), function, node.Sel.Name))
					}
					if pkg, ok := node.X.(*ast.Ident); ok && matches(pkg.Name+".", names) {
						violations = append(violations, fmt.Sprintf("%s: %s: uses %s.%s", fset.Position(node.Pos()), function, pkg.Name, node.Sel.Name))
					}
				case *ast.Ident:
					if !declaring[node] && matches(node.Name, names) {
						violations = append(violations, fmt.Sprintf("%s: %s: uses %s", fset.Position(node.Pos()), function, node.Name))
					}
				}
				return true
			})
		}
		return violations
	}
}

// inDirs limits violations to files under the named repository directories.
func inDirs(dirs []string, violations func(*token.FileSet, *ast.File) []string) func(*token.FileSet, *ast.File) []string {
	return func(fset *token.FileSet, file *ast.File) []string {
		name := fset.Position(file.Pos()).Filename
		for _, dir := range dirs {
			if strings.HasPrefix(name, dir+"/") {
				return violations(fset, file)
			}
		}
		return nil
	}
}

func TestBootSelectionWriteHasOneOwner(t *testing.T) {
	allowlist := []bootTargetAllowance{
		{"internal/generation/boot_mover.go", "publishSelection: uses WriteBootSelection", "chooses the form every bootable generation decodes"},
	}
	for _, problem := range ownershipProblems(t, nil, referenceViolations([]string{"WriteBootSelection"}, nil), allowlist) {
		t.Error(problem)
	}
}

func TestBootMoveReaderRuleHasOneOwner(t *testing.T) {
	allowlist := []bootTargetAllowance{
		{"internal/generation/boot_mover.go", "legacyReaders: uses ReadsBootMoves", "the node chooses the selection form"},
		{"cmd/katlctl/main.go", "checkBootMoveSource: uses ReadsBootMoves", "katlctl refuses a direct upgrade from a release before the first reader"},
		{"internal/generation/boot_mover.go", "ReadsBootMoves: uses FirstBootMoveReader", "the reader rule"},
		{"cmd/katlctl/main.go", "checkBootMoveSource: uses FirstBootMoveReader", "names the intermediate release"},
	}
	names := []string{"ReadsBootMoves", "FirstBootMoveReader"}
	for _, problem := range ownershipProblems(t, nil, referenceViolations(names, []string{"checkReaders"}), allowlist) {
		t.Error(problem)
	}
}

// The node compares releases only in the reader rule.
func TestNodeReleaseComparisonHasOneOwner(t *testing.T) {
	allowlist := []bootTargetAllowance{
		{"internal/generation/boot_mover.go", "package scope: uses releaseversion.Version", "declares the first reader release"},
		{"internal/generation/boot_mover.go", "ReadsBootMoves: uses releaseversion.Parse", "the reader rule"},
		{"internal/generation/boot_mover.go", "ReadsBootMoves: uses releaseversion.Dev", "the reader rule"},
	}
	violations := inDirs([]string{"internal/generation", "internal/katlc/agent"}, referenceViolations([]string{"releaseversion."}, nil))
	for _, problem := range ownershipProblems(t, nil, violations, allowlist) {
		t.Error(problem)
	}
}
