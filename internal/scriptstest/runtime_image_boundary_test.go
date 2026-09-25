package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeSysctlOwnership(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), "mkosi.profiles", "runtime", "mkosi.extra", "usr", "lib", "sysctl.d", "*.conf"))
	if err != nil {
		t.Fatal(err)
	}

	forwarding := false
	for _, path := range paths {
		config, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(config), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok || strings.HasPrefix(key, "#") {
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if key == "net.ipv4.ip_forward" {
				forwarding = value == "1"
			}
			if strings.HasSuffix(key, ".rp_filter") {
				t.Errorf("runtime sets CNI-owned reverse-path filtering: %s", key)
			}
		}
	}
	if !forwarding {
		t.Fatal("runtime must enable Kubernetes IPv4 forwarding")
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

func TestInstallerDHCPOnlyMatchesUnkindedEthernetLinks(t *testing.T) {
	config, err := os.ReadFile(filepath.Join(repoRoot(t), "mkosi.profiles", "installer-image", "mkosi.extra", "usr", "lib", "systemd", "network", "80-katl-installer-dhcp.network"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	for _, want := range []string{
		"[Match]",
		"Type=ether",
		"Kind=!*",
		"DHCP=yes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("installer DHCP fallback missing %q", want)
		}
	}
}

func TestRuntimeRootShellUsesKubeadmAdminContext(t *testing.T) {
	profile, err := os.ReadFile(filepath.Join(repoRoot(t), "mkosi.profiles", "runtime", "mkosi.extra", "etc", "profile.d", "katl-kubernetes.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(profile)
	for _, want := range []string{
		`[ -z "${KUBECONFIG+x}" ]`,
		`[ "${EUID:-$(id -u)}" -eq 0 ]`,
		`[ -r /etc/kubernetes/admin.conf ]`,
		`export KUBECONFIG=/etc/kubernetes/admin.conf`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runtime Kubernetes profile missing %q", want)
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
if [[ "${1:-}" == run && "${3:-}" == export-kernel-inputs ]]; then
  exit 0
fi
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
	machineID, err := os.ReadFile(filepath.Join(root, "etc", "machine-id"))
	if err != nil || len(machineID) != 0 {
		t.Errorf("runtime machine-id = %q, %v; want empty file to suppress first-boot presets", machineID, err)
	}
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
	cmd.Env = append(
		os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUILDDIR="+t.TempDir(),
		"BUILDROOT="+t.TempDir(),
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
