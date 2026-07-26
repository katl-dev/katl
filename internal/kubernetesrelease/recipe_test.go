package kubernetesrelease

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecipeDigestChangesWithProductionInput(t *testing.T) {
	root := writeRecipeFixture(t)
	first, err := RecipeDigest(root)
	if err != nil {
		t.Fatalf("RecipeDigest() error = %v", err)
	}
	path := filepath.Join(root, "scripts", "build-kubernetes-sysext")
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := RecipeDigest(root)
	if err != nil {
		t.Fatalf("RecipeDigest() error = %v", err)
	}
	if first == second {
		t.Fatal("recipe digest did not change")
	}
}

func TestRecipeDigestIgnoresTests(t *testing.T) {
	root := writeRecipeFixture(t)
	path := filepath.Join(root, "cmd", "katl-mkosi-artifacts", "main_test.go")
	first, err := RecipeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := RecipeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("test-only change affected recipe digest")
	}
}

func TestRecipeDigestTracksBundleProducerSources(t *testing.T) {
	for _, relative := range []string{
		"internal/kubernetesrelease/input.go",
		"internal/installer/artifact/artifact.go",
		"internal/installer/artifact/local.go",
		"internal/installer/payloadbundle/input.go",
		"internal/installer/sysextcatalog/catalog.go",
		"internal/installer/sysextcatalog/publish.go",
		"internal/installer/sysextcatalog/stage.go",
		"cmd/katl-mkosi-artifacts/input.go",
		"cmd/katl-publish-kubernetes-sysext/input.go",
		"mkosi.profiles/kubernetes-sysext/input.go",
		"mkosi.profiles/runtime/mkosi.conf",
	} {
		t.Run(relative, func(t *testing.T) {
			root := writeRecipeFixture(t)
			first, err := RecipeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			second, err := RecipeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			if first == second {
				t.Fatalf("%s did not affect recipe digest", relative)
			}
		})
	}
}

func TestRecipeDigestIgnoresUnrelatedProductAndRuntimeSources(t *testing.T) {
	for _, relative := range []string{
		"cmd/katl-boot-health/main.go",
		"cmd/katlc/input.go",
		"internal/installer/configapply/input.go",
		"internal/installer/generation/input.go",
		"internal/installer/manifest/manifest.go",
		"internal/operatorconsole/input.go",
		"internal/installer/sysextcatalog/select.go",
		"mkosi.profiles/runtime/mkosi.build",
		"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-example.service",
		"scripts/mkosi",
		"docs/input.md",
	} {
		t.Run(relative, func(t *testing.T) {
			root := writeRecipeFixture(t)
			first, err := RecipeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			second, err := RecipeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			if first != second {
				t.Fatalf("%s affected Kubernetes bundle recipe digest", relative)
			}
		})
	}
}

func TestRecipeDigestTracksSymlinkTarget(t *testing.T) {
	root := writeRecipeFixture(t)
	path := filepath.Join(root, "mkosi.profiles", "kubernetes-sysext", "input.link")
	first, err := RecipeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-b", path); err != nil {
		t.Fatal(err)
	}
	second, err := RecipeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("symlink target did not affect recipe digest")
	}
}

func TestRecipeDigestTracksExecutableMode(t *testing.T) {
	root := writeRecipeFixture(t)
	path := filepath.Join(root, "scripts", "build-kubernetes-sysext")
	first, err := RecipeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := RecipeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("executable mode did not affect recipe digest")
	}
}

func TestRefreshRecipeAdvancesEverySupportedArtifact(t *testing.T) {
	root := writeRecipeFixture(t)
	supported := SupportedVersions{
		APIVersion:   APIVersion,
		Kind:         Kind,
		RecipeScope:  CurrentRecipeScope,
		RecipeDigest: "sha256:" + strings.Repeat("a", 64),
		Versions: []SupportedVersion{
			testSupportedVersion("v1.35.9", 2),
			testSupportedVersion("v1.36.3", 1),
		},
	}
	updated, changed, err := RefreshRecipe(root, supported)
	if err != nil {
		t.Fatalf("RefreshRecipe() error = %v", err)
	}
	if !changed || updated.Versions[0].ArtifactRevision != 3 || updated.Versions[1].ArtifactRevision != 2 {
		t.Fatalf("updated = %#v, changed = %t", updated, changed)
	}
	again, changed, err := RefreshRecipe(root, updated)
	if err != nil {
		t.Fatalf("RefreshRecipe() error = %v", err)
	}
	if changed || again.Versions[0].ArtifactRevision != 3 || again.Versions[1].ArtifactRevision != 2 {
		t.Fatalf("second refresh = %#v, changed = %t", again, changed)
	}
}

