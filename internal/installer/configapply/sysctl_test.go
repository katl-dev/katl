package configapply

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

// sysctlHostRoot returns a host root with a running kernel's parameters and
// vendor sysctl.d files resembling KatlOS.
func sysctlHostRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for key, mode := range map[string]os.FileMode{
		"kernel.modules_disabled":     0o644,
		"kernel.sysrq":                0o644,
		"net.core.somaxconn":          0o644,
		"net.ipv4.conf.all.rp_filter": 0o644,
		"net.ipv4.ip_forward":         0o644,
		"vm.drop_caches":              0o200,
		"vm.swappiness":               0o644,
	} {
		writeTestFile(t, filepath.Join(root, "proc/sys", strings.ReplaceAll(key, ".", "/")), "0\n", mode)
	}
	for filePath, content := range map[string]string{
		"/usr/lib/sysctl.d/50-default.conf":         "net.ipv4.conf.*.rp_filter = 2\n-net.ipv4.conf.all.rp_filter\nkernel.sysrq = 16\n",
		"/usr/lib/sysctl.d/50-katl-kubernetes.conf": "net.ipv4.ip_forward=1\n",
		"/usr/lib/sysctl.d/99-vendor.conf":          "net.core.somaxconn = 4096\n",
	} {
		writeTestFile(t, filepath.Join(root, filePath), content, 0o644)
	}
	return root
}

func writeTestFile(t *testing.T, filePath, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filePath, mode); err != nil {
		t.Fatal(err)
	}
}

// sysctlConfiguration places each managed file in its own set.
func sysctlConfiguration(files map[string]string) manifest.HostConfiguration {
	config := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{}}
	for filePath, content := range files {
		content := content
		config.Sets[strings.TrimSuffix(path.Base(filePath), ".conf")] = manifest.HostConfigurationSet{
			State: manifest.HostConfigurationPresent,
			Files: []manifest.HostConfigurationFile{{Path: filePath, Content: &content, Mode: 0o644}},
		}
	}
	return config
}

// A managed file carried by source has no inline content; its active copy
// still takes part in precedence.
func TestPlanHostConfigurationResolvesSourceBackedSysctlFile(t *testing.T) {
	root := sysctlHostRoot(t)
	writeTestFile(t, filepath.Join(root, "/etc/sysctl.d/99-memory.conf"), "vm.swappiness = 30\n", 0o644)
	sourced := manifest.HostConfigurationSet{Files: []manifest.HostConfigurationFile{{Path: "/etc/sysctl.d/99-memory.conf", Source: "files/99-memory.conf"}}}
	current := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{"memory": sourced}}
	desired := sysctlConfiguration(map[string]string{"/etc/sysctl.d/95-demo.conf": "vm.swappiness = 20\n"})
	desired.Sets["memory"] = sourced

	plan := planHostConfigurationChange(current, desired, root)

	if plan.Live || !strings.Contains(plan.Message, "overridden by /etc/sysctl.d/99-memory.conf") {
		t.Fatalf("plan live=%t message=%q, want the source-backed file to override the addition", plan.Live, plan.Message)
	}
}

func TestEffectiveSysctlFollowsSystemdPrecedence(t *testing.T) {
	root := t.TempDir()
	for filePath, content := range map[string]string{
		"/usr/lib/sysctl.d/10-base.conf":       "vm.swappiness = 1\n",
		"/usr/lib/sysctl.d/30-masked.conf":     "vm.swappiness = 2\n",
		"/run/sysctl.d/30-masked.conf":         "# replaces the vendor file\n",
		"/usr/local/lib/sysctl.d/40-glob.conf": "vm.* = 9\n",
		"/usr/lib/sysctl.d/60-slash.conf":      "vm/swappiness = 6\n",
		"/usr/lib/sysctl.d/70-other.txt":       "vm.swappiness = 7\n",
		"/usr/lib/sysctl.d/.80-hidden.conf":    "vm.swappiness = 8\n",
	} {
		writeTestFile(t, filepath.Join(root, filePath), content, 0o644)
	}

	files, err := effectiveSysctlFiles(root, map[string]string{"/etc/sysctl.d/50-managed.conf": "-vm.swappiness = 5\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range files {
		if strings.HasPrefix(path.Base(file.Path), ".") {
			t.Fatalf("files include hidden %s, which systemd-sysctl ignores", file.Path)
		}
	}

	got, ok := effectiveSysctl(files, "vm.swappiness")
	if !ok || got != (sysctlAssignment{Path: "/usr/lib/sysctl.d/60-slash.conf", Value: "6"}) {
		t.Fatalf("effective = %#v, %t; want the latest explicit assignment by file name", got, ok)
	}
	files = files[:len(files)-1]
	if got, _ := effectiveSysctl(files, "vm.swappiness"); got.Value != "5" {
		t.Fatalf("effective without 60-slash = %#v, want the managed value over globs and replaced files", got)
	}
}
