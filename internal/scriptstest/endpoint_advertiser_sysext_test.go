package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpointAdvertiserSysextOwnsOnlyTheAPIVIP(t *testing.T) {
	repo := repoRoot(t)
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	appUnit := read("mkosi.profiles/endpoint-advertiser-sysext/katl-app-api-vip.service")
	for _, want := range []string{
		"ConditionPathExists=/etc/katl/apps/api-vip/config.yaml",
		"ConditionPathExists=/etc/katl/apps/api-vip/ownership-enabled",
		"ConditionPathExists=/etc/kubernetes/pki/ca.crt",
		"ExecStopPost=/usr/lib/katl/endpoint-advertiser/katl-endpoint-advertiser withdraw",
		"CapabilityBoundingSet=CAP_NET_ADMIN",
	} {
		if !strings.Contains(appUnit, want) {
			t.Fatalf("API VIP unit is missing %q", want)
		}
	}
	pathUnit := read("mkosi.profiles/endpoint-advertiser-sysext/katl-app-api-vip.path")
	for _, want := range []string{
		"ConditionPathExists=/etc/katl/apps/api-vip/ownership-enabled",
		"PathExists=/etc/kubernetes/pki/ca.crt",
		"Unit=katl-app-api-vip.service",
	} {
		if !strings.Contains(pathUnit, want) {
			t.Fatalf("API VIP path unit is missing %q", want)
		}
	}
	activationUnit := read("mkosi.profiles/runtime/katl-endpoint-activate.service")
	for _, want := range []string{
		"ConditionPathExists=/etc/katl/apps/api-vip/config.yaml",
		"ExecStart=/usr/bin/systemctl start katl-app-api-vip.service",
		"ExecStart=-/usr/bin/systemctl start katl-app-api-vip.path",
	} {
		if !strings.Contains(activationUnit, want) {
			t.Fatalf("endpoint activation unit is missing %q", want)
		}
	}
	build := read("mkosi.profiles/endpoint-advertiser-sysext/mkosi.build")
	profile := read("mkosi.profiles/endpoint-advertiser-sysext/mkosi.conf")
	for _, content := range []string{appUnit, pathUnit, activationUnit, build, profile} {
		if strings.Contains(strings.ToLower(content), "bird") || strings.Contains(strings.ToLower(content), "bgp") {
			t.Fatalf("endpoint advertiser retains built-in routing:\n%s", content)
		}
	}
	release := read("mkosi.profiles/endpoint-advertiser-sysext/mkosi.extra/usr/lib/extension-release.d/extension-release.katl-endpoint-advertiser")
	for _, want := range []string{"ID=katlos", "SYSEXT_LEVEL=katl-runtime-1", "ARCHITECTURE=x86-64"} {
		if !strings.Contains(release, want) {
			t.Fatalf("endpoint sysext compatibility metadata is missing %q", want)
		}
	}
}
