package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeInitrdPackagingPreservesEarlyMicrocode(t *testing.T) {
	wrapper, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "mkosi"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(wrapper)
	splitAt := strings.Index(text, "mkosi_artifacts split-initrd")
	repackAt := strings.Index(text, "cpio --null --create --append --format=newc")
	joinAt := strings.Index(text, "mkosi_artifacts join-initrd")
	if splitAt < 0 || repackAt < 0 || joinAt < 0 || splitAt >= repackAt || repackAt >= joinAt {
		t.Fatal("runtime initrd packaging must preserve early microcode around normal initramfs changes")
	}
	if !strings.Contains(text, `"$root/usr/lib64/libseccomp.so.2"`) {
		t.Fatal("runtime initrd packaging must include libseccomp for the runtime switch-root helper")
	}
}

func TestRuntimeBootInputsArePublishedBeforeInitrdPackaging(t *testing.T) {
	wrapper, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "mkosi"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(wrapper)
	publish := `cp --reflink=auto "$kernel_source" "$runtime_kernel"`
	packageInitrd := `mkosi_artifacts split-initrd`
	publishAt := strings.Index(text, publish)
	packageAt := strings.Index(text, packageInitrd)
	if publishAt < 0 || packageAt < 0 || publishAt >= packageAt {
		t.Fatalf("runtime boot inputs must be copied to durable artifacts before initrd packaging")
	}
	for _, want := range []string{
		`cp --reflink=auto "$initrd_source" "$runtime_initrd"`,
		`--linux "$runtime_kernel"`,
		`--initrd "$runtime_initrd"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("runtime UKI packaging does not use published boot input %q", want)
		}
	}
}

func TestRuntimeKubernetesSysctlsSupportCNIs(t *testing.T) {
	config, err := os.ReadFile(filepath.Join(repoRoot(t), "mkosi.profiles", "runtime", "mkosi.extra", "usr", "lib", "sysctl.d", "50-katl-kubernetes.conf"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	for _, want := range []string{
		"net.ipv4.ip_forward=1",
		"net.ipv4.conf.all.rp_filter=0",
		"net.ipv4.conf.default.rp_filter=0",
		"net.ipv4.conf.lxc*.rp_filter=0",
		"net.ipv4.conf.cilium_*.rp_filter=0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runtime Kubernetes sysctls missing %q", want)
		}
	}
}

func TestRuntimeNetworkdLeavesKubernetesRoutesAlone(t *testing.T) {
	config, err := os.ReadFile(filepath.Join(repoRoot(t), "mkosi.profiles", "runtime", "mkosi.extra", "usr", "lib", "systemd", "networkd.conf.d", "10-katl-kubernetes.conf"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	for _, want := range []string{
		"[Network]",
		"ManageForeignRoutes=no",
		"ManageForeignRoutingPolicyRules=no",
		"ManageForeignNextHops=no",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runtime networkd policy missing %q", want)
		}
	}
}

func TestRuntimeBuildExcludesVMTestSupportByDefault(t *testing.T) {
	repo := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	writeFakeExecutable(t, bin, "go", `
output=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    output="$2"
    break
  fi
  shift
done
[[ -n "$output" ]] || exit 2
mkdir -p "$(dirname "$output")"
printf 'fake binary\n' > "$output"
`)

	production := filepath.Join(t.TempDir(), "production")
	runRuntimeBuild(t, repo, bin, production, "0")
	for _, path := range vmtestRuntimePaths(production) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("production runtime contains VM-test path %s: %v", path, err)
		}
	}
	assertRuntimeServicePolicy(t, production)

	instrumented := filepath.Join(t.TempDir(), "instrumented")
	runRuntimeBuild(t, repo, bin, instrumented, "1")
	for _, path := range vmtestRuntimePaths(instrumented) {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("instrumented runtime missing VM-test path %s: %v", path, err)
		}
	}
}

func assertRuntimeServicePolicy(t *testing.T, root string) {
	t.Helper()
	for _, unit := range []string{
		"authselect-apply-changes.service",
		"fips-crypto-policy-overlay.service",
		"systemd-homed-activate.service",
		"systemd-homed.service",
		"systemd-oomd.service",
		"systemd-oomd.socket",
		"systemd-preset-all.service",
		"systemd-tpm2-clear.service",
	} {
		path := filepath.Join(root, "etc", "systemd", "system", unit)
		target, err := os.Readlink(path)
		if err != nil {
			t.Errorf("runtime mask %s: %v", unit, err)
			continue
		}
		if target != "/dev/null" {
			t.Errorf("runtime mask %s = %q, want /dev/null", unit, target)
		}
	}
	getty := filepath.Join(root, "usr", "lib", "systemd", "system", "getty.target.wants", "getty@tty2.service")
	if _, err := os.Lstat(getty); !os.IsNotExist(err) {
		t.Errorf("runtime includes unsupported tty2 getty %s: %v", getty, err)
	}
}

func runRuntimeBuild(t *testing.T, repo, bin, dest, support string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(repo, "mkosi.profiles", "runtime", "mkosi.build"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUILDDIR="+t.TempDir(),
		"DESTDIR="+dest,
		"SRCDIR="+repo,
		"KATL_BUILD_COMMIT=test",
		"KATL_VERSION=0.0.0-test",
		"KATL_VMTEST_IMAGE_SUPPORT="+support,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runtime build support=%s failed: %v\n%s", support, err, output)
	}
}

func vmtestRuntimePaths(root string) []string {
	return []string{
		filepath.Join(root, "usr", "lib", "katl", "vmtest"),
		filepath.Join(root, "usr", "lib", "katl", "vmtest", "katl-vmtest-agent"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "katl-vmtest-agent.service"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "katl-vmtest-debug-shell.service"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "multi-user.target.wants", "katl-vmtest-agent.service"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "multi-user.target.wants", "katl-vmtest-debug-shell.service"),
	}
}
