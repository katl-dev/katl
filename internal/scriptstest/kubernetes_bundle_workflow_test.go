package scriptstest

import (
	"strings"
	"testing"
)

func TestKubernetesBundleWorkflowContract(t *testing.T) {
	repo := repoRoot(t)
	workflow := string(mustReadFile(t, repo+"/.github/workflows/kubernetes-bundles.yml"))

	required := []string{
		"push:",
		"- main",
		"mkosi.profiles/kubernetes-sysext/kubernetes.env",
		"payload_version:",
		"artifact_version:",
		"publish:",
		"artifact-metadata: write",
		"packages: write",
		"oras_1.3.3_linux_amd64.tar.gz",
		"9ce999f8d2de03fc03968b29d743077a58783e545e5eaa53917ca177352d0e59",
		"scripts/check-kubernetes-sysext",
		`source mkosi.profiles/kubernetes-sysext/kubernetes.env`,
		`KATL_KUBERNETES_ARTIFACT_REVISION`,
		`PUBLISH=true`,
		`env.PUBLISH == 'true'`,
		"go run ./cmd/katl-publish-kubernetes-sysext",
		"ghcr.io/katl-dev/kubernetes",
		"org.opencontainers.image.description",
		"org.opencontainers.image.licenses",
		"org.opencontainers.image.source",
		"org.opencontainers.image.documentation",
		"Kubernetes system extension bundle for KatlOS nodes",
		"application/vnd.katl.kubernetes.payload.bundle.v1",
		"sha256-",
		`version_tag="$ARTIFACT_VERSION"`,
		"immutable OCI tag already exists",
		"actions/attest@v4",
	}
	for _, value := range required {
		if !strings.Contains(workflow, value) {
			t.Fatalf("Kubernetes bundle workflow missing %q", value)
		}
	}
	for _, key := range []string{
		"org.opencontainers.image.description",
		"org.opencontainers.image.licenses",
		"org.opencontainers.image.source",
	} {
		if count := strings.Count(workflow, `--annotation "`+key+`=`); count != 2 {
			t.Fatalf("Kubernetes bundle workflow annotation %q count = %d, want local and published copies", key, count)
		}
	}

	runtimeBuild := strings.Index(workflow, "scripts/mkosi build-runtime")
	sysextBuild := strings.Index(workflow, "scripts/mkosi build-kubernetes-sysext")
	if runtimeBuild < 0 || sysextBuild < 0 || runtimeBuild >= sysextBuild {
		t.Fatalf("Kubernetes bundle workflow must build the compatible runtime before the sysext")
	}

	releaseWorkflow := string(mustReadFile(t, repo+"/.github/workflows/release-artifacts.yml"))
	if !strings.Contains(releaseWorkflow, `- "v*"`) || strings.Contains(releaseWorkflow, `- "**"`) {
		t.Fatalf("KatlOS release workflow must select KatlOS tags without consuming bundle tags")
	}
	for _, value := range []string{
		"attestations: write",
		"id-token: write",
		"actions/attest@v4",
		"subject-path: dist/*",
		"Verify published release provenance",
		`--signer-workflow "$GITHUB_REPOSITORY/.github/workflows/release-artifacts.yml"`,
		`--source-ref "$GITHUB_REF"`,
		`--source-digest "$GITHUB_SHA"`,
		"fetch-depth: 0",
		"--notes-file dist/RELEASE_NOTES.md",
		`katl-release-artifacts build-katlctl "$KATL_VERSION"`,
	} {
		if !strings.Contains(releaseWorkflow, value) {
			t.Fatalf("KatlOS release workflow missing %q", value)
		}
	}
	if strings.Contains(releaseWorkflow, "--generate-notes") {
		t.Fatal("KatlOS releases must use the tested staged release notes")
	}
}

func TestKubernetesBundleRenovateContract(t *testing.T) {
	repo := repoRoot(t)
	config := string(mustReadFile(t, repo+"/renovate.json"))
	manifest := string(mustReadFile(t, repo+"/mkosi.profiles/kubernetes-sysext/kubernetes.env"))

	for _, value := range []string{
		`"customType": "regex"`,
		`github-releases`,
		`datasource=(?<datasource>rpm)`,
		`Kubernetes v1.36 bundle`,
		`"automerge": false`,
		`KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT=1`,
	} {
		if !strings.Contains(config, value) {
			t.Fatalf("Renovate config missing %q", value)
		}
	}

	for _, value := range []string{
		`https://pkgs.k8s.io/core:/stable:/v1.36/rpm/repodata/`,
		`KATL_KUBERNETES_PAYLOAD_DEFAULT=v1.36.0`,
		`KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT=3`,
		`KATL_KUBERNETES_KUBEADM_VERSION:=0:1.36.0-150500.1.1`,
		`KATL_KUBERNETES_KUBELET_VERSION:=0:1.36.0-150500.1.1`,
		`KATL_KUBERNETES_KUBECTL_VERSION:=0:1.36.0-150500.1.1`,
		`KATL_KUBERNETES_CRITOOLS_VERSION:=0:1.36.0-150500.1.1`,
	} {
		if !strings.Contains(manifest, value) {
			t.Fatalf("Kubernetes release lock missing %q", value)
		}
	}
}
