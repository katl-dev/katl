package configapply

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

const candidateGeneration = "2026.06.05-002"

// confextHost models the selected confext and kernel parameters of a running
// host: the candidate's files are visible only while it is activated.
type confextHost struct {
	fakeCommandRunner
	active   string
	log      []string
	failName string
}

func (h *confextHost) Activate(_ context.Context, record generation.Record) error {
	h.active = record.GenerationID
	h.log = append(h.log, "activate "+record.GenerationID)
	return nil
}

func (h *confextHost) Rollback(_ context.Context, target string) error {
	h.active = target
	h.log = append(h.log, "rollback "+target)
	return nil
}

func (h *confextHost) Run(ctx context.Context, command Command) (CommandResult, error) {
	h.log = append(h.log, command.Name)
	if command.Name == h.failName && h.active == candidateGeneration {
		return CommandResult{ExitStatus: 1, Stderr: "write refused"}, nil
	}
	// Rules added by the candidate do not exist once it is rolled back.
	if command.Name == "udev-rules-verify" && h.active != candidateGeneration {
		return CommandResult{ExitStatus: 1, Stderr: "no such file"}, nil
	}
	return h.fakeCommandRunner.Run(ctx, command)
}

func TestExecutorRestoresSysctlsAfterPartialFailure(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true}})
	desired := sysctlConfiguration(map[string]string{"/etc/sysctl.d/95-demo.conf": "kernel.sysrq = 1\nvm.swappiness = 20\n"})
	host := planHostConfigurationChange(manifest.HostConfiguration{}, desired, sysctlHostRoot(t))
	if !host.Live || len(host.SysctlAssignments) != 2 {
		t.Fatalf("host plan = %#v, want two live assignments", host)
	}
	runtime := &confextHost{failName: "sysctl-apply-vm.swappiness"}
	runtime.sysctls = map[string]string{"kernel.sysrq": "16", "vm.swappiness": "60"}

	status, err := (Executor{Runner: runtime, Activator: runtime, HostConfiguration: &host, Now: fixedNow}).ExecuteLive(t.Context(), plan)

	if err == nil || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionPassed {
		t.Fatalf("error = %v, rollback = %#v; want a passed rollback", err, status.Rollback)
	}
	// The previous generation has no 95-demo.conf, so selecting it restores
	// the file's absence before the captured runtime values are rewritten.
	previous := plan.GenerationRecord.ConfigApply.PreviousGeneration
	if runtime.active != previous {
		t.Fatalf("active generation = %q, want previous %q", runtime.active, previous)
	}
	want := map[string]string{"kernel.sysrq": "16", "vm.swappiness": "60"}
	if !maps.Equal(runtime.sysctls, want) {
		t.Fatalf("kernel values = %v, want captured values %v", runtime.sysctls, want)
	}
	rollback := slices.Index(runtime.log, "rollback "+previous)
	restore := slices.Index(runtime.log, "sysctl-restore-kernel.sysrq")
	if rollback < 0 || restore < rollback {
		t.Fatalf("log = %v, want runtime values restored after the previous files", runtime.log)
	}
}

func TestExecutorRollsBackAddedUdevRules(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true}})
	desired := testHostConfiguration("ups-device", "/etc/udev/rules.d/80-ups.rules", `SUBSYSTEM=="usb"`+"\n")
	host := planHostConfigurationChange(manifest.HostConfiguration{}, desired, t.TempDir())
	runtime := &confextHost{failName: "udev-rules-reload"}

	status, err := (Executor{Runner: runtime, Activator: runtime, HostConfiguration: &host, Now: fixedNow}).ExecuteLive(t.Context(), plan)

	if err == nil || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionPassed {
		t.Fatalf("error = %v, rollback = %#v; want a passed rollback", err, status.Rollback)
	}
}
