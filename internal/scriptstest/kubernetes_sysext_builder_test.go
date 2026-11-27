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
	profile := filepath.Join(profileDir, "mkosi.conf")
	manifest := filepath.Join(profileDir, "kubernetes.env")
	runtimePackageDB := filepath.Join(buildDir, "katl-runtime-root", "usr", "lib", "sysimage", "rpm")
	for _, dir := range []string{filepath.Join(repo, "scripts"), profileDir, buildDir, runtimePackageDB} {
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
        ethtool
        socat
`
	writeTemporaryFile(t, profile, originalProfile)
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
cmp "$KATL_ORIGINAL_PROFILE" "$KATL_REPO_ROOT/mkosi.profiles/kubernetes-sysext/mkosi.conf"
while [[ $# -gt 0 ]]; do
    case "$1" in
        # The profile's Output names the extension-release file.
        --output) exit 64 ;;
        --output-directory) output_dir="$KATL_MKOSI_BUILD_DIR/${2##*/}"; shift ;;
        --sandbox-tree) tree="$KATL_MKOSI_BUILD_DIR/${2##*/}"; shift ;;
    esac
    shift
done
grep -Fx 'baseurl=https://packages.example/v1.36/rpm/' "$tree/etc/yum.repos.d/kubernetes.repo"
[[ ! -e "$KATL_REPO_ROOT/mkosi.profiles/kubernetes-sysext/mkosi.sandbox" ]]
printf 'sysext payload\n' > "$output_dir/katl-kubernetes.raw"
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

	writeTemporaryFile(t, filepath.Join(buildDir, "katl-kubernetes.raw"), "default payload\n")
	profileCopy := filepath.Join(repo, "original-profile")
	writeTemporaryFile(t, profileCopy, originalProfile)

	cmd := exec.Command(filepath.Join(repo, "scripts", "build-kubernetes-sysext"), "--output", "katl-kubernetes-upgrade", "--no-cache")
	cmd.Dir = repo
	cmd.Env = append(
		os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_REPO_ROOT="+repo,
		"KATL_ORIGINAL_PROFILE="+profileCopy,
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
		"kubeadm-0:1.36.1-1.x86_64",
		"kubelet-0:1.36.1-1.x86_64",
		"kubectl-0:1.36.1-1.x86_64",
		"cri-tools-0:1.36.0-1.x86_64",
	} {
		if !containsString(args, want) {
			t.Fatalf("mkosi args missing %q: %#v", want, args)
		}
	}
	if _, err := os.Stat(filepath.Join(buildDir, "katl-kubernetes-upgrade.raw.json")); err != nil {
		t.Fatalf("custom sysext output metadata: %v", err)
	}
	// A custom output leaves the existing default payload available to readers.
	if got := string(mustReadFile(t, filepath.Join(buildDir, "katl-kubernetes.raw"))); got != "default payload\n" {
		t.Fatalf("default sysext = %q, want it untouched by the custom output build", got)
	}
	if got := string(mustReadFile(t, filepath.Join(buildDir, "katl-kubernetes-upgrade.raw"))); got != "sysext payload\n" {
		t.Fatalf("custom sysext = %q, want the built payload", got)
	}
	if leftovers, err := filepath.Glob(filepath.Join(buildDir, ".kubernetes-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("private build trees left behind: %v, %v", leftovers, err)
	}
	if got := strings.Join(readLinesForScripts(t, goArgs), " "); !strings.Contains(got, "--artifact "+filepath.Join(buildDir, "katl-kubernetes-upgrade.raw")) {
		t.Fatalf("metadata command did not receive the explicit output: %s", got)
	} else if !strings.Contains(got, "run ./cmd/katl-kubernetes-metadata write-from-log") {
		t.Fatalf("metadata command still uses the shared artifact controller: %s", got)
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
