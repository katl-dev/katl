package scenarios

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zariel/katl/internal/vmtest"
)

func TestInstallerISOBootSmoke(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer ISO boot smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-iso-boot")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	iso := os.Getenv("KATL_INSTALLER_ISO")
	if iso == "" {
		iso = filepath.Join(katlRepoRoot(t), "_build", "mkosi", "katl-installer.iso")
	}
	runner := vmtest.NewRunner(options)
	result, err := vmtest.RunInstallerBoot(
		context.Background(),
		runner,
		vmtest.Scenario{Name: "installer-iso-boot"},
		vmtest.InstallerBootConfig{
			InstallerISO: iso,
			VM: vmtest.VMConfig{
				KVM:     options.KVM,
				RAMMiB:  2048,
				CPUs:    2,
				Timeout: 3 * time.Minute,
			},
		},
		vmtest.VMRunner{},
	)
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer ISO boot status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}
