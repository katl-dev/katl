package generation

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// bootTargetWriters name the writes of boot state: the published selection,
// the move intent, generation status and the EFI boot target.
var bootTargetWriters = []string{
	"WriteBootSelection", "writeBootMoveIntent", "removeBootMoveIntent", "WriteGenerationStatus", "WriteGeneration",
}

// bootTargetLiterals name boot state that code could write directly.
var bootTargetLiterals = []string{
	"set-default", "set-oneshot", "LoaderEntryDefault", "LoaderEntryOneShot", "selection.json", "move-intent.json", "loader/entries",
}

// bootTargetViolations reports each place a file writes boot state or names
// it in a string, as "position: function: finding".
func bootTargetViolations(fset *token.FileSet, file *ast.File) []string {
	var violations []string
	report := func(node ast.Node, function, finding string) {
		violations = append(violations, fmt.Sprintf("%s: %s: %s", fset.Position(node.Pos()), function, finding))
	}
	for _, decl := range file.Decls {
		function := "package scope"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			function = fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				function = receiverName(fn.Recv.List[0].Type) + "." + function
			}
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				switch callee := node.Fun.(type) {
				case *ast.Ident:
					if slices.Contains(bootTargetWriters, callee.Name) {
						report(node, function, "calls "+callee.Name)
					}
				case *ast.SelectorExpr:
					pkg, _ := callee.X.(*ast.Ident)
					switch {
					case slices.Contains(bootTargetWriters, callee.Sel.Name):
						report(node, function, "calls "+callee.Sel.Name)
					case pkg != nil && pkg.Name == "efiboot" && (callee.Sel.Name == "SetDefault" || callee.Sel.Name == "SetOneShot"):
						report(node, function, "calls efiboot."+callee.Sel.Name)
					}
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					break
				}
				value, err := strconv.Unquote(node.Value)
				if err != nil {
					break
				}
				for _, name := range bootTargetLiterals {
					if strings.Contains(value, name) {
						report(node, function, "literal names "+name)
					}
				}
			}
			return true
		})
	}
	return violations
}

func receiverName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.StarExpr:
		return receiverName(expr.X)
	case *ast.IndexExpr:
		return receiverName(expr.X)
	case *ast.Ident:
		return expr.Name
	}
	return "?"
}

func TestBootTargetViolationsDetection(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		want         int
	}{
		{name: "selection write", source: `package p; func f() { _ = generation.WriteBootSelection(root, selection) }`, want: 1},
		{name: "bootctl one-shot", source: `package p; func f() { exec.Command("bootctl", "set-oneshot", entry) }`, want: 1},
		{name: "selection path", source: `package p; func f() { os.WriteFile(filepath.Join(root, "var/lib/katl/boot/selection.json"), data, 0o644) }`, want: 1},
		{name: "loader entry removal", source: `package p; func f() { os.Remove(filepath.Join(root, "efi/loader/entries", name)) }`, want: 1},
		{name: "efivar path", source: "package p; var path = `/sys/firmware/efi/efivars/LoaderEntryOneShot-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f`", want: 1},
		{name: "status write", source: `package p; func f() { generation.WriteGenerationStatus(root, spec, status) }`, want: 1},
		{name: "efi default", source: `package p; func f() { efiboot.SetDefault(root, entry) }`, want: 1},
		{name: "selection read", source: `package p; func f() { generation.ReadBootSelection(root) }`},
		{name: "bootctl list", source: `package p; func f() { exec.Command("bootctl", "list") }`},
		{name: "comment", source: "package p\n// selection.json is published by the owner.\nvar x = 1"},
		{name: "other setter", source: `package p; func f() { flags.SetDefault(name) }`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", tt.source, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			if got := bootTargetViolations(fset, file); len(got) != tt.want {
				t.Fatalf("violations = %v, want %d", got, tt.want)
			}
		})
	}
}

type bootTargetAllowance struct{ path, fragment, reason string }

