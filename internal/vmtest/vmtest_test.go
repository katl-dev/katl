package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	options := normalizeOptions(Options{})
	if options.StateRoot != filepath.Join(os.TempDir(), "katl-vmtest") {
		t.Fatalf("StateRoot = %q", options.StateRoot)
	}
	if options.Keep != KeepFailed {
		t.Fatalf("Keep = %q", options.Keep)
	}
	if options.KVM != KVMAuto {
		t.Fatalf("KVM = %q", options.KVM)
	}
	if options.Missing != MissingFails {
		t.Fatalf("Missing = %q", options.Missing)
	}

	scenario := normalizeScenario(Scenario{Name: "boot"}, Options{
		StateRoot: "/tmp/state",
		Keep:      KeepAlways,
		KVM:       KVMOff,
	})
	if scenario.StateRoot != "/tmp/state" || scenario.Keep != KeepAlways || scenario.KVM != KVMOff {
		t.Fatalf("scenario not normalized: %#v", scenario)
	}
	if scenario.Host.KVM != KVMOff {
		t.Fatalf("Host.KVM = %q", scenario.Host.KVM)
	}
}

func TestStrictWorldForcesMissingFailures(t *testing.T) {
	t.Setenv("KATL_VMTEST_WORLD_STRICT", "1")
	options := normalizeOptions(Options{Missing: MissingSkips})
	if options.Missing != MissingFails {
		t.Fatalf("Missing = %q, want %q", options.Missing, MissingFails)
	}
}

func TestOptIn(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		want    Status
		skip    bool
		fail    bool
	}{
		{
			name: "disabled",
			options: Options{
				Enabled: false,
				RunID:   "run-1",
			},
			want: StatusSkipped,
			skip: true,
		},
		{
			name: "fail",
			options: Options{
				Enabled: true,
				RunID:   "run-1",
				Missing: MissingFails,
			},
			want: StatusFailed,
			fail: true,
		},
		{
			name: "skip",
			options: Options{
				Enabled: true,
				RunID:   "run-1",
				Missing: MissingSkips,
			},
			want: StatusSkipped,
			skip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := &fakeTB{}
			tt.options.StateRoot = t.TempDir()
			runner := Runner{
				Options: tt.options,
				probe: probe{
					lookPath: func(string) (string, error) {
						return "", errors.New("missing")
					},
				},
			}
			result := runner.Run(tb, Scenario{
				Name: "boot",
				Host: HostRequirements{Libvirt: true},
			})
			if result.Status != tt.want {
				t.Fatalf("Status = %q", result.Status)
			}
			if tb.skipped != tt.skip || tb.failed != tt.fail {
				t.Fatalf("skipped=%v failed=%v", tb.skipped, tb.failed)
			}
			if tt.options.Enabled && !strings.Contains(tb.message, "virsh") {
				t.Fatalf("message %q missing tool name", tb.message)
			}
		})
	}
}

func TestRequireEnvFailsInStrictWorld(t *testing.T) {
	t.Setenv("KATL_VMTEST_WORLD_STRICT", "1")
	tb := &fakeTB{}
	value := RequireEnv(tb, "KATL_REQUIRED_FIXTURE")
	if value != "" {
		t.Fatalf("RequireEnv() = %q, want empty after fatal", value)
	}
	if !tb.failed || tb.skipped {
		t.Fatalf("failed=%v skipped=%v message=%q", tb.failed, tb.skipped, tb.message)
	}
	if !strings.Contains(tb.message, "KATL_REQUIRED_FIXTURE") {
		t.Fatalf("message %q missing env name", tb.message)
	}
}

func TestStrictWorldFailsMissingPrerequisites(t *testing.T) {
	t.Setenv("KATL_VMTEST_WORLD_STRICT", "1")
	tb := &fakeTB{}
	runner := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: t.TempDir(),
			RunID:     "run-strict",
			Missing:   MissingSkips,
		},
		probe: probe{
			lookPath: func(string) (string, error) {
				return "", errors.New("missing")
			},
		},
	}
	result := runner.Run(tb, Scenario{
		Name: "boot",
		Host: HostRequirements{Libvirt: true},
	})
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q, want %q", result.Status, StatusFailed)
	}
	if !tb.failed || tb.skipped {
		t.Fatalf("failed=%v skipped=%v message=%q", tb.failed, tb.skipped, tb.message)
	}
}

