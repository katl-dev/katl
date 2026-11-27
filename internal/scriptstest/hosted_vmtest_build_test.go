package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHostedVMInputBuild(t *testing.T) {
	repo := repoRoot(t)
	var serialManifest string
	for _, mode := range []string{"serial", "parallel", "installer failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			scripts := filepath.Join(dir, "scripts")
			hosted := filepath.Join(dir, ".github", "scripts")
			bin := filepath.Join(dir, "bin")
			for _, path := range []string{scripts, hosted, bin, filepath.Join(dir, "_build/mkosi/package-cache/cache"), filepath.Join(dir, "_build/hosted-evidence")} {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeTemporaryFile(t, filepath.Join(dir, "_build/mkosi/package-cache/cache/seed.rpm"), "original")
			writeTemporaryFile(t, filepath.Join(dir, "_build/hosted-evidence/source.txt"), "source\n")
			writeTemporaryFile(t, filepath.Join(dir, "_build/hosted-evidence/mkosi-version.txt"), "builder\n")

			writeFakeExecutable(t, hosted, "hosted-vmtest-measure", "shift\nexec \"$@\"\n")
			writeFakeExecutable(t, hosted, "hosted-vmtest-run", "test -f _build/mkosi/katl-runtime.efi.json\n")
			writeFakeExecutable(t, bin, "go", "test -f _build/mkosi/katl-kubernetes.raw\n")
			writeFakeExecutable(t, bin, "docker", `case "${!#}" in
    scripts/package-installer-iso) exec scripts/package-installer-iso ;;
    scripts/check-kubernetes-sysext) test -f _build/mkosi/katl-kubernetes.raw ;;
    *) exit 90 ;;
esac
`)
			writeFakeExecutable(t, scripts, "mkosi", `case "$1" in
    build-runtime)
        if [[ "$MODE" != serial ]]; then
            # A serial implementation cannot get past this barrier.
            for ((i=0; i<200; i++)); do
                [[ ! -e _build/installer-started ]] || break
                sleep .02
            done
            test -f _build/installer-started
        fi
        test "$(cat _build/mkosi/package-cache/cache/seed.rpm)" = original
        printf runtime > _build/mkosi/katl-runtime.efi.json ;;
    build-installer)
        test "$KATL_MKOSI_BUILD_DIR" != "$PWD/_build/mkosi"
        test "$KATL_GO_MOD_CACHE" != "$PWD/_build/go-mod"
        test "$KATL_GO_BUILD_CACHE" != "$PWD/_build/go-cache"
        test "$(cat "$KATL_MKOSI_BUILD_DIR/package-cache/cache/seed.rpm")" = original
        printf modified > "$KATL_MKOSI_BUILD_DIR/package-cache/cache/seed.rpm"
        touch _build/installer-started
        if [[ "$MODE" == 'installer failure' ]]; then
            echo "installer failed" >&2
            exit 43
        fi
        printf boot > "$KATL_MKOSI_BUILD_DIR/katl-installer.efi" ;;
    build-installer-iso)
        printf boot > _build/mkosi/katl-installer.efi
        scripts/build-katlos-install-image
        scripts/package-installer-iso ;;
    build-katlos-upgrade-image) exec scripts/build-katlos-install-image --upgrade ;;
    *) exit 91 ;;
esac
`)
			writeFakeExecutable(t, scripts, "build-endpoint-advertiser-sysext", `
test -f _build/mkosi/katl-runtime.efi.json
touch _build/endpoint-built
`)
			writeFakeExecutable(t, scripts, "build-katlos-install-image", `
if [[ "$MODE" != serial ]]; then test -f _build/endpoint-built; fi
if [[ "${1:-}" == --upgrade ]]; then
    printf upgrade > _build/mkosi/katlos-upgrade-test.squashfs
else
    printf install > _build/mkosi/katlos-install-test.squashfs
fi
`)
			writeFakeExecutable(t, scripts, "build-kubernetes-sysext", `
test -f _build/mkosi/katl-runtime.efi.json
printf sysext > _build/mkosi/katl-kubernetes.raw
`)
			writeFakeExecutable(t, scripts, "package-installer-iso", `
cat _build/mkosi/katl-installer.efi _build/mkosi/katlos-install-test.squashfs > _build/mkosi/katl-installer.iso
echo index > _build/mkosi/artifacts.json
`)

			var args []string
			if mode == "serial" {
				args = append(args, "--serial")
			}
			build := exec.Command(filepath.Join(repo, ".github/scripts/hosted-vmtest-build"), args...)
			build.Dir = dir
			build.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
				"GITHUB_WORKSPACE="+dir, "GITHUB_EVENT_NAME=merge_group", "KATL_MKOSI_IMAGE=builder", "MODE="+mode)
			output, err := runInGroup(t, build, scriptTimeout)
			if mode == "installer failure" {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 {
					t.Fatalf("build error = %v, want failed producer\n%s", err, output)
				}
				if _, err := os.Stat(filepath.Join(dir, "_build/mkosi/artifacts.json")); !os.IsNotExist(err) {
					t.Fatalf("failed build produced an index: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("build: %v\n%s", err, output)
			}

			pack := exec.Command(filepath.Join(repo, ".github/scripts/hosted-vmtest-package"))
			pack.Dir = dir
			if output, err := pack.CombinedOutput(); err != nil {
				t.Fatalf("package: %v\n%s", err, output)
			}
			manifest := string(mustReadFile(t, filepath.Join(dir, "_build/hosted-inputs/inputs.sha256")))
			if mode == "serial" {
				serialManifest = manifest
			} else if manifest != serialManifest {
				t.Fatalf("lane input manifest =\n%s\nwant serial manifest\n%s", manifest, serialManifest)
			}
		})
	}
}
