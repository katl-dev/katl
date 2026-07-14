package scriptstest

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
		"record-compatibility",
		"internal/installer/kubernetescompat/catalog.json",
		"automation/kubernetes-compatibility-",
		"draft: false",
		"createWorkflowDispatch",
		"enablePullRequestAutoMerge",
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

func TestKatlReleaseWorkflowBuildGraph(t *testing.T) {
	repo := repoRoot(t)
	workflow := string(mustReadFile(t, repo+"/.github/workflows/release-artifacts.yml"))
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
		t.Fatalf("parse KatlOS release workflow: %v", err)
	}

	if count := strings.Count(workflow, "scripts/mkosi build-runtime"); count != 1 {
		t.Fatalf("KatlOS release workflow runtime build count = %d, want exactly one", count)
	}
	for _, value := range []string{
		"  runtime:\n",
		"  installer:\n",
		"  katlctl:\n",
		"  assemble:\n",
		"name: katl-runtime-images-${{ github.sha }}",
		"name: katl-installer-${{ github.sha }}",
		"_build/mkosi/katl-runtime.packages.tsv",
		"scripts/build-katlos-install-image &",
		"KATL_KATLOS_IMAGE_ROLE=upgrade scripts/build-katlos-install-image &",
	} {
		if !strings.Contains(workflow, value) {
			t.Fatalf("KatlOS release workflow missing parallel build contract %q", value)
		}
	}

	assemble := workflow[strings.Index(workflow, "  assemble:\n"):strings.Index(workflow, "  publish-tag:\n")]
	assertTextContains(t, assemble,
		"- runtime",
		"- installer",
		"- katlctl",
		"scripts/build-installer-iso",
		"write-installer-artifacts",
		"actions/attest@v4",
	)
	if !regexp.MustCompile(`xargs[^\n]*-P[1-9][0-9]*`).MatchString(workflow) {
		t.Fatal("KatlOS release workflow does not verify published attestations with bounded concurrency")
	}
}

