package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// addressOwnerFiles own the conclusions a command draws about a node's
// address and the advice that follows from them; see
// docs/internal/management-address-ownership.md.
var addressOwnerFiles = []string{
	"cmd/katlctl/node_address.go",
	"cmd/katlctl/management_confirmation.go",
}

// addressAdviceFragments are phrases of address and trust advice. A literal
// outside the owner that contains one is a second owner of that advice.
var addressAdviceFragments = []string{
	"refusing to move",
	"restore management.address",
	"check the address",
	"check the endpoint",
	"check its management.address",
	"is the node's management address",
	"did not answer",
	"may still trust",
	"management authority",
	"identity recover",
	"context rebind",
	"already moved",
	"still answers at",
	"CURRENT_ADDRESS",
	"node status' shows it up",
}

// addressOwnerTypes may be built only by the owner.
var addressOwnerTypes = []string{"candidate", "verified", "move", "managementCheck"}

const classifyEvidenceCall = "call transport.ClassifyManagementEvidence"

// addressAllowance excuses count findings of rule in path. Each remaining
// duplicate names the sub-issue of #1107 that removes it; each legitimately
// different site gives its reason. Several entries may share a path and rule;
// their counts add up.
type addressAllowance struct {
	path, rule string
	count      int
	reason     string
}

var addressAllowlist = []addressAllowance{
	// Duplicates, each removed by the sub-issue it names.
	{"cmd/katlctl/enroll.go", "composite managementCheck", 1, "#1112: context rebind asserts addressUnverified instead of deriving the binding"},
	{"cmd/katlctl/management_enable_mtls.go", "composite managementCheck", 3, "#1112: the enable-mtls preflight derives the binding a second time and the trial wait reuses it"},
	{"cmd/katlctl/enroll.go", classifyEvidenceCall, 1, "#1113: addressHintApplies classifies evidence a second time to gate the hints"},
	{"cmd/katlctl/enroll.go", "already moved", 1, "#1113: movedHint"},
	{"cmd/katlctl/enroll.go", "CURRENT_ADDRESS", 1, "#1113: readdressHint"},
	{"cmd/katlctl/enroll.go", "context rebind", 1, "#1113: movedHint"},
	{"cmd/katlctl/kubeadm_control_plane_config.go", "refusing to move", 1, "#1114: the pinned-kubelet worker refusal does not take a move"},
	{"cmd/katlctl/kubeadm_control_plane_config.go", "restore management.address", 1, "#1114: the pinned-kubelet worker refusal does not take a move"},
	{"cmd/katlctl/enroll.go", "context rebind", 1, "#1115: warnPendingReaddress decides a pending move from configuration differences"},
	{"cmd/katlctl/host_management.go", "restore management.address", 1, "#1115: addressChangeRebootError advice"},
	{"cmd/katlctl/host_management.go", "still answers at", 1, "#1115: addressChangeRebootError advice"},
	{"cmd/katlctl/host_management.go", "did not answer", 1, "#1115: addressChangeRebootError advice"},
	{"cmd/katlctl/management_enable_mtls.go", "check its management.address", 1, "#1116: the enable-mtls identity mismatch advice"},
	{"cmd/katlctl/management_rotation.go", "check the endpoint", 1, "#1108: the rotate already-rotated check advises on the endpoint whatever the evidence"},
	{"cmd/katlctl/management_rotation.go", "composite managementCheck", 1, "#1108: the rotate already-rotated check passes a zero address"},
	{"internal/katlc/transport/credentials.go", "identity recover", 1, "#1119: ManagementAuthorityRemedy is transport advice"},
	{"internal/katlc/transport/credentials.go", "management authority", 1, "#1119: the transport's default authority advice"},
	{"internal/katlc/transport/management_wait.go", "did not answer", 1, "#1119: the transport's refused-connection advice"},
	{"internal/katlc/transport/management_wait.go", "check the address", 1, "#1119: the transport's refused-connection advice"},

	// Legitimately different contracts.
	{"cmd/katlctl/management_enable_mtls.go", "composite managementCheck", 7, "a trust-changing step whose outcome is unknown names its own step with a zero address (#1108 makes the step typed)"},
	{"cmd/katlctl/management_recovery.go", "composite managementCheck", 5, "a trust-changing step whose outcome is unknown names its own step with a zero address (#1108 makes the step typed)"},
	{"cmd/katlctl/management_rotation.go", "composite managementCheck", 7, "a trust-changing step whose outcome is unknown names its own step with a zero address (#1108 makes the step typed)"},
	{"cmd/katlctl/enroll.go", classifyEvidenceCall, 1, "contextEndpointStatus classifies only to decide whether a status read succeeded; it gives no advice"},
	{"cmd/katlctl/host_management.go", classifyEvidenceCall, 1, "the shutdown wait classifies only to decide whether to keep waiting"},
	{"cmd/katlctl/management_enable_mtls.go", classifyEvidenceCall, 5, "enable-mtls classifies only to decide whether to continue a trial; it gives no advice"},
	{"cmd/katlctl/management_rotation.go", classifyEvidenceCall, 2, "rotation classifies only to decide whether to continue; it gives no advice"},
	{"cmd/katlctl/enroll.go", "context rebind", 1, "context rebind names itself as the rerun command"},
	{"cmd/katlctl/main.go", "context rebind", 2, "the command's own usage example"},
	{"cmd/katlctl/kubeadm_control_plane_config.go", "still answers at", 1, "the apply report, printed only after this run verified the node at its current address and built the address change from a move"},
	{"cmd/katlctl/management_recovery.go", "identity recover", 1, "the command's own usage example"},
	{"cmd/katlctl/management_recovery.go", "management authority", 6, "the recovery command's help, progress and claims name the authority it reads and replaces over SSH, a channel with its own trust root"},
	{"cmd/katlctl/management_rotation.go", "management authority", 1, "a malformed replacement authority in the command's own input, not a claim about a node"},
	{"cmd/katlctl/management_secrets.go", "identity recover", 1, "missing local credentials are workstation file state, not node evidence"},
	{"internal/katlc/agent/management_trial.go", "management authority", 1, "a node-side log line about the node's own trust store"},
}