func TestHostCheck(t *testing.T) {
	err := checkHost(HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     KVMOn,
	}, probe{
		lookPath: func(name string) (string, error) {
			if name == "virsh" || name == "script" {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("%s missing", name)
		},
		stat: func(path string) (fs.FileInfo, error) {
			if path == "/ovmf/code.fd" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		},
		env: func(name string) string {
			if name == "KATL_OVMF_CODE" {
				return "/ovmf/code.fd"
			}
			return ""
		},
		access: func(string) error {
			return os.ErrPermission
		},
	})
	if err == nil {
		t.Fatal("CheckHost succeeded")
	}
	var prereq PrereqError
	if !errors.As(err, &prereq) {
		t.Fatalf("error type = %T", err)
	}
	text := err.Error()
	for _, want := range []string{"OVMF vars", "/dev/kvm", "KATL_OVMF_VARS"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q missing %q", text, want)
		}
	}
	if len(prereq.Missing) != 2 {
		t.Fatalf("missing = %#v", prereq.Missing)
	}
}

func TestRunnerCheckHostUsesRunnerProbe(t *testing.T) {
	runner := Runner{
		probe: probe{
			env: func(name string) string {
				switch name {
				case "KATL_OVMF_CODE":
					return "/runner/code.fd"
				case "KATL_OVMF_VARS":
					return "/runner/vars.fd"
				default:
					return ""
				}
			},
			stat: func(path string) (fs.FileInfo, error) {
				return nil, fmt.Errorf("runner stat missing: %s", path)
			},
		},
	}
	err := runner.CheckHost(HostRequirements{OVMF: true})
	if err == nil {
		t.Fatal("Runner.CheckHost() error = nil")
	}
	if !strings.Contains(err.Error(), "/runner/code.fd") || !strings.Contains(err.Error(), "runner stat missing") {
		t.Fatalf("Runner.CheckHost() error = %v", err)
	}
}

func TestPlanPaths(t *testing.T) {
	result, err := NewRunner(Options{
		StateRoot: "/tmp/katl-vmtest",
		Keep:      KeepAlways,
		KVM:       KVMOff,
	}).Plan(Scenario{
		Name:  "first install",
		RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if result.RunDir != "/tmp/katl-vmtest/run-1" {
		t.Fatalf("RunDir = %q", result.RunDir)
	}
	if result.VMDir != "/tmp/katl-vmtest/run-1/vm" {
		t.Fatalf("VMDir = %q", result.VMDir)
	}
	if result.DiskDir != "/tmp/katl-vmtest/run-1/disks" {
		t.Fatalf("DiskDir = %q", result.DiskDir)
	}
	if result.ManifestDir != "/tmp/katl-vmtest/run-1/manifests" {
		t.Fatalf("ManifestDir = %q", result.ManifestDir)
	}
	if result.Keep != KeepAlways || result.KVM != KVMOff {
		t.Fatalf("result policy = %#v", result)
	}
	if result.Artifacts.Scenario != "/tmp/katl-vmtest/run-1/scenario.json" {
		t.Fatalf("scenario artifact = %q", result.Artifacts.Scenario)
	}
	if result.Artifacts.LaunchCommand != "/tmp/katl-vmtest/run-1/vm/launch-command.txt" {
		t.Fatalf("launch command artifact = %q", result.Artifacts.LaunchCommand)
	}
	if result.Artifacts.DomainXML != "/tmp/katl-vmtest/run-1/vm/domain.xml" {
		t.Fatalf("domain XML artifact = %q", result.Artifacts.DomainXML)
	}
}

func TestPersistPass(t *testing.T) {
	root := t.TempDir()
	now := fixedClock()
	runner := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: root,
			RunID:     "run-1",
		},
		probe: probe{
			lookPath: func(name string) (string, error) {
				return "/usr/bin/" + name, nil
			},
		},
		now: now,
	}
	tb := &fakeTB{}
	result := runner.Run(tb, Scenario{
		Name: "installer boot",
		Host: HostRequirements{Libvirt: true},
		Disks: []DiskFixture{
			TargetDisk("root", "qcow2", "20G"),
		},
	})
	if tb.failed || tb.skipped {
		t.Fatalf("failed=%v skipped=%v message=%q", tb.failed, tb.skipped, tb.message)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q", result.Status)
	}
	loaded := readResult(t, result.Artifacts.Result)
	if loaded.Status != StatusPassed {
		t.Fatalf("persisted Status = %q", loaded.Status)
	}
	if loaded.DurationMS != 1000 {
		t.Fatalf("DurationMS = %d", loaded.DurationMS)
	}
	if loaded.Artifacts.InstallerSerial != filepath.Join(root, "run-1", "vm", "installer-serial.log") {
		t.Fatalf("installer serial = %q", loaded.Artifacts.InstallerSerial)
	}
	if len(loaded.Phases) != 1 || loaded.Phases[0].Status != StatusPassed {
		t.Fatalf("phases = %#v", loaded.Phases)
	}
	if len(loaded.Disks) != 1 || loaded.Disks[0].GuestSelector != "/dev/disk/by-id/virtio-katl-root" {
		t.Fatalf("disks = %#v", loaded.Disks)
	}
	if _, err := os.Stat(result.Artifacts.Scenario); err != nil {
		t.Fatalf("scenario.json missing: %v", err)
	}
	record := readRecord(t, result.Artifacts.Scenario)
	if record.Scenario.Name != "installer boot" || record.Result.Status != StatusPassed {
		t.Fatalf("scenario record = %#v", record)
	}
}

