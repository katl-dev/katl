package scriptstest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
				FailFast *bool `yaml:"fail-fast"`
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
	if _, ok := workflow.On["push"]; ok {
		t.Fatal("Katl commits must not trigger Kubernetes publication")
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("producer has no presubmit")
	}
	if _, ok := workflow.Jobs["compatibility"]; ok {
		t.Fatal("publication still depends on a shared catalog update")
	}
	build := workflow.Jobs["build"]
	if build.Strategy.FailFast == nil || *build.Strategy.FailFast {
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
		{"Inspect published upstream release", "Build package query environment"},
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
}

func TestKubernetesReleasePlan(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/kubernetes-bundles.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ ID, Run string } `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.ID == "plan" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("release plan script is missing")
	}

	for _, test := range []struct {
		name, event, ref, version, publish string
		empty, queryFailure, wantFailure   bool
		wantPublish                        bool
	}{
		{name: "scheduled", event: "schedule", ref: "refs/heads/main", wantPublish: true},
		{name: "main push", event: "push", ref: "refs/heads/main"},
		{name: "branch push", event: "push", ref: "refs/heads/topic"},
		{name: "pull request", event: "pull_request", ref: "refs/pull/12/merge"},
		{name: "pull request with main ref", event: "pull_request", ref: "refs/heads/main"},
		{name: "unrelated pull request", event: "pull_request", ref: "refs/pull/12/merge", empty: true},
		{name: "manual defaults", event: "workflow_dispatch", ref: "refs/heads/main"},
		{name: "manual publish", event: "workflow_dispatch", ref: "refs/heads/main", version: "v1.37.0", publish: "true", queryFailure: true, wantPublish: true},
		{name: "branch cannot publish", event: "workflow_dispatch", ref: "refs/heads/topic", version: "v1.37.0", publish: "true"},
		{name: "reject prerelease", event: "workflow_dispatch", ref: "refs/heads/main", version: "v1.37.0-rc.1", wantFailure: true},
		{name: "upstream failure", event: "schedule", ref: "refs/heads/main", queryFailure: true, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			// Substitute only the external discovery process; execute the real workflow policy.
			stub := "#!/bin/sh\n[ \"$3\" = \"$EXPECTED_COMMAND\" ] || exit 2\n[ \"$QUERY_FAILURE\" != true ] || exit 1\nprintf '%s\\n' \"$RELEASE_MATRIX\"\n"
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "output")
			matrix := `{"include":[{"payloadVersion":"v1.37.0"}]}`
			if test.empty {
				matrix = `{"include":[]}`
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GITHUB_EVENT_NAME", test.event)
			t.Setenv("GITHUB_REF", test.ref)
			t.Setenv("GITHUB_OUTPUT", output)
			t.Setenv("PAYLOAD_VERSION", test.version)
			t.Setenv("REQUEST_PUBLISH", test.publish)
			t.Setenv("BASE_SHA", "base")
			command := "discover"
			if test.event == "pull_request" {
				command = "presubmit"
			}
			t.Setenv("EXPECTED_COMMAND", command)
			t.Setenv("RELEASE_MATRIX", matrix)
			t.Setenv("QUERY_FAILURE", strconv.FormatBool(test.queryFailure))
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
			log, err := cmd.CombinedOutput()
			if test.wantFailure {
				if err == nil {
					t.Fatalf("invalid release plan succeeded: %s", log)
				}
				if data, _ := os.ReadFile(output); strings.Contains(string(data), "publish=true") {
					t.Fatal("failed plan enabled publication")
				}
				return
			}
			if err != nil {
				t.Fatalf("release plan: %v\n%s", err, log)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				key, value, _ := strings.Cut(line, "=")
				values[key] = value
			}
			if values["publish"] != strconv.FormatBool(test.wantPublish) || values["build"] != strconv.FormatBool(!test.empty) {
				t.Fatalf("release decisions = %v", values)
			}
			var got struct {
				Include []struct {
					PayloadVersion string `json:"payloadVersion"`
				} `json:"include"`
			}
			if err := json.Unmarshal([]byte(values["matrix"]), &got); err != nil {
				t.Fatal(err)
			}
			if test.empty {
				if len(got.Include) != 0 {
					t.Fatalf("unexpected releases: %+v", got.Include)
				}
			} else if len(got.Include) != 1 || got.Include[0].PayloadVersion != "v1.37.0" {
				t.Fatalf("release matrix = %+v", got.Include)
			}
		})
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
