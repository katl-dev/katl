package scriptstest

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFedoraPackageLockUpdateContract(t *testing.T) {
	repo := repoRoot(t)
	scriptPath := repo + "/scripts/update-fedora-package-lock"
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat Fedora package-lock updater: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("Fedora package-lock updater is not executable")
	}
	script := string(mustReadFile(t, scriptPath))
	for _, want := range []string{
		"scripts/mkosi build-runtime",
		"scripts/mkosi build-installer",
		"scripts/mkosi build-kubernetes-sysext",
		"--package-set installer-image",
		"--package-set runtime",
		"--package-set kubernetes-sysext",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Fedora package-lock updater missing %q", want)
		}
	}
	mkosi := string(mustReadFile(t, repo+"/scripts/mkosi"))
	if !strings.Contains(mkosi, "installer_cache_root=/mkosi-build/cache/fedora~44~x86-64~main.cache") {
		t.Fatal("containerized mkosi build does not record the installer package set inside the builder")
	}

	workflow := string(mustReadFile(t, repo+"/.github/workflows/fedora-package-lock.yml"))
	var document any
	if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
		t.Fatalf("parse Fedora package-lock workflow: %v", err)
	}
	for _, want := range []string{
		"schedule:",
		"workflow_dispatch:",
		"scripts/update-fedora-package-lock",
		"draft: false",
		"createWorkflowDispatch",
		"enablePullRequestAutoMerge",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("Fedora package-lock workflow missing %q", want)
		}
	}

	fastChecks := string(mustReadFile(t, repo+"/.github/workflows/fast-checks.yml"))
	if !strings.Contains(fastChecks, "workflow_dispatch:") || !strings.Contains(fastChecks, `range="origin/main...HEAD"`) {
		t.Fatal("fast checks do not support package-lock branch dispatch")
	}
}