func TestPersistFail(t *testing.T) {
	root := t.TempDir()
	runner := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: root,
			RunID:     "run-1",
			Missing:   MissingFails,
		},
		probe: probe{
			lookPath: func(string) (string, error) {
				return "", errors.New("not found")
			},
		},
		now: fixedClock(),
	}
	tb := &fakeTB{}
	result := runner.Run(tb, Scenario{
		Name: "installer boot",
		Host: HostRequirements{Libvirt: true},
	})
	if !tb.failed {
		t.Fatalf("failed = false")
	}
	if !strings.Contains(tb.message, result.RunDir) {
		t.Fatalf("message %q missing run dir %q", tb.message, result.RunDir)
	}
	loaded := readResult(t, result.Artifacts.Result)
	if loaded.Status != StatusFailed {
		t.Fatalf("persisted Status = %q", loaded.Status)
	}
	if !strings.Contains(loaded.FailureSummary, "virsh") {
		t.Fatalf("failure = %q", loaded.FailureSummary)
	}
	if loaded.DurationMS != 1000 {
		t.Fatalf("DurationMS = %d", loaded.DurationMS)
	}
	if len(loaded.Missing) != 2 {
		t.Fatalf("missing = %#v", loaded.Missing)
	}
}

type fakeTB struct {
	skipped bool
	failed  bool
	message string
}

func (t *fakeTB) Helper() {}

func (t *fakeTB) Skipf(format string, args ...any) {
	t.skipped = true
	t.message = fmt.Sprintf(format, args...)
}

func (t *fakeTB) Fatalf(format string, args ...any) {
	t.failed = true
	t.message = fmt.Sprintf(format, args...)
}

func TestHostCheckMTools(t *testing.T) {
	err := checkHost(HostRequirements{
		MTools: true,
	}, probe{
		lookPath: func(name string) (string, error) {
			if name == "mformat" {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("%s missing", name)
		},
	})
	if err == nil {
		t.Fatal("checkHost() error = nil, want missing mtools")
	}
	text := err.Error()
	for _, want := range []string{"mcopy", "truncate"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q missing %q", text, want)
		}
	}
}

func readResult(t *testing.T, path string) Result {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return result
}

func readRecord(t *testing.T, path string) scenarioRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scenario: %v", err)
	}
	var record scenarioRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode scenario: %v", err)
	}
	return record
}

func fixedClock() func() time.Time {
	values := []time.Time{
		time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 3, 12, 0, 1, 0, time.UTC),
	}
	return func() time.Time {
		if len(values) == 0 {
			return time.Date(2026, 6, 3, 12, 0, 1, 0, time.UTC)
		}
		value := values[0]
		values = values[1:]
		return value
	}
}
