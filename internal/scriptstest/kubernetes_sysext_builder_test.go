package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubernetesSysextBuilderOwnsProfileAndOutputSelection(t *testing.T) {
	realRepo := repoRoot(t)
	repo := t.TempDir()
	buildDir := filepath.Join(repo, "_build", "mkosi")
	profileDir := filepath.Join(repo, "mkosi.profiles", "kubernetes-sysext")
	repoFile := filepath.Join(profileDir, "mkosi.sandbox", "etc", "yum.repos.d", "kubernetes.repo")
	profile := filepath.Join(profileDir, "mkosi.conf")
	manifest := filepath.Join(profileDir, "kubernetes.env")
	runtimePackageDB := filepath.Join(buildDir, "katl-runtime-root", "usr", "lib", "sysimage", "rpm")
	for _, dir := range []string{filepath.Join(repo, "scripts"), filepath.Dir(repoFile), buildDir, runtimePackageDB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	builder, err := os.ReadFile(filepath.Join(realRepo, "scripts", "build-kubernetes-sysext"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "build-kubernetes-sysext"), builder, 0o755); err != nil {
		t.Fatal(err)
	}
	originalProfile := `[Output]
Output=katl-kubernetes

[Content]
Packages=
        kubeadm-old.x86_64
        kubelet-old.x86_64
        kubectl-old.x86_64
        cri-tools-old.x86_64
        ethtool
        socat
`
	originalRepo := "[old]\nbaseurl=https://old.invalid/\n"
	writeTemporaryFile(t, profile, originalProfile)
	writeTemporaryFile(t, repoFile, originalRepo)
	writeTemporaryFile(t, manifest, `KATL_KUBERNETES_MINOR=v1.36
KATL_KUBERNETES_REPO_ID=kubernetes
KATL_KUBERNETES_REPO_BASEURL=https://packages.example/v1.36/rpm/
KATL_KUBERNETES_REPO_GPGKEY=https://packages.example/v1.36/rpm/key
KATL_KUBERNETES_PAYLOAD_VERSION=v1.36.1
KATL_KUBERNETES_KUBEADM_VERSION=0:1.36.1-1
KATL_KUBERNETES_KUBELET_VERSION=0:1.36.1-1
KATL_KUBERNETES_KUBECTL_VERSION=0:1.36.1-1
KATL_KUBERNETES_CRITOOLS_VERSION=0:1.36.0-1
KATL_KUBERNETES_ETHTOOL_VERSION=
KATL_KUBERNETES_SOCAT_VERSION=
`)
	writeTemporaryFile(t, filepath.Join(buildDir, "katl-runtime-root.squashfs"), "runtime")
	writeTemporaryFile(t, filepath.Join(buildDir, "katl-runtime-root.squashfs.json"), `{"sha256":"runtime"}`)

	bin := filepath.Join(repo, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mkosiArgs := filepath.Join(repo, "mkosi-args.txt")
	goArgs := filepath.Join(repo, "go-args.txt")
	writeFakeExecutable(t, filepath.Join(repo, "scripts"), "mkosi", `printf '%s\n' "$@" > "$KATL_FAKE_MKOSI_ARGS"
printf 'sysext payload\n' > "$KATL_MKOSI_BUILD_DIR/katl-kubernetes.raw"
printf 'kubeadm x 0:1.36.1-1 kubernetes\n'
printf 'kubelet x 0:1.36.1-1 kubernetes\n'
printf 'kubectl x 0:1.36.1-1 kubernetes\n'
printf 'cri-tools x 0:1.36.0-1 kubernetes\n'
printf 'ethtool x 1:1.0 fedora\n'
printf 'socat x 1:1.0 fedora\n'
`)
	writeFakeExecutable(t, bin, "go", `printf '%s\n' "$@" > "$KATL_FAKE_GO_ARGS"
artifact=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "--artifact" ]]; then
    artifact="$2"
    break
  fi
  shift
done
[[ -n "$artifact" ]]
digest="$(sha256sum "$artifact")"
digest="${digest%% *}"
size="$(stat -c %s "$artifact")"
printf '{"sha256":"%s","sizeBytes":%s}\n' "$digest" "$size" > "$artifact.json"
(
  cd "$(dirname "$artifact")"
  sha256sum "$(basename "$artifact")" > "$(basename "$artifact").sha256"
)
`)

	cmd := exec.Command(filepath.Join(repo, "scripts", "build-kubernetes-sysext"), "--output", "katl-kubernetes-upgrade", "--no-cache")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_REPO_ROOT="+repo,
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_FAKE_MKOSI_ARGS="+mkosiArgs,
		"KATL_FAKE_GO_ARGS="+goArgs,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-kubernetes-sysext failed: %v\n%s", err, output)
	}

	args := readLinesForScripts(t, mkosiArgs)
	for _, want := range []string{
		"--profile",
		"kubernetes-sysext",
		"-f",
		"build",
		"KATL_KUBERNETES_PAYLOAD_VERSION=v1.36.1",
		"KATL_KUBERNETES_KUBEADM_VERSION=0:1.36.1-1",
	} {
		if !containsString(args, want) {
			t.Fatalf("mkosi args missing %q: %#v", want, args)
		}
	}
	if _, err := os.Stat(filepath.Join(buildDir, "katl-kubernetes-upgrade.raw.json")); err != nil {
		t.Fatalf("custom sysext output metadata: %v", err)
	}
	if got := string(mustReadFile(t, profile)); got != originalProfile {
		t.Fatalf("profile was not restored:\n%s", got)
	}
	if got := string(mustReadFile(t, repoFile)); got != originalRepo {
		t.Fatalf("repository file was not restored:\n%s", got)
	}
	if got := strings.Join(readLinesForScripts(t, goArgs), " "); !strings.Contains(got, "--artifact "+filepath.Join(buildDir, "katl-kubernetes-upgrade.raw")) {
		t.Fatalf("metadata command did not receive the explicit output: %s", got)
	}

	if err := os.RemoveAll(runtimePackageDB); err != nil {
		t.Fatalf("remove runtime package database: %v", err)
	}
	missingDB := exec.Command(filepath.Join(repo, "scripts", "build-kubernetes-sysext"), "--output", "katl-kubernetes-upgrade", "--no-cache")
	missingDB.Dir = repo
	missingDB.Env = cmd.Env
	output, err = missingDB.CombinedOutput()
	if err == nil {
		t.Fatalf("build-kubernetes-sysext without the runtime package database unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "rebuild the runtime with scripts/mkosi build-runtime") {
		t.Fatalf("missing package database error is not actionable:\n%s", output)
	}
}
