package nspawntest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type KeepPolicy string

const (
	KeepNever  KeepPolicy = "never"
	KeepFailed KeepPolicy = "failed"
	KeepAlways KeepPolicy = "always"
)

type MissingPolicy string

const (
	MissingFails MissingPolicy = "fail"
	MissingSkips MissingPolicy = "skip"
)

type Status string

const (
	StatusPlanned Status = "planned"
	StatusPassed  Status = "passed"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

type Scenario struct {
	Name      string     `json:"name"`
	RunID     string     `json:"runId,omitempty"`
	StateRoot string     `json:"stateRoot,omitempty"`
	Root      string     `json:"root,omitempty"`
	Image     string     `json:"image,omitempty"`
	Keep      KeepPolicy `json:"keep,omitempty"`
	Commands  []Command  `json:"commands,omitempty"`
	Binds     []Bind     `json:"binds,omitempty"`
}

type Command struct {
	Name       string            `json:"name,omitempty"`
	Argv       []string          `json:"argv"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Timeout    time.Duration     `json:"timeout,omitempty"`
}

type Bind struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type Options struct {
	Enabled           bool
	StateRoot         string
	Root              string
	Image             string
	Keep              KeepPolicy
	Missing           MissingPolicy
	RunID             string
	AllowUnprivileged bool
}

type Runner struct {
	Options Options
	probe   probe
	command commandRunner
	now     func() time.Time
}

type Result struct {
	ScenarioName   string                `json:"scenarioName"`
	Status         Status                `json:"status"`
	RunID          string                `json:"runId"`
	RunDir         string                `json:"runDir"`
	Root           RootIdentity          `json:"root"`
	Keep           KeepPolicy            `json:"keep"`
	Started        time.Time             `json:"started,omitempty"`
	Finished       time.Time             `json:"finished,omitempty"`
	DurationMS     int64                 `json:"durationMs,omitempty"`
	FailureSummary string                `json:"failureSummary,omitempty"`
	Artifacts      ArtifactPaths         `json:"artifacts"`
	Commands       []CommandArtifact     `json:"commands,omitempty"`
	Missing        []MissingPrerequisite `json:"missing,omitempty"`
}

type RootIdentity struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Hint      string `json:"-"`
	Device    uint64 `json:"device,omitempty"`
	Inode     uint64 `json:"inode,omitempty"`
	Mode      string `json:"mode,omitempty"`
	OSRelease string `json:"osRelease,omitempty"`
}

type ArtifactPaths struct {
	Scenario string `json:"scenario"`
	Result   string `json:"result"`
	Commands string `json:"commandsDir"`
}

type CommandArtifact struct {
	Name       string    `json:"name"`
	Argv       []string  `json:"argv"`
	WorkingDir string    `json:"workingDir,omitempty"`
	Dir        string    `json:"dir"`
	Stdout     string    `json:"stdout"`
	Stderr     string    `json:"stderr"`
	Command    string    `json:"command"`
	ExitStatus int       `json:"exitStatus"`
	Error      string    `json:"error,omitempty"`
	Started    time.Time `json:"started,omitempty"`
	Finished   time.Time `json:"finished,omitempty"`
	DurationMS int64     `json:"durationMs,omitempty"`
}

type MissingPrerequisite struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type PrereqError struct {
	Missing []MissingPrerequisite
}

func (e PrereqError) Error() string {
	if len(e.Missing) == 0 {
		return "nspawn prerequisites missing"
	}
	parts := make([]string, 0, len(e.Missing))
	for _, missing := range e.Missing {
		if missing.Detail == "" {
			parts = append(parts, missing.Name)
			continue
		}
		parts = append(parts, missing.Name+": "+missing.Detail)
	}
	return "nspawn prerequisites missing: " + strings.Join(parts, "; ")
}

type testTB interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

var (
	runFlag       = flag.Bool("katl.nspawn.run", false, "run Katl systemd-nspawn scenarios")
	stateRootFlag = flag.String("katl.nspawn.state-root", "", "Katl nspawn scenario state root")
	rootFlag      = flag.String("katl.nspawn.root", "", "prepared Katl or Fedora userspace root for nspawn scenarios")
	imageFlag     = flag.String("katl.nspawn.image", "", "prepared Katl or Fedora disk image for nspawn scenarios")
	keepFlag      = flag.String("katl.nspawn.keep", "", "Katl nspawn artifact keep policy: never, failed, or always")
)

func DefaultOptions() Options {
	return Options{
		Enabled:           *runFlag || envBool("KATL_NSPAWN_RUN"),
		StateRoot:         first(*stateRootFlag, os.Getenv("KATL_NSPAWN_STATE_ROOT")),
		Root:              first(*rootFlag, os.Getenv("KATL_NSPAWN_ROOT")),
		Image:             first(*imageFlag, os.Getenv("KATL_NSPAWN_IMAGE")),
		Keep:              KeepPolicy(first(*keepFlag, os.Getenv("KATL_NSPAWN_KEEP"))),
		Missing:           MissingFails,
		AllowUnprivileged: envBool("KATL_NSPAWN_ALLOW_UNPRIVILEGED"),
	}
}

func NewRunner(options Options) Runner {
	return Runner{
		Options: options,
		probe:   systemProbe(),
		command: execRunner{},
	}
}

func Run(t testing.TB, scenario Scenario) Result {
	return NewRunner(DefaultOptions()).Run(t, scenario)
}

func CheckHost(root string, binds []Bind) error {
	return checkHost(rootRef{Kind: rootKindDirectory, Path: root}, binds, systemProbe(), false)
}

func (r Runner) Run(t testTB, scenario Scenario) Result {
	t.Helper()
	result, err := r.Plan(scenario)
	if err != nil {
		t.Fatalf("nspawn plan failed: %v", err)
		return result
	}
	if !r.options().Enabled {
		result.Status = StatusSkipped
		result.FailureSummary = "nspawn scenario is not enabled"
		t.Skipf("set -katl.nspawn.run or KATL_NSPAWN_RUN=1 to run nspawn scenario %q", scenario.Name)
		return result
	}
	result.start(r.time())
	if err := r.check(rootRef{Kind: result.Root.Kind, Path: result.Root.Path, Hint: result.Root.Hint}, scenario.Binds); err != nil {
		status := StatusFailed
		if r.options().Missing == MissingSkips {
			status = StatusSkipped
		}
		result.finish(status, err.Error(), r.time())
		var prereq PrereqError
		if errors.As(err, &prereq) {
			result.Missing = prereq.Missing
		}
		if writeErr := r.Write(scenario, result); writeErr != nil {
			t.Fatalf("write nspawn result for %q failed: %v\nnspawn run dir: %s", scenario.Name, writeErr, result.RunDir)
			return result
		}
		if r.options().Missing == MissingSkips {
			t.Skipf("%v", err)
			return result
		}
		t.Fatalf("%v\nnspawn run dir: %s", err, result.RunDir)
		return result
	}
	for i, command := range scenario.Commands {
		record, err := r.runCommand(context.Background(), result, scenario.Binds, i, command)
		result.Commands = append(result.Commands, record)
		if err != nil {
			result.finish(StatusFailed, err.Error(), r.time())
			if writeErr := r.Write(scenario, result); writeErr != nil {
				t.Fatalf("write nspawn result for %q failed: %v\nnspawn run dir: %s", scenario.Name, writeErr, result.RunDir)
				return result
			}
			t.Fatalf("%v\nnspawn run dir: %s", err, result.RunDir)
			return result
		}
	}
	result.finish(StatusPassed, "", r.time())
	if err := r.Write(scenario, result); err != nil {
		t.Fatalf("write nspawn result for %q failed: %v\nnspawn run dir: %s", scenario.Name, err, result.RunDir)
	}
	return result
}

func (r Runner) Plan(scenario Scenario) (Result, error) {
	options := r.options()
	scenario = normalizeScenario(scenario, options)
	if strings.TrimSpace(scenario.Name) == "" {
		return Result{}, errors.New("scenario name is required")
	}
	root, err := resolveRoot(scenario, options)
	if err != nil {
		return Result{}, err
	}
	runID := first(scenario.RunID, options.RunID)
	if runID == "" {
		runID = fmt.Sprintf("%s-%d", clean(scenario.Name), r.time().Unix())
	}
	runDir := filepath.Join(scenario.StateRoot, runID)
	return Result{
		ScenarioName: scenario.Name,
		Status:       StatusPlanned,
		RunID:        runID,
		RunDir:       runDir,
		Root:         rootIdentity(root, r.probe.withDefaults()),
		Keep:         scenario.Keep,
		Artifacts: ArtifactPaths{
			Scenario: filepath.Join(runDir, "scenario.json"),
			Result:   filepath.Join(runDir, "result.json"),
			Commands: filepath.Join(runDir, "commands"),
		},
	}, nil
}

func (r Runner) Write(scenario Scenario, result Result) error {
	if err := os.MkdirAll(result.Artifacts.Commands, 0o755); err != nil {
		return err
	}
	if err := writeJSON(result.Artifacts.Scenario, scenarioRecord{Scenario: scenario, Result: result}); err != nil {
		return err
	}
	return writeJSON(result.Artifacts.Result, result)
}

func (r Runner) check(root rootRef, binds []Bind) error {
	return checkHost(root, binds, r.probe.withDefaults(), r.options().AllowUnprivileged)
}

func (r Runner) runCommand(ctx context.Context, result Result, binds []Bind, index int, command Command) (CommandArtifact, error) {
	if len(command.Argv) == 0 {
		return CommandArtifact{}, errors.New("nspawn command argv is required")
	}
	name := first(command.Name, filepath.Base(command.Argv[0]))
	dir := filepath.Join(result.Artifacts.Commands, fmt.Sprintf("%02d-%s", index, clean(name)))
	record := CommandArtifact{
		Name:       name,
		Argv:       append([]string(nil), command.Argv...),
		WorkingDir: command.WorkingDir,
		Dir:        dir,
		Stdout:     filepath.Join(dir, "stdout"),
		Stderr:     filepath.Join(dir, "stderr"),
		Command:    filepath.Join(dir, "nspawn-command.txt"),
		ExitStatus: -1,
		Started:    r.time(),
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return record, err
	}
	argv, err := nspawnArgv(rootRef{Kind: result.Root.Kind, Path: result.Root.Path}, binds, command)
	if err != nil {
		record.Error = err.Error()
		_ = writeJSON(filepath.Join(dir, "command.json"), record)
		return record, err
	}
	if err := writeArtifact(record.Command, []byte(strings.Join(shellQuote(argv), " ")+"\n"), 0o644); err != nil {
		return record, err
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()
	err = r.runner().Run(runCtx, argv[0], argv[1:], stdout, stderr)
	record.Finished = r.time()
	record.DurationMS = record.Finished.Sub(record.Started).Milliseconds()
	record.ExitStatus = exitStatus(err)
	if err != nil {
		record.Error = err.Error()
	}
	if writeErr := writeArtifact(record.Stdout, stdout.Bytes(), 0o644); writeErr != nil {
		return record, writeErr
	}
	if writeErr := writeArtifact(record.Stderr, stderr.Bytes(), 0o644); writeErr != nil {
		return record, writeErr
	}
	if writeErr := writeJSON(filepath.Join(dir, "command.json"), record); writeErr != nil {
		return record, writeErr
	}
	if err != nil {
		return record, fmt.Errorf("nspawn command %q failed: %w", name, err)
	}
	return record, nil
}

func (r Runner) options() Options {
	return normalizeOptions(r.Options)
}

func (r Runner) runner() commandRunner {
	if r.command != nil {
		return r.command
	}
	return execRunner{}
}

func (r Runner) time() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

func (r *Result) start(now time.Time) {
	r.Started = now
}

func (r *Result) finish(status Status, failure string, now time.Time) {
	r.Status = status
	r.Finished = now
	if !r.Started.IsZero() {
		r.DurationMS = r.Finished.Sub(r.Started).Milliseconds()
	}
	r.FailureSummary = failure
}

func normalizeOptions(options Options) Options {
	if options.StateRoot == "" {
		options.StateRoot = filepath.Join("build", "nspawn")
	}
	if options.Keep == "" {
		options.Keep = KeepFailed
	}
	if options.Missing == "" {
		options.Missing = MissingFails
	}
	return options
}

func normalizeScenario(scenario Scenario, options Options) Scenario {
	if scenario.StateRoot == "" {
		scenario.StateRoot = options.StateRoot
	}
	if scenario.Keep == "" {
		scenario.Keep = options.Keep
	}
	return scenario
}

func checkHost(root rootRef, binds []Bind, probe probe, allowUnprivileged bool) error {
	probe = probe.withDefaults()
	var missing []MissingPrerequisite
	missing = appendCommand(missing, probe, "systemd-nspawn")
	if probe.euid() != 0 && !allowUnprivileged {
		missing = append(missing, MissingPrerequisite{
			Name:   "nspawn privileges",
			Detail: "run as root or set KATL_NSPAWN_ALLOW_UNPRIVILEGED=1 after configuring rootless systemd-nspawn support",
		})
	}
	if root.Path == "" {
		missing = append(missing, MissingPrerequisite{Name: "nspawn root", Detail: "set KATL_NSPAWN_ROOT, KATL_NSPAWN_IMAGE, Scenario.Root, or Scenario.Image"})
	} else {
		switch root.Kind {
		case rootKindImage:
			missing = appendImage(missing, probe, root.Path)
		default:
			missing = appendDir(missing, probe, "nspawn root", root.Path, root.Hint)
		}
	}
	for _, bind := range binds {
		if strings.TrimSpace(bind.Source) == "" {
			missing = append(missing, MissingPrerequisite{Name: "nspawn bind source", Detail: "source path is required"})
			continue
		}
		missing = appendPath(missing, probe, "nspawn bind source", bind.Source)
		if strings.TrimSpace(bind.Target) == "" || !filepath.IsAbs(bind.Target) {
			missing = append(missing, MissingPrerequisite{Name: "nspawn bind target", Detail: "target must be an absolute guest path"})
		}
	}
	if len(missing) > 0 {
		return PrereqError{Missing: missing}
	}
	return nil
}

func nspawnArgv(root rootRef, binds []Bind, command Command) ([]string, error) {
	if len(command.Argv) == 0 {
		return nil, errors.New("nspawn command argv is required")
	}
	argv := []string{
		"systemd-nspawn",
		"--quiet",
		"--settings=no",
		"--volatile=state",
		"--setenv", "SYSTEMD_LOG_LEVEL=warning",
	}
	switch root.Kind {
	case rootKindImage:
		argv = append(argv, "--image", root.Path)
	default:
		argv = append(argv, "--directory", root.Path)
	}
	if command.WorkingDir != "" {
		if !filepath.IsAbs(command.WorkingDir) {
			return nil, fmt.Errorf("nspawn working directory %q must be absolute", command.WorkingDir)
		}
		argv = append(argv, "--chdir", command.WorkingDir)
	}
	for _, bind := range binds {
		if bind.Target == "" || !filepath.IsAbs(bind.Target) {
			return nil, fmt.Errorf("nspawn bind target %q must be absolute", bind.Target)
		}
		argv = append(argv, "--bind-ro", bind.Source+":"+bind.Target)
	}
	names := make([]string, 0, len(command.Env))
	for name := range command.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := command.Env[name]
		if strings.TrimSpace(name) == "" || strings.Contains(name, "=") {
			return nil, fmt.Errorf("nspawn environment name %q is invalid", name)
		}
		argv = append(argv, "--setenv", name+"="+value)
	}
	argv = append(argv, "--")
	argv = append(argv, command.Argv...)
	return argv, nil
}

func appendCommand(missing []MissingPrerequisite, probe probe, name string) []MissingPrerequisite {
	if _, err := probe.lookPath(name); err != nil {
		return append(missing, MissingPrerequisite{Name: name, Detail: "not found in PATH"})
	}
	return missing
}

func appendDir(missing []MissingPrerequisite, probe probe, name, path, hint string) []MissingPrerequisite {
	info, err := probe.stat(path)
	if err != nil {
		detail := path + ": " + err.Error()
		if hint != "" {
			detail += "; " + hint
		}
		return append(missing, MissingPrerequisite{Name: name, Detail: detail})
	}
	if !info.IsDir() {
		return append(missing, MissingPrerequisite{Name: name, Detail: path + ": not a directory"})
	}
	return missing
}

func appendImage(missing []MissingPrerequisite, probe probe, path string) []MissingPrerequisite {
	info, err := probe.stat(path)
	if err != nil {
		return append(missing, MissingPrerequisite{Name: "nspawn image", Detail: path + ": " + err.Error()})
	}
	mode := info.Mode()
	if mode.IsRegular() || mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0 {
		return missing
	}
	return append(missing, MissingPrerequisite{Name: "nspawn image", Detail: path + ": not a regular file or block device"})
}

func appendPath(missing []MissingPrerequisite, probe probe, name, path string) []MissingPrerequisite {
	if _, err := probe.stat(path); err != nil {
		return append(missing, MissingPrerequisite{Name: name, Detail: path + ": " + err.Error()})
	}
	return missing
}

const (
	rootKindDirectory = "directory"
	rootKindImage     = "image"
)

type rootRef struct {
	Kind string
	Path string
	Hint string
}

func resolveRoot(scenario Scenario, options Options) (rootRef, error) {
	root := first(strings.TrimSpace(scenario.Root), strings.TrimSpace(options.Root))
	image := first(strings.TrimSpace(scenario.Image), strings.TrimSpace(options.Image))
	if root != "" && image != "" {
		return rootRef{}, errors.New("nspawn scenario must set only one of root or image")
	}
	if image != "" {
		return rootRef{Kind: rootKindImage, Path: image}, nil
	}
	if root == "" {
		root = filepath.Join("build", "nspawn", "root")
		return rootRef{
			Kind: rootKindDirectory,
			Path: root,
			Hint: "set KATL_NSPAWN_ROOT, -katl.nspawn.root, Scenario.Root, KATL_NSPAWN_IMAGE, or Scenario.Image",
		}, nil
	}
	return rootRef{Kind: rootKindDirectory, Path: root}, nil
}

func rootIdentity(root rootRef, probe probe) RootIdentity {
	identity := RootIdentity{Kind: root.Kind, Path: root.Path, Hint: root.Hint}
	info, err := probe.withDefaults().stat(root.Path)
	if err == nil {
		identity.Mode = info.Mode().String()
		if stat, ok := fileSys(info); ok {
			identity.Device = stat.dev
			identity.Inode = stat.ino
		}
	}
	data, err := probe.withDefaults().readFile(filepath.Join(root.Path, "etc/os-release"))
	if err == nil && root.Kind == rootKindDirectory {
		identity.OSRelease = summarizeOSRelease(data)
	}
	return identity
}

func summarizeOSRelease(data []byte) string {
	var parts []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID=") || strings.HasPrefix(line, "VERSION_ID=") || strings.HasPrefix(line, "IMAGE_ID=") || strings.HasPrefix(line, "IMAGE_VERSION=") {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}

type scenarioRecord struct {
	Scenario Scenario `json:"scenario"`
	Result   Result   `json:"result"`
}

type probe struct {
	lookPath func(string) (string, error)
	stat     func(string) (fs.FileInfo, error)
	env      func(string) string
	readFile func(string) ([]byte, error)
	euid     func() int
}

func systemProbe() probe {
	return probe{
		lookPath: exec.LookPath,
		stat:     os.Stat,
		env:      os.Getenv,
		readFile: os.ReadFile,
		euid:     os.Geteuid,
	}
}

func (p probe) withDefaults() probe {
	if p.lookPath == nil {
		p.lookPath = exec.LookPath
	}
	if p.stat == nil {
		p.stat = os.Stat
	}
	if p.env == nil {
		p.env = os.Getenv
	}
	if p.readFile == nil {
		p.readFile = os.ReadFile
	}
	if p.euid == nil {
		p.euid = os.Geteuid
	}
	return p
}

type commandRunner interface {
	Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeArtifact(path, append(data, '\n'), 0o644)
}

func writeArtifact(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func envBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func clean(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func shellQuote(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg == "" {
			out = append(out, "''")
			continue
		}
		if strings.IndexFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%_+=:,./-", r))
		}) == -1 {
			out = append(out, arg)
			continue
		}
		out = append(out, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
	}
	return out
}
