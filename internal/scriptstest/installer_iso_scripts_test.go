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
	for _, tool := range []string{"mkfs.vfat", "mcopy", "mmd", "xorriso"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("real ISO test requires %s: %v", tool, err)
		}
	}
	installer := writeArtifact(t, tmp, "katl-installer.efi", "installer")
	if err := os.WriteFile(installer+".json", []byte(`{"version":"2026.7.0-dev.1","architecture":"x86_64"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	katlosImage := writeArtifact(t, tmp, "katlos-install-2026.7.0-dev.1-x86_64.squashfs", "katlos")
	writeKatlosImageSidecars(t, katlosImage, "2026.7.0-dev.1")
	output := filepath.Join(tmp, "katl-installer.iso")
	cmd := exec.Command(filepath.Join(repo, "scripts", "build-installer-iso"))
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
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
	for _, path := range []string{"/efiboot.img", "/katl/media.json", "/katl/images/" + filepath.Base(katlosImage)} {
		extracted := filepath.Join(tmp, filepath.Base(path))
		cmd := exec.Command("xorriso", "-osirrox", "on", "-indev", output, "-extract", path, extracted)
		if result, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("extract %s: %v\n%s", path, err, result)
		}
	}
	if result, err := exec.Command("mcopy", "-i", filepath.Join(tmp, "efiboot.img"), "::/EFI/BOOT/BOOTX64.EFI", filepath.Join(tmp, "extracted-installer.efi")).CombinedOutput(); err != nil {
		t.Fatalf("extract EFI executable: %v\n%s", err, result)
	}
	for _, pair := range [][2]string{
		{installer, filepath.Join(tmp, "extracted-installer.efi")},
		{katlosImage, filepath.Join(tmp, filepath.Base(katlosImage))},
		{katlosImage + ".json", filepath.Join(tmp, "media.json")},
	} {
		if string(mustReadFile(t, pair[0])) != string(mustReadFile(t, pair[1])) {
			t.Errorf("ISO payload differs from %s", pair[0])
		}
	}
	digest := sha256.Sum256(mustReadFile(t, output))
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	checksum := hex.EncodeToString(digest[:])
	metadata, err := json.Marshal(map[string]any{
		"kind": "InstallerBootArtifact", "artifactRole": "installer-iso", "format": "iso",
		"sha256": checksum, "sizeBytes": info.Size(), "version": "2026.7.0-dev.1", "architecture": "x86_64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output+".json", append(metadata, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output+".sha256", []byte(checksum+"  "+filepath.Base(output)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := exec.Command(filepath.Join(repo, "scripts", "check-installer-iso"), output)
	check.Dir = repo
	check.Env = cmd.Env
	if result, err := check.CombinedOutput(); err != nil {
		t.Fatalf("verify built installer ISO: %v\n%s", err, result)
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