// bootTargetAllowlist excuses boot-state findings outside the owner. Each
// entry excuses exactly one finding; repeated entries excuse repeated findings.
var bootTargetAllowlist = []bootTargetAllowance{
	{"internal/generation/boot_move.go", "package scope: literal names move-intent.json", "the intent path constant the owner and its reader share"},
	{"internal/generation/boot_selection.go", "BootSelectionPath: literal names selection.json", "the selection path the owner writes and every reader reads"},
	{"internal/generation/initialize.go", "Initialize: calls WriteGeneration", "creates generation 0 under the boot-state lock, refusing a root that already has an intent or another initial selection; the owner then publishes the first selection"},
	{"internal/generation/management.go", "removeEntries: literal names loader/entries", "checks the shape of an entry path before removal"},
	{"internal/generation/management.go", "removeEntries: literal names loader/entries", "removes a generation's loader entry only after the owner's move that stopped naming it resolved"},
	{"internal/generation/metadata.go", "NewRuntimeConfigRecord: literal names loader/entries", "derives a new generation's entry path; writes nothing"},
	{"internal/generation/upgrade_handoff.go", "validateUpgradeHandoff: literal names loader/entries", "validates a handoff's entry path; writes nothing"},
	{"internal/installer/bootstrapruntime/prepare.go", "Prepare: literal names loader/entries", "derives the bootstrap generation's entry path; writes nothing"},
	{"internal/installer/bootstrapruntime/prepare.go", "Prepare: calls WriteGeneration", "creates the bootstrap generation before any boot target names it"},
	{"internal/installer/configapply/runtime_apply.go", "ApplyTrustedBundle: calls WriteGeneration", "creates a configuration candidate that no boot target names until the owner arms or promotes it"},
	{"internal/katlc/agent/destructive_reset_executor.go", "package scope: literal names loader/entries", "destructive reset wipes every Katl boot artifact; no selection survives it"},
	{"internal/katlc/agent/generation_apply_recovery.go", "abandonCandidateGeneration: calls WriteGenerationStatus", "abandons an interrupted candidate that never left commit state candidate, so no boot target names it"},
	{"internal/katlc/agent/host_upgrade_executor.go", "Executor.executeHostUpgrade: calls WriteGeneration", "creates the host upgrade candidate before the owner arms its trial"},
	{"internal/katlc/agent/host_upgrade_handoff.go", "Executor.executeHostUpgradeHandoff: literal names loader/entries", "derives the prepared candidate's entry path; writes nothing"},
	{"internal/katlc/agent/host_upgrade_namespace.go", "snapshotUpgradeSource: literal names selection.json", "copies the selection read-only into the private preparation root"},
	{"internal/katlc/agent/host_upgrade_plan.go", "Executor.planHostUpgradeWithHandoff: literal names loader/entries", "derives the candidate's entry path; writes nothing"},
	{"internal/katlc/agent/host_upgrade_prepare.go", "commitPreparedHostUpgrade: calls WriteGeneration", "writes the prepared candidate inside the private preparation root, not the node's boot state"},
	{"internal/katlc/agent/host_upgrade_prepare.go", "commitPreparedHostUpgrade: calls WriteGenerationStatus", "commits the prepared candidate inside the private preparation root; the handoff then arms it through the owner"},
	{"internal/katlc/agent/host_upgrade_preview.go", "Server.previewHostUpgrade: literal names loader/entries", "derives the previewed entry path; writes nothing"},
	{"internal/katlc/agent/kubeadm_upgrade_executor.go", "Executor.stageKubernetesCandidate: literal names loader/entries", "derives the Kubernetes candidate's entry path; writes nothing"},
	{"internal/katlc/agent/kubeadm_upgrade_executor.go", "Executor.stageKubernetesCandidate: calls WriteGeneration", "creates the Kubernetes candidate before the owner promotes it"},
	{"internal/vmtest/first_install.go", "checkExtractedESPArtifacts: literal names loader/entries", "VM test reader of the installed ESP"},
	{"internal/vmtest/first_install_health.go", "checkFirstInstallHealth: literal names selection.json", "VM test reader of the published selection"},
	{"internal/vmtest/installed.go", "CheckESP: literal names loader/entries", "VM test reader of the installed ESP"},
}

// The boot-state owner, BootMover in boot_mover.go, is the only writer of the
// published selection, the move intent, the EFI boot target and the status of
// a generation a boot target names. Everything else reads, or is allowlisted
// with a reason.
func TestBootTargetHasOneOwner(t *testing.T) {
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
	owners := []string{"internal/generation/boot_mover.go", "internal/efiboot"}

	used := make([]int, len(bootTargetAllowlist))
	var problems []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "_build" || name == "vendor" || name == "testdata") || slices.Contains(owners, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || slices.Contains(owners, rel) {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, source, 0)
		if err != nil {
			return err
		}
		for _, violation := range bootTargetViolations(fset, file) {
			matches := func(allowed bootTargetAllowance) bool {
				return allowed.path == rel && strings.Contains(violation, allowed.fragment)
			}
			index := -1
			for i, allowed := range bootTargetAllowlist {
				if matches(allowed) && (index < 0 || used[index] > 0 && used[i] == 0) {
					index = i
				}
			}
			if index < 0 {
				problems = append(problems, violation)
				continue
			}
			used[index]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, allowed := range bootTargetAllowlist {
		switch {
		case used[index] == 0:
			problems = append(problems, fmt.Sprintf("%s: %q is allowlisted but no longer present; remove the entry", allowed.path, allowed.fragment))
		case used[index] > 1:
			problems = append(problems, fmt.Sprintf("%s: %q allowlists %d findings; it may excuse only one", allowed.path, allowed.fragment, used[index]))
		}
	}
	if len(problems) > 0 {
		t.Log("change boot state through generation.BootMover; allowlist a finding only when it does not write a boot target")
		for _, problem := range problems {
			t.Error(problem)
		}
	}
}
