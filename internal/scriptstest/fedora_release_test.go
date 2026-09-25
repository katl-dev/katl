package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuilderRelease(t *testing.T) {
	repo := repoRoot(t)
	fixture := t.TempDir()
	scripts := filepath.Join(fixture, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(filepath.Join(repo, "scripts", "fedora-release"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "fedora-release"), helper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "mkosi.conf"), []byte("[Distribution]\nRelease=99\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	argsPath := filepath.Join(fixture, "container-args")
	writeFakeExecutable(t, bin, "podman", `case "$1" in
 image) exit 1 ;;
 build) printf '%s\n' "$@" > "$KATL_FAKE_PODMAN_ARGS" ;;
 run) printf 'mkosi 26\n' ;;
 *) exit 1 ;;
esac
`)
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "builder-version")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KATL_REPO_ROOT="+fixture, "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_IMAGE=", "KATL_FAKE_PODMAN_ARGS="+argsPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("builder-version: %v\n%s", err, output)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--build-arg\nFEDORA_RELEASE=99\n", "--tag\nlocalhost/katl-mkosi-builder:fedora-99-go-squashfs\n"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("builder does not use selected release: %s", args)
		}
	}
}
