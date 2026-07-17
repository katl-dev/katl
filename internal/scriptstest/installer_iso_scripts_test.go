package scriptstest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildInstallerISO(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	installer := writeArtifact(t, tmp, "katl-installer.efi", "installer")
	katlosImage := writeArtifact(t, tmp, "katlos-install-2026.7.0-dev.1-x86_64.squashfs", "katlos")
	writeKatlosImageSidecars(t, katlosImage, "2026.7.0-dev.1")
	output := filepath.Join(tmp, "katl-installer.iso")
	for _, tool := range []string{"mkfs.vfat", "mcopy", "mmd"} {
		writeFakeExecutable(t, bin, tool, "exit 0\n")
	}
	writeFakeExecutable(t, bin, "xorriso", `
output=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-output" ]]; then
    output="$2"
    break
  fi
  shift
done
[[ -n "$output" ]]
touch "$output"
`)
	cmd := exec.Command(filepath.Join(repo, "scripts", "build-installer-iso"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_INSTALLER_UKI="+installer,
		"KATL_KATLOS_IMAGE="+katlosImage,
		"KATL_VERSION=2026.7.0-dev.1",
		"KATL_ARCHITECTURE=x86_64",
		"KATL_INSTALLER_ISO="+output,
		"TMPDIR="+tmp,
	)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build installer ISO failed: %v\n%s", err, result)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("installer ISO output missing: %v", err)
	}
}

