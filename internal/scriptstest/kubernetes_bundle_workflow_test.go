package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKubernetesBundleWorkflowLinksUpstreamRelease(t *testing.T) {
	repo := repoRoot(t)
	contents, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "kubernetes-bundles.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			Env   map[string]string `yaml:"env"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse Kubernetes bundle workflow: %v", err)
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("Kubernetes bundle workflow does not build relevant pull requests")
	}

	build, ok := workflow.Jobs["build"]
	if !ok {
		t.Fatal("Kubernetes bundle workflow has no build job")
	}
	const releaseURL = "https://github.com/kubernetes/kubernetes/releases/tag/${{ matrix.release.payloadVersion }}"
	if got := build.Env["KUBERNETES_RELEASE_URL"]; got != releaseURL {
		t.Fatalf("KUBERNETES_RELEASE_URL = %q, want %q", got, releaseURL)
	}

	var packStep, publishStep string
	for _, step := range build.Steps {
		switch step.Name {
		case "Stage Kubernetes payload bundle":
			packStep = step.Run
		case "Publish immutable OCI bundle":
			publishStep = step.Run
		}
	}
	if packStep == "" {
		t.Fatal("Kubernetes bundle workflow has no common OCI pack validation")
	}
	if publishStep == "" {
		t.Fatal("Kubernetes bundle workflow has no immutable OCI publication step")
	}

	for _, contract := range []string{
		`go run ./cmd/katl-publish-kubernetes-sysext`,
		`oci-manifest-digest:`,
		`oci-manifest-tag:`,
		`manifest-sha256-`,
		`--annotation "org.opencontainers.image.url=${KUBERNETES_RELEASE_URL}"`,
		`--annotation "dev.katl.kubernetes.payload.version=${PAYLOAD_VERSION}"`,
	} {
		if !strings.Contains(packStep, contract) {
			t.Errorf("common OCI pack step does not enforce %q", contract)
		}
	}
	for _, contract := range []string{
		`go run ./cmd/katl-publish-kubernetes-sysext`,
		`--publish-ref "${OCI_REPOSITORY}:${VERSION_TAG}"`,
		`--publish-ref "${OCI_REPOSITORY}:${DIGEST_TAG}"`,
		`--annotation "org.opencontainers.image.url=${KUBERNETES_RELEASE_URL}"`,
		`--annotation "dev.katl.kubernetes.payload.version=${PAYLOAD_VERSION}"`,
	} {
		if !strings.Contains(publishStep, contract) {
			t.Errorf("OCI publication step does not enforce %q", contract)
		}
	}
	if strings.Contains(string(contents), "oras push") || strings.Contains(string(contents), "oras cp") {
		t.Fatal("Kubernetes workflow must not assemble or publish through a second OCI implementation")
	}
}

func TestPublicKubernetesBundleCheckRequiresUpstreamRelease(t *testing.T) {
	repo := repoRoot(t)
	contents, err := os.ReadFile(filepath.Join(repo, "scripts", "check-public-kubernetes-bundle"))
	if err != nil {
		t.Fatal(err)
	}
	check := string(contents)
	for _, contract := range []string{
		`upstream_release="https://github.com/kubernetes/kubernetes/releases/tag/${payload_version}"`,
		`.annotations["org.opencontainers.image.url"] == $upstream_release`,
		`.annotations["dev.katl.kubernetes.payload.version"] == $payload_version`,
	} {
		if !strings.Contains(check, contract) {
			t.Errorf("public Kubernetes bundle check does not enforce %q", contract)
		}
	}
}

func TestKubernetesBundleWorkflowRetiresSupersededCompatibilityPRs(t *testing.T) {
	repo := repoRoot(t)
	contents, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "kubernetes-bundles.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Name string `yaml:"name"`
				Uses string `yaml:"uses"`
				With struct {
					Script string `yaml:"script"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse Kubernetes bundle workflow: %v", err)
	}

	compatibility, ok := workflow.Jobs["compatibility"]
	if !ok {
		t.Fatal("Kubernetes bundle workflow has no compatibility job")
	}
	if got := compatibility.Permissions["actions"]; got != "write" {
		t.Fatalf("compatibility actions permission = %q, want write", got)
	}

	var script string
	for _, step := range compatibility.Steps {
		if step.Name == "Open compatibility pull request" {
			script = step.With.Script
			break
		}
	}
	if script == "" {
		t.Fatal("Kubernetes bundle workflow has no compatibility pull request step")
	}
	for _, contract := range []string{
		"github.paginate(github.rest.pulls.list",
		`candidate.head.ref.startsWith(branchPrefix)`,
		`candidate.head.repo?.full_name === repository`,
		`candidate.number !== pull.number`,
		"github.rest.issues.createComment",
		"github.rest.pulls.update",
		`state: "closed"`,
		"github.rest.actions.listWorkflowRuns",
		`run.head_sha === process.env.HEAD_SHA`,
		`validationRun.conclusion === "action_required"`,
		`/actions/runs/{run_id}/approve`,
		"run_id: validationRun.id",
	} {
		if !strings.Contains(script, contract) {
			t.Errorf("compatibility pull request step does not enforce %q", contract)
		}
	}
	if strings.Contains(script, "createWorkflowDispatch") {
		t.Error("compatibility pull request step still dispatches a duplicate Fast Checks run")
	}
}
