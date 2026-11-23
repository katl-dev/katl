package inventory

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// endpointOwners hold the node endpoint ports and derivations. Everything
// else asks them, so a second derivation cannot drift from the first.
var endpointOwners = []string{
	// The workstation-to-node and node-to-node endpoint owner.
	"internal/bootstrap/inventory",
	// The API proxy defines its listener and backend ports.
	"internal/apiproxy",
	// The control-plane endpoint's default port is part of its schema.
	"internal/installer/controlplaneendpoint",
}

// endpointAllowance is a literal outside the owners that is not a derivation
// of a node endpoint: a finding in path whose text contains fragment.
type endpointAllowance struct {
	path, fragment string
}

// endpointLiteralAllowlist lists the literals that genuinely need a port, each
// with the reason it is not a node endpoint derivation.
var endpointLiteralAllowlist = []endpointAllowance{
	// The agent's own listen default. ManagementPort is the workstation's view
	// of the same port; the agent sits below inventory and cannot import it.
	{"internal/katlc/agent/server.go", `"tcp://0.0.0.0:9443"`},
	// The generated agent unit passes that listen address explicitly, and the
	// unit text is a versioned generation artifact.
	{"internal/generation/state.go", "--listen tcp://0.0.0.0:9443"},
	// Node-side loopback: the agent and the console reach the node's own API
	// proxy or kube-apiserver, not another node.
	{"internal/katlc/agent/api_proxy_access.go", `"https://127.0.0.1:7445"`},
	{"internal/operatorconsole/kubernetes.go", `"https://127.0.0.1:6443"`},
	// The agent rebuilds the local kube-apiserver endpoint from its own static
	// pod's --advertise-address, which is not an inventory address.
	{"internal/katlc/agent/kubeadm_upgrade_executor.go", "net.JoinHostPort(e.Address, ...)"},
	// Help, error and template text that shows an operator the default.
	{"cmd/katlc/main.go", "such as tcp://0.0.0.0:9443"},
	{"cmd/katlctl/install.go", "check the installed system (port 9443)"},
	{"cmd/katlctl/diagnostics.go", "for example https://192.0.2.10:6443"},
	{"cmd/katlctl/config_init.go", "# port: 6443"},
	{"internal/externalworker/config.go", "such as https://api.lab.example:6443"},
	// A VM test world's ClusterConfig fixture spells the port as an operator
	// would.
	{"internal/vmtest/first_install_world.go", "6443"},
}

// endpointFieldAllowlist names string fields in this package whose name says
// address or endpoint but which are not node addresses.
var endpointFieldAllowlist = map[string]string{
	// An operator-supplied kubectl server, host:port or https://host:port,
	// passed to kubectl unchanged.
	"Bootstrap.StableEndpoint": "kubectl server",
}

var (
	endpointPortPattern = regexp.MustCompile(`(^|[^0-9])(9443|6443|7445|2380)([^0-9]|$)`)
	endpointFieldSuffix = regexp.MustCompile(`(Address|Endpoint|CIDR|Prefix)$`)
)

func TestEndpointsHaveOneOwner(t *testing.T) {
	root := repositoryRoot(t)
	var problems []string
	used := map[int]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "_build" || entry.Name() == "vendor" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if slices.ContainsFunc(endpointOwners, func(owner string) bool { return filepath.ToSlash(filepath.Dir(rel)) == owner }) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, finding := range endpointFindings(file) {
			index := slices.IndexFunc(endpointLiteralAllowlist, func(allowed endpointAllowance) bool {
				return allowed.path == rel && strings.Contains(finding.literal, allowed.fragment)
			})
			if index >= 0 {
				used[index] = true
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: %s: %s", rel, finding.literal, finding.reason))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, allowed := range endpointLiteralAllowlist {
		if !used[index] {
			problems = append(problems, fmt.Sprintf("%s: %q is allowlisted but no longer present; remove the entry", allowed.path, allowed.fragment))
		}
	}
	slices.Sort(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
	if len(problems) > 0 {
		t.Log("derive node endpoints with inventory.Node/PlannedNode/Plan endpoint methods; allowlist a literal only when it is not a node endpoint")
	}
}

type endpointFinding struct {
	literal string
	reason  string
}

// endpointFindings reports port literals and JoinHostPort calls that build an
// endpoint from a node's management address.
func endpointFindings(file *ast.File) []endpointFinding {
	var findings []endpointFinding
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BasicLit:
			if (node.Kind == token.STRING || node.Kind == token.INT) && endpointPortPattern.MatchString(node.Value) {
				findings = append(findings, endpointFinding{literal: node.Value, reason: "names a node endpoint port"})
			}
		case *ast.CallExpr:
			selector, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "JoinHostPort" || len(node.Args) == 0 {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "net" {
				return true
			}
			ast.Inspect(node.Args[0], func(arg ast.Node) bool {
				if field, ok := arg.(*ast.SelectorExpr); ok && field.Sel.Name == "Address" {
					findings = append(findings, endpointFinding{literal: "net.JoinHostPort(" + exprText(field) + ", ...)", reason: "joins a port onto a node address"})
					return false
				}
				return true
			})
		}
		return true
	})
	return findings
}

func exprText(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		return exprText(expr.X) + "." + expr.Sel.Name
	case *ast.CallExpr:
		return exprText(expr.Fun) + "()"
	case *ast.IndexExpr:
		return exprText(expr.X) + "[]"
	default:
		return "?"
	}
}

// TestInventoryAddressesAreTyped keeps node addresses and endpoints in this
// package typed, so they are parsed once where they enter.
func TestInventoryAddressesAreTyped(t *testing.T) {
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				spec, ok := node.(*ast.TypeSpec)
				if !ok {
					return true
				}
				structure, ok := spec.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, field := range structure.Fields.List {
					if ident, ok := field.Type.(*ast.Ident); !ok || ident.Name != "string" {
						continue
					}
					for _, name := range field.Names {
						if !endpointFieldSuffix.MatchString(name.Name) {
							continue
						}
						key := spec.Name.Name + "." + name.Name
						if _, ok := endpointFieldAllowlist[key]; ok {
							used[key] = true
							continue
						}
						t.Errorf("%s: %s is a string; use netip.Addr, netip.Prefix, Host, HostPort or ManagementAddress", fset.Position(name.Pos()), key)
					}
				}
				return true
			})
		}
	}
	for key := range endpointFieldAllowlist {
		if !used[key] {
			t.Errorf("%s is allowlisted but no longer a string field; remove the entry", key)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
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