func TestCheckInstallerISO(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	installer := writeArtifact(t, tmp, "katl-installer.efi", "installer")
	katlosImage := writeArtifact(t, tmp, "katlos-install-2026.7.0-dev.1-x86_64.squashfs", "katlos")
	katlosDigest := sha256.Sum256(mustReadFile(t, katlosImage))
	katlosMetadata, err := json.Marshal(map[string]any{
		"apiVersion":       "katl.dev/v1alpha1",
		"kind":             "KatlOSImageArtifact",
		"imageRole":        "install",
		"format":           "squashfs",
		"path":             filepath.Base(katlosImage),
		"sha256":           hex.EncodeToString(katlosDigest[:]),
		"sizeBytes":        len("katlos"),
		"version":          "2026.7.0-dev.1",
		"architecture":     "x86_64",
		"runtimeInterface": "katl-runtime-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(katlosImage+".json", append(katlosMetadata, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact := writeArtifact(t, tmp, "katl-installer.iso", "iso")
	digest := sha256.Sum256(mustReadFile(t, artifact))
	digestText := hex.EncodeToString(digest[:])
	metadata, err := json.Marshal(map[string]any{
		"kind":         "InstallerBootArtifact",
		"artifactRole": "installer-iso",
		"format":       "iso",
		"sha256":       digestText,
		"sizeBytes":    len("iso"),
		"version":      "2026.7.0-dev.1",
		"architecture": "x86_64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact+".json", append(metadata, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact+".sha256", []byte(digestText+"  "+filepath.Base(artifact)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeExecutable(t, bin, "xorriso", `
if [[ " $* " == *" -report_el_torito plain "* ]]; then
  echo "El Torito boot img : 1 EFI"
  exit 0
fi
if [[ " $* " == *" -extract /efiboot.img "* ]]; then
  touch "${@: -1}"
  exit 0
fi
if [[ " $* " == *" -extract /katl/media.json "* ]]; then
  cp "$KATL_TEST_MEDIA_METADATA" "${@: -1}"
  exit 0
fi
if [[ " $* " == *" -extract /katl/images/"* ]]; then
  cp "$KATL_TEST_KATLOS_IMAGE" "${@: -1}"
  exit 0
fi
exit 1
`)
	writeFakeExecutable(t, bin, "mcopy", `cp "$KATL_TEST_INSTALLER" "${@: -1}"`+"\n")
	cmd := exec.Command(filepath.Join(repo, "scripts", "check-installer-iso"), artifact)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_INSTALLER_UKI="+installer,
		"KATL_TEST_INSTALLER="+installer,
		"KATL_KATLOS_IMAGE="+katlosImage,
		"KATL_TEST_KATLOS_IMAGE="+katlosImage,
		"KATL_TEST_MEDIA_METADATA="+katlosImage+".json",
		"KATL_VERSION=2026.7.0-dev.1",
		"KATL_ARCHITECTURE=x86_64",
		"TMPDIR="+tmp,
	)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check installer ISO failed: %v\n%s", err, result)
	} else if !strings.Contains(string(result), "ok: "+artifact) {
		t.Fatalf("check output = %q", result)
	}
}

func TestMkosiInstallerISOUsesBuilder(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	buildDir := filepath.Join(tmp, "mkosi-build")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	katlosImage := writeArtifact(t, buildDir, "katlos-install-2026.7.0-dev.1-x86_64.squashfs", "katlos")
	writeKatlosImageSidecars(t, katlosImage, "2026.7.0-dev.1")
	preserveFile(t, filepath.Join(repo, "_build", "mkosi", "katl-installer.packages.tsv"))
	seedInstallerRPMCache(t, repo)
	podmanArgs := filepath.Join(tmp, "podman-args.txt")
	writeFakeExecutable(t, bin, "podman", `
if [[ "${1:-}" == "image" && "${2:-}" == "exists" ]]; then
  exit 0
fi
if [[ "${1:-}" == "image" && "${2:-}" == "inspect" ]]; then
  echo fake-builder-image-id
  exit 0
fi
printf '%s\n' "$@" > "$KATL_FAKE_PODMAN_ARGS"
`)
	writeFakeExecutable(t, bin, "rpm", "printf 'systemd\\t0:259.6-1.fc44.x86_64\\n'\n")
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-installer-iso")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=podman",
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_FAKE_PODMAN_ARGS="+podmanArgs,
		"KATL_VERSION=2026.7.0-dev.1",
		"TMPDIR="+tmp,
	)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scripts/mkosi build-installer-iso failed: %v\n%s", err, result)
	}
	args := readLinesForScripts(t, podmanArgs)
	for _, want := range []string{
		"KATL_EMIT_INSTALLER_ISO=1",
		"KATL_VERSION=2026.7.0-dev.1",
		"--profile",
		"installer-image",
		"-f",
		"build",
	} {
		if !containsString(args, want) {
			t.Fatalf("podman args missing %q: %#v", want, args)
		}
	}
}

func writeKatlosImageSidecars(t *testing.T, image, version string) {
	t.Helper()
	content := mustReadFile(t, image)
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	metadata, err := json.Marshal(map[string]any{
		"apiVersion":       "katl.dev/v1alpha1",
		"kind":             "KatlOSImageArtifact",
		"imageRole":        "install",
		"format":           "squashfs",
		"path":             filepath.Base(image),
		"sha256":           digestText,
		"sizeBytes":        len(content),
		"version":          version,
		"architecture":     "x86_64",
		"runtimeInterface": "katl-runtime-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image+".json", append(metadata, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image+".sha256", []byte(digestText+"  "+filepath.Base(image)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMkosiInstallerBuildClearsStaleISO(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	buildDir := filepath.Join(tmp, "mkosi-build")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	preserveFile(t, filepath.Join(repo, "_build", "mkosi", "katl-installer.packages.tsv"))
	seedInstallerRPMCache(t, repo)
	for _, name := range []string{"katl-installer.iso", "katl-installer.iso.json", "katl-installer.iso.sha256"} {
		writeArtifact(t, buildDir, name, "stale")
	}
	writeFakeExecutable(t, bin, "podman", `
if [[ "${1:-}" == "image" && "${2:-}" == "exists" ]]; then
  exit 0
fi
if [[ "${1:-}" == "image" && "${2:-}" == "inspect" ]]; then
  echo fake-builder-image-id
  exit 0
fi
exit 0
`)
	writeFakeExecutable(t, bin, "rpm", "printf 'systemd\\t0:259.6-1.fc44.x86_64\\n'\n")
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-installer")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=podman",
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"TMPDIR="+tmp,
	)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scripts/mkosi build-installer failed: %v\n%s", err, result)
	}
	for _, name := range []string{"katl-installer.iso", "katl-installer.iso.json", "katl-installer.iso.sha256"} {
		if _, err := os.Stat(filepath.Join(buildDir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s still exists: %v", name, err)
		}
	}
}
