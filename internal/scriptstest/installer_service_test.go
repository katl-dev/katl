package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerServiceHasOperationalReadiness(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "mkosi.profiles", "installer-image", "mkosi.extra", "usr", "lib", "systemd", "system", "katlos-install.service")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	for _, want := range []string{"Type=notify", "NotifyAccess=main", "StandardOutput=journal+console", "StandardError=journal+console"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("installer service missing %q", want)
		}
	}
	if strings.Contains(unit, "Type=oneshot") {
		t.Fatal("installer wait-for-config service must not remain an activating oneshot")
	}
	targetPath := filepath.Join(root, "mkosi.profiles", "installer-image", "mkosi.extra", "usr", "lib", "systemd", "system", "katl-installer.target")
	target, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(target), "After=systemd-journald.service katl-input-apply.service systemd-networkd.service systemd-resolved.service katl-installer-smoke.service katlos-install.service") {
		t.Fatal("installer target does not wait for operational installer readiness")
	}
	config, err := os.ReadFile(filepath.Join(root, "mkosi.profiles", "installer-image", "mkosi.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "rd.systemd.unit=katl-installer.target") {
		t.Fatal("installer image does not boot to the dedicated installer target")
	}
}
