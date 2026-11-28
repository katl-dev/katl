package queueadmission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		path         string
		review       bool
		reviewReason string
		mergeAlone   bool
	}{
		{path: ".github/workflows/ci.yml", review: true, reviewReason: "workflows", mergeAlone: true},
		{path: "mkosi-builder.lock", review: true, reviewReason: "boot inputs", mergeAlone: true},
		{path: ".github/scripts/hosted-vmtest-matrix.sh", review: true, reviewReason: "workflows", mergeAlone: true},
		{path: ".github/workflows/release-artifacts.yml", review: true, reviewReason: "workflows"},
		{path: "scripts/vmtest-run", mergeAlone: true},
		{path: "internal/efiboot/entry.go", review: true, reviewReason: "boot"},
		{path: "internal/efiboot/testdata/nested/entry.conf", review: true, reviewReason: "boot"},
		{path: "internal/katlc/transport/tls.go", review: true, reviewReason: "security and trust"},
		{path: "cmd/katl-queue/simulate.go", review: true, reviewReason: "admission"},
		{path: "kernels/fedora-45/x86_64/standard.json", review: true, reviewReason: "boot inputs"},
		{path: "mkosi.profiles/runtime/mkosi.conf", review: true, reviewReason: "boot inputs"},
		{path: "mkosi.profiles/runtimes/mkosi.conf"},
		{path: "internal/katlc/client.go"},
		{path: "internal/efiboot"},
		{path: "cmd/katl-queue/plan.go"},
		{path: "internal/vmtest/x.go"},
		{path: "docs/README.md"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			review := classify(tc.path, classReviewRequired)
			mergeAlone := len(classify(tc.path, classMergeAlone)) > 0
			if (len(review) > 0) != tc.review || mergeAlone != tc.mergeAlone || (tc.review && !slices.Contains(review, tc.reviewReason)) {
				t.Fatalf("review %q, merge-alone %v; want review %v (%q), merge-alone %v", review, mergeAlone, tc.review, tc.reviewReason, tc.mergeAlone)
			}
		})
	}
}

// kernel-automerge.yml admits automation pull requests itself, so its
// allowlist and the table must not drift apart.
func TestAutomationParity(t *testing.T) {
	root := filepath.Join("..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/kernel-automerge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	type branch struct{ prefix, allowed string }
	var fromWorkflow, fromTable []branch
	for _, m := range regexp.MustCompile(`(?m)^\s*(automation/[a-z-]+-)\*\) allowed='([^']+)'`).FindAllStringSubmatch(string(workflow), -1) {
		fromWorkflow = append(fromWorkflow, branch{m[1], m[2]})
	}
	for _, b := range automationBranches {
		fromTable = append(fromTable, branch{b.prefix, b.allowed.String()})
		if !strings.Contains(string(workflow), b.prefix+"**") {
			t.Errorf("workflow trigger does not list %s**", b.prefix)
		}
	}
	if !slices.Equal(fromWorkflow, fromTable) {
		t.Fatalf("workflow allowlist %q, table %q", fromWorkflow, fromTable)
	}

	samples, err := os.ReadFile(filepath.Join(root, "internal/scriptstest/testdata/automation-paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []struct {
		Name, Branch, File string
		Accepted           bool
	}
	if err := json.Unmarshal(samples, &paths); err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no automation path samples")
	}
	for _, p := range paths {
		if got := automationPaths(p.Branch, []string{p.File}); got != p.Accepted {
			t.Errorf("%s: automationPaths(%s, %s) = %v, want %v", p.Name, p.Branch, p.File, got, p.Accepted)
		}
	}
}