func TestRefreshRecipeChangesScopeWithoutAdvancingArtifacts(t *testing.T) {
	root := writeRecipeFixture(t)
	supported := SupportedVersions{
		APIVersion:   APIVersion,
		Kind:         Kind,
		RecipeDigest: "sha256:" + strings.Repeat("a", 64),
		Versions: []SupportedVersion{
			testSupportedVersion("v1.35.9", 2),
			testSupportedVersion("v1.36.3", 1),
		},
	}
	updated, changed, err := RefreshRecipe(root, supported)
	if err != nil {
		t.Fatalf("RefreshRecipe() error = %v", err)
	}
	if !changed || updated.RecipeScope != CurrentRecipeScope ||
		updated.Versions[0].ArtifactRevision != 2 ||
		updated.Versions[1].ArtifactRevision != 1 {
		t.Fatalf("updated = %#v, changed = %t", updated, changed)
	}
}

func TestDefaultRecipeDigestMatchesRepository(t *testing.T) {
	supported, err := DefaultSupportedVersions()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RecipeDigest(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if supported.RecipeDigest != digest {
		t.Fatalf("recipe digest = %s, want %s; run go run ./cmd/katl-kubernetes-release refresh-rebuilds", supported.RecipeDigest, digest)
	}
}

func TestKubernetesBundleWorkflowUsesRecipeBoundary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "kubernetes-bundles.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, required := range []string{
		".github/workflows/kubernetes-bundles.yml",
		"Containerfile.mkosi",
		"cmd/katl-kubernetes-release/**",
		"cmd/katl-mkosi-artifacts/**",
		"cmd/katl-publish-kubernetes-sysext/**",
		"containers-policy.json",
		"go.mod",
		"go.sum",
		"internal/installer/artifact/artifact.go",
		"internal/installer/artifact/local.go",
		"internal/installer/payloadbundle/**",
		"internal/installer/sysextcatalog/catalog.go",
		"internal/installer/sysextcatalog/publish.go",
		"internal/installer/sysextcatalog/stage.go",
		"internal/kubernetesrelease/**",
		"mkosi.conf",
		"mkosi.profiles/kubernetes-sysext/**",
		"mkosi.profiles/runtime/mkosi.conf",
		"mkosi.profiles/runtime/os-release.in",
		"scripts/build-kubernetes-sysext",
		"scripts/check-kubernetes-sysext",
	} {
		if !strings.Contains(workflow, "\n      - "+required+"\n") {
			t.Fatalf("Kubernetes bundle workflow does not track recipe input %q", required)
		}
	}
	for _, unrelated := range []string{
		"cmd/katlc/**",
		"internal/**",
		"internal/installer/manifest/**",
		"internal/installer/sysextcatalog/**",
		"mkosi.profiles/runtime/**",
		"scripts/mkosi",
	} {
		if strings.Contains(workflow, "\n      - "+unrelated+"\n") {
			t.Fatalf("Kubernetes bundle workflow tracks unrelated product input %q", unrelated)
		}
	}
}

func testSupportedVersion(payload string, revision int) SupportedVersion {
	numeric := strings.TrimPrefix(payload, "v")
	minor := numeric[:strings.LastIndex(numeric, ".")]
	return SupportedVersion{
		PayloadVersion:   payload,
		ArtifactRevision: revision,
		Packages: PackageVersions{
			Kubeadm:  "0:" + numeric + "-1",
			Kubelet:  "0:" + numeric + "-1",
			Kubectl:  "0:" + numeric + "-1",
			CRITools: "0:" + minor + ".0-1",
		},
	}
}

func writeRecipeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	directories := map[string]bool{
		"cmd/katl-kubernetes-release":        true,
		"cmd/katl-mkosi-artifacts":           true,
		"cmd/katl-publish-kubernetes-sysext": true,
		"internal/installer/payloadbundle":   true,
		"internal/kubernetesrelease":         true,
		"mkosi.profiles/kubernetes-sysext":   true,
	}
	for _, input := range recipeRoots {
		path := filepath.Join(root, filepath.FromSlash(input))
		if directories[input] {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "input.go"), []byte(input), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for relative, content := range map[string]string{
		"cmd/katl-mkosi-artifacts/main_test.go": "test",
		"cmd/katlc/input.go":                    "agent",
		"docs/input.md":                         "documentation",
		"internal/operatorconsole/input.go":     "runtime",
		"mkosi.profiles/runtime/mkosi.build":    "runtime build",
		"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-example.service": "runtime unit",
	} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "mkosi.profiles", "kubernetes-sysext", "input.link")
	if err := os.Symlink("target-a", link); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "--quiet")
	runGit(t, root, "add", ".")
	return root
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