func TestPublicKubernetesBundleWorkflowContract(t *testing.T) {
	repo := repoRoot(t)
	workflow := string(mustReadFile(t, repo+"/.github/workflows/public-kubernetes-bundle.yml"))
	check := string(mustReadFile(t, repo+"/scripts/check-public-kubernetes-bundle"))

	for _, value := range []string{
		"schedule:",
		"workflow_dispatch:",
		"packages: read",
		"scripts/check-public-kubernetes-bundle",
		"source mkosi.profiles/kubernetes-sysext/kubernetes.env",
		`artifact_version="${KATL_KUBERNETES_PAYLOAD_VERSION}-katl.${KATL_KUBERNETES_ARTIFACT_REVISION}"`,
		`verification="$(scripts/check-public-kubernetes-bundle "$image")"`,
		`digest="${verification##*@}"`,
		"gh attestation verify",
		"docker login ghcr.io",
		"--repo katl-dev/katl",
		"--signer-workflow katl-dev/katl/.github/workflows/kubernetes-bundles.yml",
		"--source-ref refs/heads/main",
	} {
		if !strings.Contains(workflow, value) {
			t.Fatalf("public Kubernetes bundle workflow missing %q", value)
		}
	}

	for _, value := range []string{
		"https://ghcr.io/token?service=ghcr.io&scope=repository:${repository}:pull",
		`fetch_manifest "$tag"`,
		`resolved_digest="sha256:$(sha256sum "$work/tag.json"`,
		`fetch_manifest "$expected_digest"`,
		`cmp --silent "$work/tag.json" "$work/digest.json"`,
		`sha256sum "$blob"`,
		`actual_size="$(stat -c %s "$blob")"`,
		`.artifactVersion == $artifact_version`,
		`.payloadVersion == $payload_version`,
		`index("katl-runtime-1")`,
		`index("kubeadm.k8s.io/v1beta4")`,
	} {
		if !strings.Contains(check, value) {
			t.Fatalf("public Kubernetes bundle check missing %q", value)
		}
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
		`KATL_KUBERNETES_ARTIFACT_REVISION_DEFAULT=1`,
		`KATL_KUBERNETES_PAYLOAD_DEFAULT=`,
		`KATL_KUBERNETES_KUBEADM_VERSION:=`,
		`KATL_KUBERNETES_KUBELET_VERSION:=`,
		`KATL_KUBERNETES_KUBECTL_VERSION:=`,
		`KATL_KUBERNETES_CRITOOLS_VERSION:=`,
	} {
		if !strings.Contains(manifest, value) {
			t.Fatalf("Kubernetes release lock missing %q", value)
		}
	}
	payload := regexp.MustCompile(`KATL_KUBERNETES_PAYLOAD_DEFAULT=v([0-9]+\.[0-9]+\.[0-9]+)`).FindStringSubmatch(manifest)
	if payload == nil {
		t.Fatal("Kubernetes release lock has no exact payload version")
	}
	for _, name := range []string{"KUBEADM", "KUBELET", "KUBECTL"} {
		match := regexp.MustCompile(`KATL_KUBERNETES_` + name + `_VERSION:=0:([0-9]+\.[0-9]+\.[0-9]+)-`).FindStringSubmatch(manifest)
		if match == nil || match[1] != payload[1] {
			t.Fatalf("%s package version does not match payload %s", name, payload[1])
		}
	}

	prepare := string(mustReadFile(t, repo+"/scripts/prepare-kubernetes-release"))
	for _, value := range []string{
		"go run ./cmd/katl-kubernetes-release prepare",
		"scripts/mkosi build-runtime",
		"scripts/mkosi build-kubernetes-sysext",
		"scripts/check-kubernetes-sysext",
		"go run ./cmd/katl-kubernetes-release identity",
	} {
		if !strings.Contains(prepare, value) {
			t.Fatalf("Kubernetes release preparation missing %q", value)
		}
	}
}

func TestKubernetesUpgradeVMTestArtifactContract(t *testing.T) {
	repo := repoRoot(t)
	runner := string(mustReadFile(t, repo+"/scripts/vmtest-run"))
	for _, value := range []string{
		"verify_kubernetes_upgrade_artifacts",
		`scripts/check-kubernetes-sysext`,
		`katl-kubernetes-upgrade.raw`,
		`KATL_KUBERNETES_UPGRADE_PAYLOAD_VERSION`,
		`published Kubernetes upgrade proof requires both KATL_VMTEST_KUBERNETES_BUNDLE and KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE`,
		`-package-set installer-image`,
		`-package-set runtime`,
		`-package-set katlos-install-image`,
		`sha256sum katl-kubernetes-upgrade.raw > katl-kubernetes-upgrade.raw.sha256`,
	} {
		source := runner
		if strings.HasPrefix(value, "sha256sum ") {
			source = string(mustReadFile(t, repo+"/scripts/mkosi"))
		}
		if !strings.Contains(source, value) {
			t.Fatalf("Kubernetes upgrade vmtest contract missing %q", value)
		}
	}

	workflow := string(mustReadFile(t, repo+"/.github/workflows/vmtest.yml"))
	for _, value := range []string{
		"Kubernetes Bundle Upgrade VM",
		"^TestKubeadmUpgradeOperationSmoke$",
		"KATL_VMTEST_KUBERNETES_BUNDLE",
		"KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE",
		"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.3@sha256:c974730cb3500dc4a82cb942138b9f32c1b2e9163469d5073dbedc83c8cd728b",
		"ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:1793f4aed888b48891e659cf286a88088f39a87311d5710c889341aff3f5c537",
	} {
		if !strings.Contains(workflow, value) {
			t.Fatalf("Kubernetes upgrade vmtest workflow missing %q", value)
		}
	}
}
