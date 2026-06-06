package nspawntest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	options := normalizeOptions(Options{})
	if options.StateRoot != filepath.Join("build", "nspawn") {
		t.Fatalf("StateRoot = %q", options.StateRoot)
	}
	if options.Keep != KeepFailed {
		t.Fatalf("Keep = %q", options.Keep)
	}
	if options.Missing != MissingFails {
		t.Fatalf("Missing = %q", options.Missing)
	}

	scenario := normalizeScenario(Scenario{Name: "units"}, Options{
		StateRoot: "/tmp/state",
		Keep:      KeepAlways,
	})
	if scenario.StateRoot != "/tmp/state" || scenario.Keep != KeepAlways {
		t.Fatalf("scenario not normalized: %#v", scenario)
	}
}

func TestRunDisabledSkipsWithoutHostChecks(t *testing.T) {
	tb := &fakeTB{}
	runner := Runner{
		Options: Options{
			Enabled:   false,
			StateRoot: t.TempDir(),
			RunID:     "run-1",
		},
		probe: probe{
			lookPath: func(string) (string, error) {
				return "", errors.New("should not check host")
			},
		},
	}
	result := runner.Run(tb, Scenario{Name: "confext"})
	if result.Status != StatusSkipped {
		t.Fatalf("Status = %q", result.Status)
	}
	if !tb.skipped || tb.failed {
		t.Fatalf("skipped=%v failed=%v", tb.skipped, tb.failed)
	}
	if !strings.Contains(tb.message, "KATL_NSPAWN_RUN") {
		t.Fatalf("message = %q", tb.message)
	}
}

func TestRunMissingPrerequisitesCanSkip(t *testing.T) {
	tb := &fakeTB{}
	runner := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: t.TempDir(),
			RunID:     "run-1",
			Missing:   MissingSkips,
		},
		probe: probe{
			lookPath: func(string) (string, error) {
				return "", errors.New("missing")
			},
			stat: func(string) (os.FileInfo, error) {
				return nil, os.ErrNotExist
			},
			euid: func() int { return 0 },
		},
	}
	result := runner.Run(tb, Scenario{Name: "confext", Root: "/missing"})
	if result.Status != StatusSkipped {
		t.Fatalf("Status = %q", result.Status)
	}
	if !tb.skipped || tb.failed {
		t.Fatalf("skipped=%v failed=%v", tb.skipped, tb.failed)
	}
	if len(result.Missing) != 2 {
		t.Fatalf("missing = %#v", result.Missing)
	}
}

func TestCheckHostReportsBinds(t *testing.T) {
	root := t.TempDir()
	bindSource := filepath.Join(t.TempDir(), "confext")
	if err := os.WriteFile(bindSource, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkHost(rootRef{Kind: rootKindDirectory, Path: root}, []Bind{
		{Source: bindSource, Target: "/run/confexts/katl-node"},
		{Source: "/missing", Target: "relative"},
	}, probe{
		lookPath: func(name string) (string, error) {
			if name == "systemd-nspawn" {
				return "/usr/bin/systemd-nspawn", nil
			}
			return "", os.ErrNotExist
		},
		stat: os.Stat,
		euid: func() int { return 0 },
	}, false)
	if err == nil {
		t.Fatal("checkHost() error = nil")
	}
	var prereq PrereqError
	if !errors.As(err, &prereq) {
		t.Fatalf("error type = %T", err)
	}
	text := err.Error()
	for _, want := range []string{"/missing", "target must be an absolute guest path"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q missing %q", text, want)
		}
	}
}

func TestCheckHostReportsMissingPrivileges(t *testing.T) {
	root := t.TempDir()
	err := checkHost(rootRef{Kind: rootKindDirectory, Path: root}, nil, probe{
		lookPath: func(name string) (string, error) {
			if name == "systemd-nspawn" {
				return "/usr/bin/systemd-nspawn", nil
			}
			return "", os.ErrNotExist
		},
		stat: os.Stat,
		euid: func() int { return 1000 },
	}, false)
	if err == nil || !strings.Contains(err.Error(), "nspawn privileges") {
		t.Fatalf("checkHost() error = %v, want privilege prerequisite", err)
	}

	err = checkHost(rootRef{Kind: rootKindDirectory, Path: root}, nil, probe{
		lookPath: func(name string) (string, error) {
			if name == "systemd-nspawn" {
				return "/usr/bin/systemd-nspawn", nil
			}
			return "", os.ErrNotExist
		},
		stat: os.Stat,
		euid: func() int { return 1000 },
	}, true)
	if err != nil {
		t.Fatalf("checkHost() with rootless override error = %v", err)
	}
}

func TestCheckHostRejectsDirectoryImage(t *testing.T) {
	image := t.TempDir()
	err := checkHost(rootRef{Kind: rootKindImage, Path: image}, nil, probe{
		lookPath: func(name string) (string, error) {
			if name == "systemd-nspawn" {
				return "/usr/bin/systemd-nspawn", nil
			}
			return "", os.ErrNotExist
		},
		stat: os.Stat,
		euid: func() int { return 0 },
	}, false)
	if err == nil || !strings.Contains(err.Error(), "not a regular file or block device") {
		t.Fatalf("checkHost() error = %v, want image shape rejection", err)
	}

	err = checkHost(rootRef{Kind: rootKindImage, Path: "/dev/null"}, nil, probe{
		lookPath: func(name string) (string, error) {
			if name == "systemd-nspawn" {
				return "/usr/bin/systemd-nspawn", nil
			}
			return "", os.ErrNotExist
		},
		stat: func(string) (os.FileInfo, error) {
			return fakeFileInfo{mode: os.ModeDevice | os.ModeCharDevice}, nil
		},
		euid: func() int { return 0 },
	}, false)
	if err == nil || !strings.Contains(err.Error(), "not a regular file or block device") {
		t.Fatalf("checkHost() character device error = %v, want image shape rejection", err)
	}
}

