package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKubernetesReleaseAutomation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/kubernetes-bundles.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			Name     string `yaml:"name"`
			Needs    any    `yaml:"needs"`
			Strategy struct {
				FailFast bool `yaml:"fail-fast"`
			} `yaml:"strategy"`
			Steps []struct{ Name, Run, If string } `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if _, ok := workflow.On["schedule"]; !ok {
		t.Fatal("release discovery is not scheduled")
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("producer has no presubmit")
	}
	if _, ok := workflow.Jobs["compatibility"]; ok {
		t.Fatal("publication still depends on a shared catalog update")
	}
	build := workflow.Jobs["build"]
	if build.Strategy.FailFast {
		t.Fatal("one release failure cancels other releases")
	}
	stages := map[string]int{}
	for i, step := range build.Steps {
		stages[step.Name] = i
		if step.Name == "Publish immutable OCI bundle" || step.Name == "Promote compatible bundle" {
			if !strings.Contains(step.If, "env.PUBLISH == 'true'") {
				t.Fatalf("%s can run in presubmit", step.Name)
			}
		}
	}
	for _, pair := range [][2]string{
		{"Resume existing candidate", "Build compatible runtime and Kubernetes sysext"},
		{"Verify built runtime and Kubernetes sysext", "Publish immutable OCI bundle"},
		{"Attest published OCI manifest", "Promote compatible bundle"},
		{"Verify public bundle and provenance", "Promote compatible bundle"},
	} {
		before, haveBefore := stages[pair[0]]
		after, haveAfter := stages[pair[1]]
		if !haveBefore || !haveAfter || before >= after {
			t.Fatalf("%s must precede %s", pair[0], pair[1])
		}
	}
	if workflow.Jobs["presubmit"].Name != "Kubernetes Bundle Presubmit" {
		t.Fatal("stable required-check name changed")
	}
	for _, dependency := range []string{"plan", "build"} {
		if !hasWorkflowNeed(workflow.Jobs["presubmit"].Needs, dependency) {
			t.Fatalf("presubmit does not wait for %s", dependency)
		}
	}
	if !strings.Contains(string(data), `"$GITHUB_REF" == refs/heads/main`) {
		t.Fatal("publication is not restricted to main")
	}
}

func hasWorkflowNeed(value any, want string) bool {
	switch needs := value.(type) {
	case string:
		return needs == want
	case []any:
		for _, need := range needs {
			if need == want {
				return true
			}
		}
	}
	return false
}

func TestPublicKubernetesBundleCheckRequiresUpstreamRelease(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/check-public-kubernetes-bundle"))
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{
		`upstream_release="https://github.com/kubernetes/kubernetes/releases/tag/${payload_version}"`,
		`.annotations["org.opencontainers.image.url"] == $upstream_release`,
		`.annotations["dev.katl.kubernetes.payload.version"] == $payload_version`,
	} {
		if !strings.Contains(string(contents), contract) {
			t.Errorf("public check missing %q", contract)
		}
	}
}