func TestAddressAdviceHasOneOwner(t *testing.T) {
	root := repositoryRootForOwnership(t)
	found := map[[2]string]int{}
	sites := map[[2]string][]string{}
	for _, dir := range []string{"cmd/katlctl", "internal/katlc"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if slices.Contains(addressOwnerFiles, rel) {
				return nil
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				return err
			}
			for _, finding := range addressFindings(file, rel == filepath.ToSlash(filepath.Join("cmd/katlctl", filepath.Base(rel)))) {
				key := [2]string{rel, finding.rule}
				found[key]++
				sites[key] = append(sites[key], fmt.Sprintf("%s:%d", rel, fileSet.Position(finding.pos).Line))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var problems []string
	allowed := map[[2]string]int{}
	for _, allowance := range addressAllowlist {
		key := [2]string{allowance.path, allowance.rule}
		allowed[key] += allowance.count
		if allowance.reason == "" || allowance.count <= 0 {
			problems = append(problems, fmt.Sprintf("%s: %q needs a reason and a positive count", allowance.path, allowance.rule))
		}
	}
	for key, count := range allowed {
		switch got := found[key]; {
		case got == 0:
			problems = append(problems, fmt.Sprintf("%s: %q is allowlisted but no longer present; remove the entry", key[0], key[1]))
		case got < count:
			problems = append(problems, fmt.Sprintf("%s: %q has %d findings, fewer than the %d allowlisted; lower the count", key[0], key[1], got, count))
		case got > count:
			problems = append(problems, fmt.Sprintf("%s: %q has %d findings, more than the %d allowlisted: %s", key[0], key[1], got, count, strings.Join(sites[key], ", ")))
		}
	}
	for key, count := range found {
		if _, ok := allowed[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %q appears %d times outside the address owner: %s", key[0], key[1], count, strings.Join(sites[key], ", ")))
		}
	}
	slices.Sort(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
	if len(problems) > 0 {
		t.Log("decide where a node is, whether it moved and what to do next through checkAddress and verified.move in cmd/katlctl/node_address.go")
	}
}

type addressFinding struct {
	pos  token.Pos
	rule string
}

// addressFindings reports advice literals, owner composite literals (in
// package main only) and evidence classification calls in file.
func addressFindings(file *ast.File, ownerPackage bool) []addressFinding {
	var findings []addressFinding
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(node.Value)
			if err != nil {
				value = node.Value
			}
			for _, fragment := range addressAdviceFragments {
				if strings.Contains(value, fragment) {
					findings = append(findings, addressFinding{node.Pos(), fragment})
				}
			}
		case *ast.CompositeLit:
			if ident, ok := node.Type.(*ast.Ident); ok && ownerPackage && slices.Contains(addressOwnerTypes, ident.Name) {
				findings = append(findings, addressFinding{node.Pos(), "composite " + ident.Name})
			}
		case *ast.CallExpr:
			if selector, ok := node.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "ClassifyManagementEvidence" {
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "transport" {
					findings = append(findings, addressFinding{node.Pos(), classifyEvidenceCall})
				}
			}
		}
		return true
	})
	return findings
}

func repositoryRootForOwnership(t *testing.T) string {
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
