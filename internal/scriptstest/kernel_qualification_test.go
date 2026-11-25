package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestDriverQualificationProvidesInstallImage(t *testing.T) {
	workflow, _ := readWorkflow(t, "kernel-qualification.yml")
	var script string
	for _, step := range workflow.Jobs["drivers"].Steps {
		if strings.Contains(step.Run, "TestInstalledRuntimeDRBDReplication") {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing DRBD qualification step")
	}

	dir := t.TempDir()
	buildDir := filepath.Join(dir, "_build/mkosi")
	scripts := filepath.Join(dir, "scripts")
	hostedScripts := filepath.Join(dir, ".github/scripts")
	for _, subdir := range []string{buildDir, scripts, hostedScripts} {
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"go.mod", "go.sum", "cmd", "internal"} {
		if err := os.Symlink(filepath.Join(repoRoot(t), name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"katl-runtime.efi", "katl-runtime-root.squashfs"} {
		path := writeArtifact(t, buildDir, name, name)
		writeChecksum(t, path)
		writeJSONFile(t, path+".json", map[string]string{})
	}
	for _, name := range []string{"katl-installer.efi", "katl-installer.vmlinuz", "katl-installer.initrd", "katlos-upgrade-driver-test-x86_64.squashfs"} {
		writeArtifact(t, buildDir, name, name)
	}

	// The install image build leaves a runtime-only index alongside the image.
	writeFakeExecutable(t, scripts, "mkosi", `if [[ "$1" == build-katlos-install-image ]]; then
    cd "$KATL_MKOSI_BUILD_DIR"
    image="katlos-install-$KATL_VERSION-$KATL_ARCHITECTURE.squashfs"
    printf '%s' "$image" > "$image"
    printf '{}\n' > "$image.json"
    sha256sum "$image" > "$image.sha256"
    cd "$GITHUB_WORKSPACE"
    go run ./cmd/katl-mkosi-artifacts write-runtime-index
fi
`)
	writeFakeExecutable(t, hostedScripts, "hosted-vmtest-measure", `shift
exec "$@"
`)
	writeFakeExecutable(t, hostedScripts, "hosted-vmtest-run", `image=$(jq -er '.artifacts[] | select(.kind == "katlos-install-image") | .path' "$KATL_MKOSI_BUILD_DIR/artifacts.json")
cat "$image" > image-used-by-runner
`)

	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(os.Environ(), dir), "GITHUB_WORKSPACE="+dir,
		"GOWORK=off", "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_BUILD_COMMIT=driver-test",
		"KATL_VERSION=driver-test", "KATL_ARCHITECTURE=x86_64")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("DRBD qualification failed to hand off the install image: %v\n%s", err, output)
	}
	if got := string(mustReadFile(t, filepath.Join(dir, "image-used-by-runner"))); got != "katlos-install-driver-test-x86_64.squashfs" {
		t.Fatalf("runner received %q, want the build's install image", got)
	}
}
