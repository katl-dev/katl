package configapply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestSystemdBootTransaction(t *testing.T) {
	root := t.TempDir()
	node := manifest.NodeConfig{
		HostConfiguration: manifest.HostConfiguration{EnabledUnits: []string{"systemd-timesyncd.service"}},
		SystemExtensions: []manifest.SystemExtension{{Units: []manifest.SystemExtensionUnit{
			{Name: "optional.service", Enable: true},
			{Name: "required.service", Enable: true, RequiredForBootHealth: true},
		}}},
	}
	plan, err := PrepareSystemdActivation(context.Background(), root, node, &fakeCommandRunner{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "run/systemd/system/katl-configured-units.target"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Unit]\nDescription=Configured Katl systemd units\nDefaultDependencies=no\nWants=optional.service required.service systemd-timesyncd.service\nRequires=required.service\nAfter=required.service\n"
	if string(data) != want {
		t.Fatalf("target:\n%s\nwant:\n%s", data, want)
	}
	var starts int
	for _, command := range plan.Commands {
		if len(command.Argv) > 1 && command.Argv[1] == "start" {
			starts++
			if strings.Join(command.Argv, " ") != "systemctl start katl-configured-units.target" {
				t.Fatalf("start transaction = %v", command.Argv)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("got %d start transactions, want native dependency ordering in one", starts)
	}
}

func TestSystemdFilesApplyLive(t *testing.T) {
	for _, file := range []string{"example.service", "example.service.d/override.conf", "example.timer", "example@one.service.d/site.conf"} {
		t.Run(file, func(t *testing.T) {
			content := "[Unit]\nDescription=Updated\n"
			desired := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
				"unit": {Files: []manifest.HostConfigurationFile{{Path: "/etc/systemd/system/" + file, Content: &content}}},
			}}
			plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
			if !plan.Live || !strings.Contains(plan.Message, "try-restart systemd unit") {
				t.Fatalf("plan = %#v", plan)
			}
		})
	}
}

func TestLiveUnitsShareStartTransaction(t *testing.T) {
	content := "[Service]\nEnvironment=VALUE=new\n"
	desired := manifest.HostConfiguration{
		EnabledUnits: []string{"a-new.service"},
		Sets: map[string]manifest.HostConfigurationSet{"units": {Files: []manifest.HostConfigurationFile{
			{Path: "/etc/systemd/system/z-existing.service.d/env.conf", Content: &content},
			{Path: "/etc/systemd/system/stopped.service.d/env.conf", Content: &content},
		}}},
	}
	host := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true}})
	runner := &fakeCommandRunner{results: map[string]CommandResult{
		"systemd-state-a-new.service":      {Stdout: "inactive"},
		"systemd-state-stopped.service":    {Stdout: "inactive"},
		"systemd-state-z-existing.service": {Stdout: "active"},
		"systemd-unit-load-a-new.service":  {Stdout: "loaded"},
		"systemd-units-stop-for-restart":   {Stdout: "katl-systemd-transaction-complete"},
		"systemd-units-restart":            {Stdout: "katl-systemd-transaction-complete"},
	}}
	_, err := (Executor{Runner: runner, Activator: &fakeActivator{}, HostConfiguration: &host, Now: fixedNow}).ExecuteLive(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []Command
	for _, command := range runner.commands {
		if len(command.Argv) > 0 && command.Argv[0] == "systemd-run" {
			jobs = append(jobs, command)
		}
	}
	if len(jobs) != 2 || !slices.Contains(jobs[0].Argv, "--property=Conflicts=a-new.service z-existing.service") || !slices.Contains(jobs[1].Argv, "--property=Requires=a-new.service z-existing.service") || !slices.Contains(jobs[1].Argv, "--property=After=a-new.service z-existing.service") {
		t.Fatalf("jobs = %v; stop/start groups must honour ordering and exclude inactive units", jobs)
	}
}

func TestFailedEnablementRestoresStoppedUnit(t *testing.T) {
	host := planHostConfigurationChange(manifest.HostConfiguration{}, manifest.HostConfiguration{EnabledUnits: []string{"example.service"}})
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true}})
	runner := &fakeCommandRunner{results: map[string]CommandResult{
		"systemd-state-example.service":     {Stdout: "inactive"},
		"systemd-unit-load-example.service": {Stdout: "loaded"},
		"systemd-units-restart":             {ExitStatus: 1, Stderr: "start failed"},
	}}
	activator := &fakeActivator{}
	status, err := (Executor{Runner: runner, Activator: activator, HostConfiguration: &host, Now: fixedNow}).ExecuteLive(t.Context(), plan)
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("error = %v", err)
	}
	if status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionPassed {
		t.Fatalf("rollback = %#v", status.Rollback)
	}
	var stops, disables, starts int
	for _, command := range runner.commands {
		switch strings.Join(command.Argv, " ") {
		case "systemctl stop example.service":
			stops++
		case "systemctl --runtime --no-reload disable example.service":
			disables++
		case "systemctl restart example.service":
			starts++
		}
	}
	if stops != 1 || disables != 1 || starts != 1 {
		t.Fatalf("stops=%d disables=%d starts=%d; failed new unit must end stopped and disabled", stops, disables, starts)
	}
}

func TestCancelledApplyStillRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true}})
	host := planHostConfigurationChange(manifest.HostConfiguration{}, manifest.HostConfiguration{EnabledUnits: []string{"example.service"}})
	runner := &fakeCommandRunner{results: map[string]CommandResult{
		"systemd-state-example.service":     {Stdout: "inactive"},
		"systemd-unit-load-example.service": {Stdout: "loaded"},
	}}
	cancelling := commandRunnerFunc(func(ctx context.Context, command Command) (CommandResult, error) {
		if command.Name == "systemd-units-restart" {
			cancel()
		}
		if err := ctx.Err(); err != nil {
			return CommandResult{}, err
		}
		return runner.Run(ctx, command)
	})
	status, err := (Executor{Runner: cancelling, Activator: &fakeActivator{}, HostConfiguration: &host, Now: fixedNow}).ExecuteLive(ctx, plan)
	if !errors.Is(err, context.Canceled) || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionPassed {
		t.Fatalf("status=%#v error=%v", status, err)
	}
}

type commandRunnerFunc func(context.Context, Command) (CommandResult, error)

func (run commandRunnerFunc) Run(ctx context.Context, command Command) (CommandResult, error) {
	return run(ctx, command)
}

func TestBootNotificationsWaitForExtensions(t *testing.T) {
	node := manifest.NodeConfig{HostConfiguration: manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
		"services": {Notify: manifest.HostConfigurationNotifications{Systemd: []manifest.HostConfigurationSystemdNotification{
			{Unit: "early.service", Action: "try-restart"},
			{Unit: "late.service", Action: "restart"},
		}}},
	}}}
	runner := commandRunnerFunc(func(_ context.Context, command Command) (CommandResult, error) {
		state := "active"
		if slices.Contains(command.Argv, "late.service") {
			state = "activating"
		}
		return CommandResult{Stdout: state}, nil
	})
	plan, err := PrepareSystemdActivation(t.Context(), t.TempDir(), node, runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Commands) != 1 || strings.Join(plan.Commands[0].Argv, " ") != "systemctl restart early.service" {
		t.Fatalf("boot notification commands = %#v", plan.Commands)
	}
}

func TestExtensionConfigurationSharesUnitLifecycle(t *testing.T) {
	oldConfig, newConfig := "old", "new"
	before := manifest.NodeConfig{SystemExtensions: []manifest.SystemExtension{{
		Name: "tools", Bundle: "registry.example/tools:v1", State: manifest.SystemExtensionPresent,
		Units:         []manifest.SystemExtensionUnit{{Name: "tools.service", Enable: true}},
		Configuration: manifest.SystemExtensionConfiguration{Files: []manifest.HostConfigurationFile{{Path: "/etc/tools.conf", Content: &oldConfig}}},
	}}}
	after := before
	after.SystemExtensions = slices.Clone(before.SystemExtensions)
	after.SystemExtensions[0].Configuration.Files = []manifest.HostConfigurationFile{{Path: "/etc/tools.conf", Content: &newConfig}}
	if !extensionPayloadsEqual(before.SystemExtensions, after.SystemExtensions) {
		t.Fatal("configuration change classified as payload change")
	}
	plan := planHostConfigurationChange(effectiveHostConfiguration(before), effectiveHostConfiguration(after))
	if !plan.Live || plan.unitNotifications["tools.service"] != "try-reload-or-restart" {
		t.Fatalf("extension plan = %#v", plan)
	}
	after.SystemExtensions[0].Bundle = "registry.example/tools:v2"
	if extensionPayloadsEqual(before.SystemExtensions, after.SystemExtensions) {
		t.Fatal("new payload classified as configuration-only")
	}
}

func TestDependencyFailureRollsBack(t *testing.T) {
	host := planHostConfigurationChange(manifest.HostConfiguration{}, manifest.HostConfiguration{EnabledUnits: []string{"one.service", "two.service"}})
	plan := liveExecutorPlan(t, []Change{{Domain: DomainHostConfiguration, LivePreflightOK: true}})
	runner := &fakeCommandRunner{results: map[string]CommandResult{
		"systemd-state-one.service":      {Stdout: "inactive"},
		"systemd-state-two.service":      {Stdout: "inactive"},
		"systemd-unit-load-one.service":  {Stdout: "loaded"},
		"systemd-unit-load-two.service":  {Stdout: "loaded"},
		"systemd-units-stop-for-restart": {Stdout: "katl-systemd-transaction-complete"},
		// A Requires dependency failed, so the transient service never ran even
		// though systemd-run exited zero.
		"systemd-units-restart": {ExitStatus: 0},
		"systemd-units-stop":    {Stdout: "katl-systemd-transaction-complete"},
	}}
	status, err := (Executor{Runner: runner, Activator: &fakeActivator{}, HostConfiguration: &host, Now: fixedNow}).ExecuteLive(t.Context(), plan)
	if err == nil || !strings.Contains(err.Error(), "dependency transaction did not complete") || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionPassed {
		t.Fatalf("error=%v rollback=%#v", err, status.Rollback)
	}
}

func TestExplicitUnitActions(t *testing.T) {
	content := "[Service]\nExecStart=/usr/bin/true\n"
	for _, tc := range []struct {
		name, action, want string
		masked             bool
	}{
		{name: "force start", action: "restart", want: "restart"},
		{name: "definition overrides reload", action: "reload", want: "try-restart"},
		{name: "mask overrides notification", action: "restart", masked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := manifest.HostConfiguration{Sets: map[string]manifest.HostConfigurationSet{
				"unit": {
					Files:  []manifest.HostConfigurationFile{{Path: "/etc/systemd/system/example.service", Content: &content}},
					Notify: manifest.HostConfigurationNotifications{Systemd: []manifest.HostConfigurationSystemdNotification{{Unit: "example.service", Action: tc.action}}},
				},
			}}
			if tc.masked {
				desired.MaskedUnits = []string{"example.service"}
			}
			plan := planHostConfigurationChange(manifest.HostConfiguration{}, desired)
			if got := plan.unitNotifications["example.service"]; got != tc.want {
				t.Fatalf("notification = %q, want %q", got, tc.want)
			}
		})
	}
}
