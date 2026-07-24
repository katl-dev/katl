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

	build, ok := workflow.Jobs["build"]
	if !ok {
		t.Fatal("Kubernetes bundle workflow has no build job")
	}
	const releaseURL = "https://github.com/kubernetes/kubernetes/releases/tag/${{ matrix.release.payloadVersion }}"
	if got := build.Env["KUBERNETES_RELEASE_URL"]; got != releaseURL {
		t.Fatalf("KUBERNETES_RELEASE_URL = %q, want %q", got, releaseURL)
	}

	var layoutStep, publishStep string
	for _, step := range build.Steps {
		switch step.Name {
		case "Validate OCI artifact layout":
			layoutStep = step.Run
		case "Publish immutable OCI bundle":
			publishStep = step.Run
		}
	}
	if layoutStep == "" {
		t.Fatal("Kubernetes bundle workflow has no local OCI layout validation")
	}
	if publishStep == "" {
		t.Fatal("Kubernetes bundle workflow has no immutable OCI publication step")
	}

	for _, contract := range []string{
		`--annotation "org.opencontainers.image.url=${KUBERNETES_RELEASE_URL}"`,
		`--annotation "dev.katl.kubernetes.payload.version=${PAYLOAD_VERSION}"`,
		`.annotations["org.opencontainers.image.url"] == $upstream_release`,
		`.annotations["dev.katl.kubernetes.payload.version"] == $payload_version`,
	} {
		if !strings.Contains(layoutStep, contract) {
			t.Errorf("local OCI layout step does not enforce %q", contract)
		}
	}
	for _, contract := range []string{
		`.annotations["org.opencontainers.image.url"] == $upstream_release`,
		`.annotations["dev.katl.kubernetes.payload.version"] == $payload_version`,
	} {
		if !strings.Contains(publishStep, contract) {
			t.Errorf("OCI publication step does not enforce %q", contract)
		}
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
	if got := compatibility.Permissions["actions"]; got != "read" {
		t.Fatalf("compatibility actions permission = %q, want read", got)
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
	} {
		if !strings.Contains(script, contract) {
			t.Errorf("compatibility pull request step does not enforce %q", contract)
		}
	}
	if strings.Contains(script, "createWorkflowDispatch") {
		t.Error("compatibility pull request step still dispatches a duplicate Fast Checks run")
	}
}
