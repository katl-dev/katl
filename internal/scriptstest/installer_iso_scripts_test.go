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
	if err := os.WriteFile(installer+".json", []byte(`{"version":"2026.7.0-dev.1","architecture":"x86_64"}`), 0o644); err != nil {
		t.Fatal(err)
	}
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
	cmd.Env = append(
		os.Environ(),
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

func TestInstallerWaitsForNetworkBeforeURLHandoff(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join(repoRoot(t), "mkosi.profiles", "installer-image", "mkosi.extra", "usr", "lib", "systemd", "system", "katlos-install.service"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	if !strings.Contains(text, "\nType=oneshot\n") || strings.Contains(text, "\nType=notify\n") {
		t.Fatal("installer service must block initrd completion as a oneshot")
	}
	if !strings.Contains(text, "\nTimeoutStartSec=infinity\n") {
		t.Fatal("installer handoff and safety holds must not expire")
	}
	for _, directive := range []string{"Wants=", "After="} {
		line := ""
		for _, candidate := range strings.Split(text, "\n") {
			if strings.HasPrefix(candidate, directive) {
				line = candidate
				break
			}
		}
		if !strings.Contains(" "+line+" ", " network-online.target ") {
			t.Fatalf("installer unit %s does not include network-online.target", directive)
		}
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
	cmd.Env = append(
		os.Environ(),
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
