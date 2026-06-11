package scriptstest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildKatlOSInstallImageUsesGoMetadata(t *testing.T) {
	repo := repoRoot(t)
	workDir := testBuildDir(t, repo, "katlos image ")

	runtimeRoot := writeArtifact(t, workDir, "katl-runtime-root.squashfs", "runtime root")
	runtimeRootSHA := fileSHA256(t, runtimeRoot)
	writeJSONFile(t, runtimeRoot+".json", map[string]any{
		"name":             "runtime-root",
		"kind":             "runtime-root",
		"format":           "squashfs",
		"path":             filepath.Base(runtimeRoot),
		"sizeBytes":        int64(len("runtime root")),
		"sha256":           runtimeRootSHA,
		"compression":      "zstd",
		"generation":       "test-build",
		"architecture":     "x86_64",
		"runtimeInterface": "katl-runtime-1",
		"compatibleBoot": map[string]any{
			"kind":              "uki",
			"runtimeInterface":  "katl-runtime-1",
			"kernelCommandLine": []string{"rootfstype=squashfs", "ro"},
		},
	})

	runtimeUKI := writeArtifact(t, workDir, "katl-runtime.efi", "runtime uki")
	writeJSONFile(t, runtimeUKI+".json", map[string]any{
		"name":             "runtime-uki",
		"kind":             "runtime-uki",
		"format":           "uki",
		"path":             filepath.Base(runtimeUKI),
		"sizeBytes":        int64(len("runtime uki")),
		"sha256":           fileSHA256(t, runtimeUKI),
		"version":          "test-build",
		"architecture":     "x86_64",
		"runtimeInterface": "katl-runtime-1",
		"compatibleRuntime": map[string]any{
			"interface":      "katl-runtime-1",
			"artifactPath":   filepath.Base(runtimeRoot),
			"artifactSHA256": runtimeRootSHA,
		},
		"kernelVersion":     "6.12.0",
		"kernelCommandLine": []string{"rootfstype=squashfs", "ro"},
	})

	sysext := writeArtifact(t, workDir, "katl-kubernetes.raw", "kubernetes sysext")
	writeJSONFile(t, sysext+".json", map[string]any{
		"name":           "kubernetes",
		"kind":           "sysext",
		"format":         "sysext",
		"path":           filepath.Base(sysext),
		"sizeBytes":      int64(len("kubernetes sysext")),
		"sha256":         fileSHA256(t, sysext),
		"version":        "test-build",
		"payloadVersion": "v1.36.0",
		"architecture":   "x86_64",
		"sourceRepo": map[string]any{
			"id":      "kubernetes",
			"baseURL": "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			"minor":   "v1.36",
		},
		"packageVersions": map[string]any{
			"kubeadm":   "1.36.0-1",
			"kubelet":   "1.36.0-1",
			"kubectl":   "1.36.0-1",
			"cri-tools": "1.36.0-1",
		},
		"runtimeInterface": "katl-runtime-1",
		"compatibleRuntime": map[string]any{
			"interface":      "katl-runtime-1",
			"artifactPath":   filepath.Base(runtimeRoot),
			"artifactSHA256": runtimeRootSHA,
		},
	})

	fakeBin := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", fakeBin, err)
	}
	fakeMksquashfs := filepath.Join(fakeBin, "mksquashfs")
	if err := os.WriteFile(fakeMksquashfs, []byte("#!/usr/bin/env bash\nset -euo pipefail\nprintf 'fake katlos image from %s\\n' \"$1\" >\"$2\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", fakeMksquashfs, err)
	}

	output := filepath.Join(workDir, "katlos-install.squashfs")
	root := filepath.Join(workDir, "root")
	cmd := exec.Command(filepath.Join(repo, "scripts", "build-katlos-install-image"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_VERSION=0.1.0",
		"KATL_ARCHITECTURE=x86_64",
		"KATL_KATLOS_IMAGE="+output,
		"KATL_KATLOS_IMAGE_ROOT="+root,
		"KATL_RUNTIME_ARTIFACT="+runtimeRoot,
		"KATL_RUNTIME_METADATA="+runtimeRoot+".json",
		"KATL_RUNTIME_UKI="+runtimeUKI,
		"KATL_RUNTIME_UKI_METADATA="+runtimeUKI+".json",
		"KATL_KUBERNETES_SYSEXT="+sysext,
		"KATL_KUBERNETES_SYSEXT_METADATA="+sysext+".json",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-katlos-install-image failed: %v\n%s", err, out)
	}

	var imageIndex struct {
		Kind       string `json:"kind"`
		Components []struct {
			Role   string `json:"role"`
			SHA256 string `json:"sha256"`
		} `json:"components"`
	}
	readJSONFile(t, filepath.Join(root, "katlos", "image.json"), &imageIndex)
	if imageIndex.Kind != "KatlOSImage" || len(imageIndex.Components) != 3 {
		t.Fatalf("image index = %#v", imageIndex)
	}
	assertFileEquals(t, filepath.Join(root, "components", "metadata", "runtime-root.sha256"), runtimeRootSHA+"  ../runtime/root.squashfs\n")

	var artifactMetadata struct {
		Kind              string `json:"kind"`
		EmbeddedIndexPath string `json:"embeddedIndexPath"`
		SHA256            string `json:"sha256"`
	}
	readJSONFile(t, output+".json", &artifactMetadata)
	if artifactMetadata.Kind != "KatlOSImageArtifact" || artifactMetadata.EmbeddedIndexPath != "katlos/image.json" || artifactMetadata.SHA256 == "" {
		t.Fatalf("artifact metadata = %#v", artifactMetadata)
	}
	assertFileEquals(t, output+".sha256", artifactMetadata.SHA256+"  "+filepath.Base(output)+"\n")
}

func testBuildDir(t *testing.T, repo, prefix string) string {
	t.Helper()
	buildDir := filepath.Join(repo, "_build", "mkosi")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", buildDir, err)
	}
	dir, err := os.MkdirTemp(buildDir, prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(%s) error = %v", buildDir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("RemoveAll(%s) error = %v", dir, err)
		}
	})
	return dir
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(mustReadFile(t, path))
	return hex.EncodeToString(sum[:])
}

func assertFileEquals(t *testing.T, path, want string) {
	t.Helper()
	got := string(mustReadFile(t, path))
	if got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
