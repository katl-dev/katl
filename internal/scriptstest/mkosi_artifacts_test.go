package scriptstest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMkosiArtifactsWriteProducesValidJSON(t *testing.T) {
	repo := repoRoot(t)
	buildDir := filepath.Join(repo, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", buildDir, err)
	}
	workDir, err := os.MkdirTemp(buildDir, "mkosi artifacts \"test\" ")
	if err != nil {
		t.Fatalf("MkdirTemp(%s) error = %v", buildDir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(workDir); err != nil {
			t.Fatalf("RemoveAll(%s) error = %v", workDir, err)
		}
	})

	installerUKI := writeArtifact(t, workDir, "installer.efi", "installer uki")
	installerKernel := writeArtifact(t, workDir, "vmlinuz", "kernel")
	installerInitrd := writeArtifact(t, workDir, "initrd", "initrd")
	installerISO := writeArtifact(t, workDir, "installer.iso", "installer iso")
	runtimeUKI := writeArtifact(t, workDir, "runtime.efi", "runtime uki")
	runtimeRoot := writeArtifact(t, workDir, "runtime-root.squashfs", "runtime root")
	katlosImage := writeArtifact(t, workDir, "katlos image.squashfs", "katlos image")
	writeChecksum(t, runtimeUKI)
	writeChecksum(t, runtimeRoot)
	writeChecksum(t, katlosImage)
	writeJSONFile(t, runtimeUKI+".json", map[string]any{"kind": "runtime-uki"})
	writeJSONFile(t, runtimeRoot+".json", map[string]any{"kind": "runtime-root"})
	writeJSONFile(t, katlosImage+".json", map[string]any{"kind": "katlos-image"})

	index := filepath.Join(workDir, "artifacts.json")
	cmd := exec.Command("go", "run", "./cmd/katl-mkosi-artifacts", "write", index)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"KATL_VERSION=0.1.\"quoted\"\\version",
		"KATL_ARCHITECTURE=x86_64",
		"KATL_INSTALLER_INTERFACE=katl-installer-test",
		"KATL_INSTALLER_UKI="+installerUKI,
		"KATL_INSTALLER_KERNEL="+installerKernel,
		"KATL_INSTALLER_INITRD="+installerInitrd,
		"KATL_INSTALLER_ISO="+installerISO,
		"KATL_RUNTIME_UKI="+runtimeUKI,
		"KATL_RUNTIME_UKI_METADATA="+runtimeUKI+".json",
		"KATL_RUNTIME_UKI_CHECKSUM="+runtimeUKI+".sha256",
		"KATL_RUNTIME_ARTIFACT="+runtimeRoot,
		"KATL_RUNTIME_METADATA="+runtimeRoot+".json",
		"KATL_RUNTIME_CHECKSUM="+runtimeRoot+".sha256",
		"KATL_KATLOS_IMAGE="+katlosImage,
		"KATL_KATLOS_IMAGE_METADATA="+katlosImage+".json",
		"KATL_KATLOS_IMAGE_CHECKSUM="+katlosImage+".sha256",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("katl-mkosi-artifacts write failed: %v\n%s", err, output)
	}

	var artifactIndex struct {
		SchemaVersion int `json:"schemaVersion"`
		Artifacts     []struct {
			Kind         string `json:"kind"`
			Path         string `json:"path"`
			MetadataPath string `json:"metadataPath"`
			ChecksumPath string `json:"checksumPath"`
			SHA256       string `json:"sha256"`
		} `json:"artifacts"`
	}
	readJSONFile(t, index, &artifactIndex)
	if artifactIndex.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", artifactIndex.SchemaVersion)
	}
	if len(artifactIndex.Artifacts) != 7 {
		t.Fatalf("artifact count = %d, want 7: %#v", len(artifactIndex.Artifacts), artifactIndex.Artifacts)
	}
	kinds := make(map[string]bool)
	for _, artifact := range artifactIndex.Artifacts {
		kinds[artifact.Kind] = true
		if artifact.Path == "" || artifact.SHA256 == "" {
			t.Fatalf("artifact has empty path or sha: %#v", artifact)
		}
		if artifact.Kind != "katlos-install-image" && artifact.MetadataPath == "" {
			t.Fatalf("artifact missing metadata path: %#v", artifact)
		}
		if artifact.ChecksumPath == "" {
			t.Fatalf("artifact missing checksum path: %#v", artifact)
		}
	}
	for _, kind := range []string{"installer-uki", "installer-kernel", "installer-initrd", "installer-iso", "runtime-uki", "runtime-root", "katlos-install-image"} {
		if !kinds[kind] {
			t.Fatalf("artifact kind %q missing from %#v", kind, artifactIndex.Artifacts)
		}
	}

	var bootMetadata struct {
		Kind               string `json:"kind"`
		ArtifactRole       string `json:"artifactRole"`
		Version            string `json:"version"`
		InstallerInterface string `json:"installerInterface"`
	}
	readJSONFile(t, installerUKI+".json", &bootMetadata)
	if bootMetadata.Kind != "InstallerBootArtifact" || bootMetadata.ArtifactRole != "installer-uki" {
		t.Fatalf("installer UKI metadata = %#v", bootMetadata)
	}
	if bootMetadata.Version != `0.1."quoted"\version` {
		t.Fatalf("installer UKI version = %q", bootMetadata.Version)
	}
	if bootMetadata.InstallerInterface != "katl-installer-test" {
		t.Fatalf("installer interface = %q", bootMetadata.InstallerInterface)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	return root
}

func writeArtifact(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}

func writeChecksum(t *testing.T, path string) {
	t.Helper()
	sum := sha256.Sum256(mustReadFile(t, path))
	content := hex.EncodeToString(sum[:]) + "  " + filepath.Base(path) + "\n"
	if err := os.WriteFile(path+".sha256", []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s.sha256) error = %v", path, err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func readJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data := mustReadFile(t, path)
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v\n%s", path, err, data)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return data
}
