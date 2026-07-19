package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpointAdvertiserSysextOnlyStartsBirdForManagedVIP(t *testing.T) {
	repo := repoRoot(t)
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	birdUnit := read("mkosi.profiles/endpoint-advertiser-sysext/katl-app-bird.service")
	for _, want := range []string{
		"ConditionPathExists=/etc/katl/apps/bird/bird.conf",
		"ConditionPathExists=/etc/katl/apps/bgp-api-vip/advertisement-enabled",
		"ExecStart=/usr/bin/bird ",
		"RestrictAddressFamilies=AF_INET AF_NETLINK AF_UNIX",
	} {
		if !strings.Contains(birdUnit, want) {
			t.Fatalf("Katl BIRD unit is missing %q", want)
		}
	}
	if strings.Contains(birdUnit, "WantedBy=") {
		t.Fatal("Katl BIRD unit must not be enabled independently of managed VIP activation")
	}

	appUnit := read("mkosi.profiles/endpoint-advertiser-sysext/katl-app-bgp-api-vip.service")
	for _, want := range []string{
		"Requires=katl-app-bird.service",
		"ConditionPathExists=/etc/katl/apps/bgp-api-vip/config.yaml",
		"ConditionPathExists=/etc/katl/apps/bgp-api-vip/advertisement-enabled",
		"RestrictAddressFamilies=AF_INET AF_NETLINK AF_UNIX",
	} {
		if !strings.Contains(appUnit, want) {
			t.Fatalf("endpoint advertiser unit is missing %q", want)
		}
	}
	if strings.Contains(appUnit, "WantedBy=") {
		t.Fatal("endpoint advertiser unit must be selected by Katl rather than enabled globally")
	}

	activationUnit := read("mkosi.profiles/runtime/katl-endpoint-activate.service")
	for _, want := range []string{
		"ConditionPathExists=/etc/katl/apps/bgp-api-vip/config.yaml",
		"ConditionPathExists=/etc/katl/apps/bgp-api-vip/advertisement-enabled",
		"ExecStart=/usr/bin/systemctl daemon-reload",
		"start katl-app-bgp-api-vip.service",
	} {
		if !strings.Contains(activationUnit, want) {
			t.Fatalf("endpoint activation unit is missing %q", want)
		}
	}
	if strings.Contains(activationUnit, "katl-extension-daemon-reload.service") {
		t.Fatal("endpoint activation must not depend on a nonexistent daemon-reload unit")
	}

	build := read("mkosi.profiles/endpoint-advertiser-sysext/mkosi.build")
	if !strings.Contains(build, `ln -sf /dev/null "$DESTDIR/usr/lib/systemd/system/bird.service"`) {
		t.Fatal("endpoint sysext must mask Fedora's generic bird.service")
	}
	profile := read("mkosi.profiles/endpoint-advertiser-sysext/mkosi.conf")
	for _, path := range []string{"usr/lib/sysusers.d/bird.conf", "usr/lib/tmpfiles.d/bird.conf"} {
		if !strings.Contains(profile, path) {
			t.Fatalf("endpoint sysext must remove Fedora's generic %s hook", path)
		}
	}

	release := read("mkosi.profiles/endpoint-advertiser-sysext/mkosi.extra/usr/lib/extension-release.d/extension-release.katl-endpoint-advertiser")
	for _, want := range []string{"ID=katlos", "SYSEXT_LEVEL=katl-runtime-1", "ARCHITECTURE=x86-64"} {
		if !strings.Contains(release, want) {
			t.Fatalf("endpoint sysext compatibility metadata is missing %q", want)
		}
	}
}
