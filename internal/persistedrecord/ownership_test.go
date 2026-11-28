package persistedrecord

import (
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

// bootTreeMarkers name the trees whose writes must be durable before any
// reference to them is published: the ESP (loader entries and UKIs), the boot
// state in /var/lib/katl/boot and the generation records.
var bootTreeMarkers = []string{
	"loader/entries",
	"EFI/katl",
	"EFI/Linux",
	"var/lib/katl/boot",
	"var/lib/katl/generations",
	"UKIDirectory",
	"GenerationDir",
	"GenerationRecordsDir",
	"BootSelectionPath",
	"bootStateDir",
}

// directBootTreeWriters write into a boot tree without the owner and are
// safe for the stated reason.
var directBootTreeWriters = map[string]string{
	"internal/generation/lock.go:withStateLock": "creates an empty lock file; no reader depends on its content",
	"internal/generation/state.go:writeFile":    "writes systemd units into a runtime or install root, not a boot tree",
	// Generation content: WriteGeneration syncs the generation directory
	// before it publishes the spec that makes the generation visible.
	"internal/installer/bootstrapruntime/prepare.go:materializeSysext":              "generation content made durable by WriteGeneration",
	"internal/installer/bootstrapruntime/prepare.go:rehomePreservedBundledConfexts": "generation content made durable by WriteGeneration",
	"internal/installer/bootstrapruntime/prepare.go:rehomePreservedSysexts":         "generation content made durable by WriteGeneration",
	"internal/katlc/agent/host_upgrade_namespace.go:snapshotUpgradeSource":          "private prepare view; publishPreparedUpgrade syncs the tree before renaming it into place",
	// These name a generation only to read it and write elsewhere.
	"internal/installer/configapply/module_indexes.go:PrepareModuleIndexes":  "writes build inputs into a work directory",
	"internal/katlc/agent/host_upgrade_result.go:writePreparedUpgradeResult": "writes under /var/lib/katl/upgrade-handoffs",
}

// directWritePackages own boot-tree records outright, so every direct write
// in them is refused unless listed in directBootTreeWriters.
var directWritePackages = []string{"internal/generation"}

// privateSyncs make their own durability sequence instead of using this
// package.
var privateSyncs = map[string]string{
	"internal/installer/disk/root_slot_writer.go:WriteRootSlot":                     "flushes a block device, not a file",
	"internal/katlc/agent/host_upgrade_executor.go:copyUpgradeComponent":            "consolidate in 2026.11.9, see #1072: stages sysupdate sources in a work directory that is removed afterwards",
	"internal/katlc/agent/host_upgrade_artifact.go:Server.StageHostUpgradeArtifact": "consolidate in 2026.11.9, see #1072: streams and hashes chunks into a temp file and ignores directory sync errors",
	"internal/katlc/agent/api_proxy_access.go:replaceFile":                          "consolidate in 2026.11.9, see #1072: tolerates EINVAL from the directory sync",
	"internal/installer/operation/store.go:writeFileAtomic":                         "consolidate in 2026.11.9, see #1072: creates parents 0700 and syncs each new parent",
	"internal/installer/operation/store.go:writeFileNoReplace":                      "consolidate in 2026.11.9, see #1072: publishes with link(2) after a synced temp file",
	"internal/installer/operation/store.go:fsyncDir":                                "consolidate in 2026.11.9, see #1072",
	"internal/sshhostkey/hostkey.go:syncFile":                                       "consolidate in 2026.11.9, see #1072",
	"internal/sshhostkey/hostkey.go:writePublicKey":                                 "consolidate in 2026.11.9, see #1072",
	"internal/sshhostkey/hostkey.go:syncDirectory":                                  "consolidate in 2026.11.9, see #1072",
	"internal/installer/configbundle/bundle.go:writeArchiveFile":                    "consolidate in 2026.11.9, see #1072: extracts an archive member",
	"internal/kubernetesidentity/identity.go:Write":                                 "consolidate in 2026.11.9, see #1072",
	"internal/kubernetesidentity/install.go:writeAtomic":                            "consolidate in 2026.11.9, see #1072",
	"internal/kubernetesidentity/install.go:syncDirectory":                          "consolidate in 2026.11.9, see #1072",
	"internal/managementidentity/identity.go:Write":                                 "consolidate in 2026.11.9, see #1072",
	"internal/managementidentity/identity.go:SaveExisting":                          "consolidate in 2026.11.9, see #1072",
	"internal/nodeidentity/identity.go:writeEnrollmentAtomic":                       "consolidate in 2026.11.9, see #1072",
	"internal/nodeidentity/rotation.go:stageManagementVersion":                      "consolidate in 2026.11.9, see #1072: stages a versioned directory",
	"internal/nodeidentity/rotation.go:syncManagementDirectory":                     "consolidate in 2026.11.9, see #1072",
	"cmd/katlctl/etcd_snapshot.go:receiveEtcdSnapshot":                              "consolidate in 2026.11.9, see #1072: streams a snapshot on the workstation",
	"cmd/katlctl/management_rotation.go:writeEncryptedManagementIdentity":           "consolidate in 2026.11.9, see #1072: workstation file",
}

// TestDurableWriteOwnership keeps one durable-write owner: no code outside
// this package fsyncs on its own, and no code writes into a boot tree
// directly.
func TestDurableWriteOwnership(t *testing.T) {
	root := repositoryRoot(t)
	var problems []string
	seenSync := map[string]bool{}
	seenDirect := map[string]bool{}
	for _, path := range goSources(t, root) {
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if strings.HasPrefix(rel, "internal/persistedrecord/") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		strictPackage := slices.Contains(directWritePackages, filepath.ToSlash(filepath.Dir(rel)))
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			key := rel + ":" + functionName(function)
			syncs, writes, marked := inspectFunction(function)
			if syncs {
				if _, ok := privateSyncs[key]; ok {
					seenSync[key] = true
				} else {
					problems = append(problems, key+" fsyncs on its own; use persistedrecord")
				}
			}
			if writes && (strictPackage || marked) {
				if _, ok := directBootTreeWriters[key]; ok {
					seenDirect[key] = true
				} else {
					problems = append(problems, key+" writes a boot tree directly; use persistedrecord")
				}
			}
		}
	}
	for key := range privateSyncs {
		if !seenSync[key] {
			problems = append(problems, key+" is allowlisted as a private fsync but no longer is one; remove the entry")
		}
	}
	for key := range directBootTreeWriters {
		if !seenDirect[key] {
			problems = append(problems, key+" is allowlisted as a direct boot-tree writer but no longer is one; remove the entry")
		}
	}
	slices.Sort(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
}

// inspectFunction reports whether a function calls Sync, writes a file with
// the os package directly, and mentions a boot tree.
func inspectFunction(function *ast.FuncDecl) (syncs, writes, marked bool) {
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BasicLit:
			if node.Kind == token.STRING {
				value, err := strconv.Unquote(node.Value)
				if err == nil && containsMarker(value) {
					marked = true
				}
			}
		case *ast.Ident:
			if slices.Contains(bootTreeMarkers, node.Name) {
				marked = true
			}
		case *ast.CallExpr:
			selector, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "Sync" && len(node.Args) == 0 {
				syncs = true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "os" && directWrite(selector.Sel.Name, node.Args) {
				writes = true
			}
		}
		return true
	})
	return syncs, writes, marked
}

func directWrite(name string, args []ast.Expr) bool {
	switch name {
	case "WriteFile", "Create", "CreateTemp":
		return true
	case "OpenFile":
		if len(args) < 2 {
			return true
		}
		flags := exprText(args[1])
		for _, flag := range []string{"O_WRONLY", "O_RDWR", "O_CREATE", "O_TRUNC", "O_APPEND"} {
			if strings.Contains(flags, flag) {
				return true
			}
		}
	}
	return false
}

func exprText(expr ast.Expr) string {
	var names []string
	ast.Inspect(expr, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			names = append(names, ident.Name)
		}
		return true
	})
	return strings.Join(names, " ")
}

func containsMarker(value string) bool {
	for _, marker := range bootTreeMarkers {
		if strings.Contains(marker, "/") && strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func functionName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	receiver := function.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name + "." + function.Name.Name
	}
	return function.Name.Name
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func goSources(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && (entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return paths
}