func TestDefaultRootDiagnosticIncludesOverrideHint(t *testing.T) {
	root, err := resolveRoot(Scenario{}, Options{})
	if err != nil {
		t.Fatalf("resolveRoot() error = %v", err)
	}
	err = checkHost(root, nil, probe{
		lookPath: func(name string) (string, error) {
			if name == "systemd-nspawn" {
				return "/usr/bin/systemd-nspawn", nil
			}
			return "", os.ErrNotExist
		},
		stat: func(string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		},
		euid: func() int { return 0 },
	}, false)
	if err == nil || !strings.Contains(err.Error(), "KATL_NSPAWN_ROOT") {
		t.Fatalf("checkHost() error = %v, want root override hint", err)
	}
}

func TestNspawnArgv(t *testing.T) {
	argv, err := nspawnArgv(rootRef{Kind: rootKindDirectory, Path: "/rootfs"}, []Bind{{Source: "/host/confext", Target: "/run/confexts/katl-node"}}, Command{
		Argv:       []string{"systemd-analyze", "verify", "/etc/systemd/system/example.service"},
		WorkingDir: "/etc",
		Env: map[string]string{
			"B": "two",
			"A": "one",
		},
	})
	if err != nil {
		t.Fatalf("nspawnArgv() error = %v", err)
	}
	got := strings.Join(argv, "\x00")
	for _, want := range []string{
		"systemd-nspawn\x00--quiet",
		"--settings=no",
		"--directory\x00/rootfs",
		"--volatile=state",
		"--bind-ro\x00/host/confext:/run/confexts/katl-node",
		"--setenv\x00A=one\x00--setenv\x00B=two",
		"--\x00systemd-analyze\x00verify",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv = %#v, missing %q", argv, want)
		}
	}
}

func TestNspawnArgvImage(t *testing.T) {
	argv, err := nspawnArgv(rootRef{Kind: rootKindImage, Path: "/tmp/root.raw"}, nil, Command{
		Argv: []string{"true"},
	})
	if err != nil {
		t.Fatalf("nspawnArgv() error = %v", err)
	}
	got := strings.Join(argv, "\x00")
	if !strings.Contains(got, "--image\x00/tmp/root.raw") {
		t.Fatalf("argv = %#v, missing image path", argv)
	}
}

func TestRunCapturesCommandArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=fedora\nVERSION_ID=42\nNAME=Fedora\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{stdout: "ok\n", stderr: "warn\n"}
	tb := &fakeTB{}
	harness := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: t.TempDir(),
			RunID:     "run-1",
		},
		probe: probe{
			lookPath: func(name string) (string, error) {
				if name == "systemd-nspawn" {
					return "/usr/bin/systemd-nspawn", nil
				}
				return "", os.ErrNotExist
			},
			stat:     os.Stat,
			readFile: os.ReadFile,
			euid:     func() int { return 0 },
		},
		command: runner,
		now:     fixedClock(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)),
	}
	result := harness.Run(tb, Scenario{
		Name: "confext verify",
		Root: root,
		Commands: []Command{{
			Name: "verify",
			Argv: []string{"systemd-analyze", "verify", "/etc/systemd/system/example.service"},
		}},
	})
	if result.Status != StatusPassed || tb.failed || tb.skipped {
		t.Fatalf("status=%q failed=%v skipped=%v message=%q", result.Status, tb.failed, tb.skipped, tb.message)
	}
	if result.Root.Kind != rootKindDirectory || result.Root.OSRelease != "ID=fedora VERSION_ID=42" {
		t.Fatalf("root identity = %#v", result.Root)
	}
	if len(result.Commands) != 1 || result.Commands[0].ExitStatus != 0 {
		t.Fatalf("commands = %#v", result.Commands)
	}
	stdout, err := os.ReadFile(result.Commands[0].Stdout)
	if err != nil {
		t.Fatalf("read stdout artifact: %v", err)
	}
	if string(stdout) != "ok\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	commandLine, err := os.ReadFile(result.Commands[0].Command)
	if err != nil {
		t.Fatalf("read command artifact: %v", err)
	}
	if !strings.Contains(string(commandLine), "systemd-nspawn") {
		t.Fatalf("command artifact = %q", commandLine)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "systemd-nspawn" {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
	if _, err := os.Stat(result.Artifacts.Result); err != nil {
		t.Fatalf("result artifact missing: %v", err)
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
	t.message = strings.TrimSpace(formatMessage(format, args...))
}

func (t *fakeTB) Fatalf(format string, args ...any) {
	t.failed = true
	t.message = strings.TrimSpace(formatMessage(format, args...))
}

type fakeRunner struct {
	stdout string
	stderr string
	err    error
	calls  []runnerCall
}

type runnerCall struct {
	name string
	args []string
}

type fakeFileInfo struct {
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

func (r *fakeRunner) Run(_ context.Context, name string, args []string, stdout, stderr io.Writer) error {
	r.calls = append(r.calls, runnerCall{name: name, args: append([]string(nil), args...)})
	_, _ = io.WriteString(stdout, r.stdout)
	_, _ = io.WriteString(stderr, r.stderr)
	return r.err
}

func fixedClock(start time.Time) func() time.Time {
	next := start
	return func() time.Time {
		now := next
		next = next.Add(time.Second)
		return now
	}
}

func formatMessage(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
