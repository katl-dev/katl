package scriptstest

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublishedSupportBoundaryContract(t *testing.T) {
	repo := repoRoot(t)
	support := string(mustReadFile(t, repo+"/docs/support.md"))
	for _, value := range []string{
		"experimental alpha software",
		"production clusters",
		"x86-64 machines booted with UEFI",
		"libvirt",
		"KVM",
		"OVMF",
		"exact VM and physical-hardware paths named",
		"SHA-256 checksums",
		"keyless GitHub",
		"provenance attestations",
		"UEFI Secure Boot signatures",
		"All `v1alpha1` source",
		"persisted-state formats",
		"Reinstall may be required",
		"do not roll back etcd",
		"Kubernetes version upgrade execution",
		"support SLA",
		"Katl GitHub issue",
		"private vulnerability",
		"Remove tokens, private keys, kubeconfigs",
	} {
		if !strings.Contains(support, value) {
			t.Fatalf("support boundary missing %q", value)
		}
	}

	issueForm := string(mustReadFile(t, repo+"/.github/ISSUE_TEMPLATE/bug.yml"))
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(issueForm), &parsed); err != nil {
		t.Fatalf("parse bug report form: %v", err)
	}
	for _, value := range []string{
		"id: version",
		"id: artifacts",
		"id: environment",
		"id: reproduce",
		"id: expected",
		"id: evidence",
		"private vulnerability reporting",
		"I removed credentials",
		"required: true",
	} {
		if !strings.Contains(issueForm, value) {
			t.Fatalf("bug report form missing %q", value)
		}
	}
}
