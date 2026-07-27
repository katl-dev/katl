package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/artifact"
)

func TestWriteFromLog(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	runtimeRoot := writeFile(t, root, "katl-runtime-root.squashfs", "runtime root")
	runtimeDigest := fileDigest(t, runtimeRoot)
	writeFile(t, root, "katl-runtime-root.squashfs.json", `{"sha256":"`+runtimeDigest+`"}`)
	sysext := writeFile(t, root, "katl-kubernetes.raw", "kubernetes sysext")
	logPath := writeFile(t, root, "mkosi.log", strings.Join([]string{
		"kubeadm x86_64 1.36.0-1 kubernetes installed",
		"kubelet x86_64 1.36.0-1 kubernetes installed",
		"kubectl x86_64 1.36.0-1 kubernetes installed",
		"cri-tools x86_64 1.36.0-1 kubernetes installed",
		"ethtool x86_64 2:7.0-1.fc44 fedora installed",
		"socat x86_64 0:1.8.1.1-1.fc44 updates installed",
	}, "\n"))

	var stdout bytes.Buffer
	err = run([]string{
		"write-from-log",
		"--artifact", sysext,
		"--log", logPath,
		"--runtime-artifact", runtimeRoot,
		"--runtime-metadata", runtimeRoot + ".json",
		"--repo-id", "kubernetes",
		"--repo-base-url", "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
		"--repo-minor", "v1.36",
		"--expected-payload-version", "v1.36.0",
		"--expected-kubeadm-version", "1.36.0-1",
	}, &stdout, &bytes.Buffer{}, []string{
		"KATL_BUILD_COMMIT=test-build",
		"KATL_ARCHITECTURE=x86_64",
		"SOURCE_DATE_EPOCH=0",
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := artifact.ReadLocal(sysext + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PayloadVersion != "v1.36.0" || metadata.Version != "test-build" || metadata.Architecture != "x86_64" {
		t.Fatalf("metadata identity = %#v", metadata)
	}
	if metadata.PackageVersions["kubeadm"] != "1.36.0-1" ||
		metadata.PackageVersions["ethtool"] != "2:7.0-1.fc44" ||
		metadata.PackageVersions["socat"] != "0:1.8.1.1-1.fc44" {
		t.Fatalf("package versions = %#v", metadata.PackageVersions)
	}
	if metadata.CompatibleRuntime == nil ||
		metadata.CompatibleRuntime.ArtifactPath != filepath.Base(runtimeRoot) ||
		metadata.CompatibleRuntime.ArtifactSHA256 != runtimeDigest {
		t.Fatalf("compatible runtime = %#v", metadata.CompatibleRuntime)
	}
	if metadata.Created != "1970-01-01T00:00:00Z" {
		t.Fatalf("created = %q", metadata.Created)
	}
	if strings.TrimSpace(stdout.String()) != fileDigest(t, sysext) {
		t.Fatalf("stdout digest = %q", stdout.String())
	}
}

func TestWriteFromLogRejectsWrongRepository(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	logPath := writeFile(t, root, "mkosi.log", strings.Join([]string{
		"kubeadm x86_64 1.36.0-1 wrong installed",
		"kubelet x86_64 1.36.0-1 kubernetes installed",
		"kubectl x86_64 1.36.0-1 kubernetes installed",
		"cri-tools x86_64 1.36.0-1 kubernetes installed",
		"ethtool x86_64 2:7.0-1.fc44 fedora installed",
		"socat x86_64 0:1.8.1.1-1.fc44 updates installed",
	}, "\n"))
	err = run([]string{
		"write-from-log",
		"--artifact", writeFile(t, root, "katl-kubernetes.raw", "payload"),
		"--log", logPath,
		"--repo-id", "kubernetes",
		"--repo-base-url", "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
		"--repo-minor", "v1.36",
	}, &bytes.Buffer{}, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "kubeadm resolved from wrong, want kubernetes") {
		t.Fatalf("error = %v", err)
	}
}

func writeFile(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
