package nspawntest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestPrepareDefaultRootRunsFixtureScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture requires POSIX execution")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repo, "scripts", "prepare-nspawn-userspace-fixture")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > prepare-args.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := Options{Enabled: true}
	if err := PrepareDefaultRoot(context.Background(), &options, repo); err != nil {
		t.Fatalf("PrepareDefaultRoot() error = %v", err)
	}
	wantState := filepath.Join(repo, "build", "nspawn")
	wantRoot := filepath.Join(wantState, "root")
	if options.StateRoot != wantState || options.Root != wantRoot {
		t.Fatalf("options = %#v, want state %q root %q", options, wantState, wantRoot)
	}
	args, err := os.ReadFile(filepath.Join(repo, "prepare-args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--state-dir\n" + wantState, "--root\n" + wantRoot, "--force"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("prepare args = %q, missing %q", args, want)
		}
	}
}

func TestPrepareFixtureSelfProvisionsRuntimeHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture requires POSIX execution")
	}
	repo := repoRootForTest(t)
	dir := t.TempDir()
	sourceRoot := filepath.Join(dir, "source-root")
	for _, path := range []string{
		"usr/bin/sh",
		"usr/bin/cp",
		"usr/bin/grep",
		"usr/bin/mktemp",
		"usr/bin/systemd-analyze",
	} {
		writeGuestExecutable(t, filepath.Join(sourceRoot, path))
	}
	stateDir := filepath.Join(dir, "state")
	cmd := exec.Command(filepath.Join(repo, "scripts", "prepare-nspawn-userspace-fixture"),
		"--source-root", sourceRoot,
		"--state-dir", stateDir,
		"--force",
	)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOCACHE=/tmp/katl-go-cache")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prepare fixture error = %v\n%s", err, output)
	}

	root := filepath.Join(stateDir, "root")
	for _, path := range []string{
		"usr/lib/katl/runtime/katl-generation-activate",
		"usr/lib/katl/runtime/katl-runtime-status",
	} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("stat helper %s: %v", path, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("helper %s mode = %v, want executable", path, info.Mode())
		}
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "nspawn-fixture.json"))
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	var manifest struct {
		ProvisionedHelpers []struct {
			Name   string `json:"name"`
			Path   string `json:"path"`
			Source string `json:"source"`
			SHA256 string `json:"sha256"`
		} `json:"provisionedHelpers"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	if len(manifest.ProvisionedHelpers) != 2 {
		t.Fatalf("provisioned helpers = %#v", manifest.ProvisionedHelpers)
	}
	for _, helper := range manifest.ProvisionedHelpers {
		if helper.Name == "" || helper.Path == "" || helper.Source == "" || len(helper.SHA256) != 64 {
			t.Fatalf("helper provenance = %#v", helper)
		}
	}
}

func TestPrepareFixtureReplacesUnmarkedDefaultRootWithForce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture requires POSIX execution")
	}
	repo := repoRootForTest(t)
	dir := t.TempDir()
	sourceRoot := filepath.Join(dir, "source-root")
	for _, path := range []string{
		"usr/bin/sh",
		"usr/bin/cp",
		"usr/bin/grep",
		"usr/bin/mktemp",
		"usr/bin/systemd-analyze",
	} {
		writeGuestExecutable(t, filepath.Join(sourceRoot, path))
	}
	stateDir := filepath.Join(dir, "state")
	staleRoot := filepath.Join(stateDir, "root")
	if err := os.MkdirAll(staleRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleRoot, "stale"), []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "prepare-nspawn-userspace-fixture"),
		"--source-root", sourceRoot,
		"--state-dir", stateDir,
		"--force",
	)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOCACHE=/tmp/katl-go-cache")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prepare fixture error = %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(staleRoot, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale file stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(staleRoot, ".katl-nspawn-fixture")); err != nil {
		t.Fatalf("managed marker missing after replacement: %v", err)
	}
}

func TestPrepareDefaultRootLeavesOverridesAlone(t *testing.T) {
	options := Options{Enabled: true, Root: "/custom/root"}
	if err := PrepareDefaultRoot(context.Background(), &options, "/missing/repo"); err != nil {
		t.Fatalf("PrepareDefaultRoot() with explicit root error = %v", err)
	}
	if options.Root != "/custom/root" {
		t.Fatalf("Root = %q", options.Root)
	}

	options = Options{Enabled: true, Image: "/custom/root.raw"}
	if err := PrepareDefaultRoot(context.Background(), &options, "/missing/repo"); err != nil {
		t.Fatalf("PrepareDefaultRoot() with explicit image error = %v", err)
	}
	if options.Image != "/custom/root.raw" || options.Root != "" {
		t.Fatalf("options = %#v", options)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}

func writeGuestExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
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
