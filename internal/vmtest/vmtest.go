package vmtest

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type KVMPolicy string

const (
	KVMAuto KVMPolicy = "auto"
	KVMOn   KVMPolicy = "on"
	KVMOff  KVMPolicy = "off"
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

type Scenario struct {
	Name      string           `json:"name"`
	RunID     string           `json:"runId,omitempty"`
	StateRoot string           `json:"stateRoot,omitempty"`
	Keep      KeepPolicy       `json:"keep,omitempty"`
	KVM       KVMPolicy        `json:"kvm,omitempty"`
	Host      HostRequirements `json:"host,omitempty"`
	Disks     []DiskFixture    `json:"disks,omitempty"`
}

type HostRequirements struct {
	QEMU     bool      `json:"qemu,omitempty"`
	QEMUImg  bool      `json:"qemuImg,omitempty"`
	OVMF     bool      `json:"ovmf,omitempty"`
	KVM      KVMPolicy `json:"kvm,omitempty"`
	OVMFCode string    `json:"ovmfCode,omitempty"`
	OVMFVars string    `json:"ovmfVars,omitempty"`
}

type Options struct {
	Enabled   bool
	StateRoot string
	Keep      KeepPolicy
	KVM       KVMPolicy
	Missing   MissingPolicy
	RunID     string
}

type Runner struct {
	Options Options
	probe   probe
	now     func() time.Time
}

type Result struct {
	ScenarioName   string                `json:"scenarioName"`
	Status         Status                `json:"status"`
	RunID          string                `json:"runId"`
	RunDir         string                `json:"runDir"`
	QEMUDir        string                `json:"qemuDir"`
	DiskDir        string                `json:"diskDir"`
	ManifestDir    string                `json:"manifestDir"`
	Keep           KeepPolicy            `json:"keep"`
	KVM            KVMPolicy             `json:"kvm"`
	Started        time.Time             `json:"started,omitempty"`
	Finished       time.Time             `json:"finished,omitempty"`
	DurationMS     int64                 `json:"durationMs,omitempty"`
	FailureSummary string                `json:"failureSummary,omitempty"`
	Artifacts      ArtifactPaths         `json:"artifacts"`
	Disks          []DiskPlan            `json:"disks,omitempty"`
	VSock          VSockPlan             `json:"vsock,omitempty"`
	Phases         []PhaseResult         `json:"phases,omitempty"`
	Missing        []MissingPrerequisite `json:"missing,omitempty"`
}

type Status string

const (
	StatusPlanned Status = "planned"
	StatusPassed  Status = "passed"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

type ArtifactPaths struct {
	Scenario             string `json:"scenario"`
	Result               string `json:"result"`
	QEMUCommand          string `json:"qemuCommand"`
	InstallerQEMUCommand string `json:"installerQEMUCommand,omitempty"`
	RuntimeQEMUCommand   string `json:"runtimeQEMUCommand,omitempty"`
	InstallerSerial      string `json:"installerSerial"`
	RuntimeSerial        string `json:"runtimeSerial"`
	InstallManifest      string `json:"installManifest,omitempty"`
	HandoffRequest       string `json:"handoffRequest,omitempty"`
	HandoffResponse      string `json:"handoffResponse,omitempty"`
	VSockTranscript      string `json:"vsockTranscript,omitempty"`
	ManifestsDir         string `json:"manifestsDir"`
	DisksDir             string `json:"disksDir"`
	GuestDir             string `json:"guestDir"`
}

type PhaseResult struct {
	Name           string    `json:"name"`
	Status         Status    `json:"status"`
	Started        time.Time `json:"started,omitempty"`
	Finished       time.Time `json:"finished,omitempty"`
	DurationMS     int64     `json:"durationMs,omitempty"`
	FailureSummary string    `json:"failureSummary,omitempty"`
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
		return "host prerequisites missing"
	}
	parts := make([]string, 0, len(e.Missing))
	for _, missing := range e.Missing {
		if missing.Detail == "" {
			parts = append(parts, missing.Name)
			continue
		}
		parts = append(parts, missing.Name+": "+missing.Detail)
	}
	return "host prerequisites missing: " + strings.Join(parts, "; ")
}

type testTB interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

var (
	runFlag       = flag.Bool("katl.vmtest.run", false, "run Katl VM scenarios")
	stateRootFlag = flag.String("katl.vmtest.state-root", "", "Katl VM scenario state root")
	keepFlag      = flag.String("katl.vmtest.keep", "", "Katl VM artifact keep policy: never, failed, or always")
	kvmFlag       = flag.String("katl.vmtest.kvm", "", "Katl VM KVM policy: auto, on, or off")
)

func DefaultOptions() Options {
	return Options{
		Enabled:   *runFlag || envBool("KATL_VMTEST_RUN"),
		StateRoot: first(*stateRootFlag, os.Getenv("KATL_VMTEST_STATE_ROOT")),
		Keep:      KeepPolicy(first(*keepFlag, os.Getenv("KATL_VMTEST_KEEP"))),
		KVM:       KVMPolicy(first(*kvmFlag, os.Getenv("KATL_VMTEST_KVM"))),
		Missing:   MissingFails,
	}
}

func NewRunner(options Options) Runner {
	return Runner{Options: options, probe: systemProbe()}
}

func Run(t testing.TB, scenario Scenario) Result {
	return NewRunner(DefaultOptions()).Run(t, scenario)
}

func RequireHost(t testing.TB, requirements HostRequirements) {
	NewRunner(DefaultOptions()).RequireHost(t, requirements)
}

func RequireEnv(t testing.TB, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("set %s to run this VM scenario", name)
	}
	return value
}

func CheckHost(requirements HostRequirements) error {
	return checkHost(requirements, systemProbe())
}

func (r Runner) Run(t testTB, scenario Scenario) Result {
	t.Helper()
	result, err := r.Plan(scenario)
	if err != nil {
		t.Fatalf("vmtest plan failed: %v", err)
		return result
	}
	if !r.options().Enabled {
		result.Status = StatusSkipped
		result.FailureSummary = "VM scenario is not enabled"
		t.Skipf("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run VM scenario %q", scenario.Name)
		return result
	}
	result.start(r.time())
	if err := r.check(scenario.Host); err != nil {
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
			t.Fatalf("write vmtest result for %q failed: %v\nvmtest run dir: %s", scenario.Name, writeErr, result.RunDir)
			return result
		}
		if r.options().Missing == MissingSkips {
			result.Status = StatusSkipped
			t.Skipf("%v", err)
			return result
		}
		t.Fatalf("%v\nvmtest run dir: %s", err, result.RunDir)
		return result
	}
	result.finish(StatusPassed, "", r.time())
	if err := r.Write(scenario, result); err != nil {
		t.Fatalf("write vmtest result for %q failed: %v\nvmtest run dir: %s", scenario.Name, err, result.RunDir)
	}
	return result
}

func (r Runner) RequireHost(t testTB, requirements HostRequirements) {
	t.Helper()
	if !r.options().Enabled {
		t.Skipf("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run VM host checks")
		return
	}
	if err := r.check(requirements); err != nil {
		if r.options().Missing == MissingSkips {
			t.Skipf("%v", err)
			return
		}
		t.Fatalf("%v", err)
	}
}

func (r Runner) Plan(scenario Scenario) (Result, error) {
	options := r.options()
	scenario = normalizeScenario(scenario, options)
	if scenario.Name == "" {
		return Result{}, errors.New("scenario name is required")
	}
	runID := first(scenario.RunID, options.RunID)
	if runID == "" {
		runID = fmt.Sprintf("%s-%d", clean(scenario.Name), time.Now().UTC().Unix())
	}
	runDir := filepath.Join(scenario.StateRoot, runID)
	paths := pathsFor(runDir)
	disks, err := planDisks(filepath.Join(runDir, "disks"), scenario.Disks)
	if err != nil {
		return Result{}, err
	}
	return Result{
		ScenarioName: scenario.Name,
		Status:       StatusPlanned,
		RunID:        runID,
		RunDir:       runDir,
		QEMUDir:      filepath.Join(runDir, "qemu"),
		DiskDir:      filepath.Join(runDir, "disks"),
		ManifestDir:  filepath.Join(runDir, "manifests"),
		Keep:         scenario.Keep,
		KVM:          scenario.KVM,
		Artifacts:    paths,
		Disks:        disks,
	}, nil
}

func (r Runner) Write(scenario Scenario, result Result) error {
	if err := os.MkdirAll(result.QEMUDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(result.DiskDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(result.ManifestDir, 0o755); err != nil {
		return err
	}
	record := scenarioRecord{
		Scenario: scenario,
		Result:   result,
	}
	if err := writeJSON(result.Artifacts.Scenario, record); err != nil {
		return err
	}
	return writeJSON(result.Artifacts.Result, result)
}

func (r Runner) check(requirements HostRequirements) error {
	return checkHost(requirements, r.probe.withDefaults())
}

func (r Runner) options() Options {
	return normalizeOptions(r.Options)
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
	r.addPhase("host-prerequisites", status, failure, r.Started, now)
}

func (r *Result) addPhase(name string, status Status, failure string, started, finished time.Time) {
	phase := PhaseResult{
		Name:           name,
		Status:         status,
		Started:        started,
		Finished:       finished,
		FailureSummary: failure,
	}
	if !started.IsZero() && !finished.IsZero() {
		phase.DurationMS = finished.Sub(started).Milliseconds()
	}
	r.Phases = append(r.Phases, phase)
}

func normalizeOptions(options Options) Options {
	if options.StateRoot == "" {
		options.StateRoot = filepath.Join("build", "vmtest")
	}
	if options.Keep == "" {
		options.Keep = KeepFailed
	}
	if options.KVM == "" {
		options.KVM = KVMAuto
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
	if scenario.KVM == "" {
		scenario.KVM = options.KVM
	}
	if scenario.Host.KVM == "" {
		scenario.Host.KVM = scenario.KVM
	}
	return scenario
}

func checkHost(requirements HostRequirements, probe probe) error {
	probe = probe.withDefaults()
	var missing []MissingPrerequisite
	if requirements.QEMU {
		missing = appendCommand(missing, probe, "qemu-system-x86_64")
	}
	if requirements.QEMUImg {
		missing = appendCommand(missing, probe, "qemu-img")
	}
	if requirements.OVMF {
		code := first(requirements.OVMFCode, probe.env("KATL_OVMF_CODE"))
		vars := first(requirements.OVMFVars, probe.env("KATL_OVMF_VARS"))
		missing = appendFile(missing, probe, "OVMF code", code, "set KATL_OVMF_CODE or Scenario.Host.OVMFCode")
		missing = appendFile(missing, probe, "OVMF vars", vars, "set KATL_OVMF_VARS or Scenario.Host.OVMFVars")
	}
	if requirements.KVM == KVMOn {
		if err := probe.access("/dev/kvm"); err != nil {
			missing = append(missing, MissingPrerequisite{
				Name:   "/dev/kvm",
				Detail: "required by KVM policy on: " + err.Error(),
			})
		}
	}
	if len(missing) > 0 {
		return PrereqError{Missing: missing}
	}
	return nil
}

func appendCommand(missing []MissingPrerequisite, probe probe, name string) []MissingPrerequisite {
	if _, err := probe.lookPath(name); err != nil {
		return append(missing, MissingPrerequisite{Name: name, Detail: "not found in PATH"})
	}
	return missing
}

func appendFile(missing []MissingPrerequisite, probe probe, name, path, hint string) []MissingPrerequisite {
	if path == "" {
		return append(missing, MissingPrerequisite{Name: name, Detail: hint})
	}
	if _, err := probe.stat(path); err != nil {
		return append(missing, MissingPrerequisite{Name: name, Detail: path + ": " + err.Error()})
	}
	return missing
}

type probe struct {
	lookPath func(string) (string, error)
	stat     func(string) (fs.FileInfo, error)
	access   func(string) error
	env      func(string) string
	output   func(string, ...string) ([]byte, error)
}

func systemProbe() probe {
	return probe{
		lookPath: exec.LookPath,
		stat:     os.Stat,
		access: func(path string) error {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return file.Close()
		},
		env: os.Getenv,
		output: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
	}
}

func (p probe) withDefaults() probe {
	if p.lookPath == nil {
		p.lookPath = exec.LookPath
	}
	if p.stat == nil {
		p.stat = os.Stat
	}
	if p.access == nil {
		p.access = func(path string) error {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return file.Close()
		}
	}
	if p.env == nil {
		p.env = os.Getenv
	}
	if p.output == nil {
		p.output = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		}
	}
	return p
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

func pathsFor(runDir string) ArtifactPaths {
	return ArtifactPaths{
		Scenario:             filepath.Join(runDir, "scenario.json"),
		Result:               filepath.Join(runDir, "result.json"),
		QEMUCommand:          filepath.Join(runDir, "qemu", "qemu-command.txt"),
		InstallerQEMUCommand: filepath.Join(runDir, "qemu", "installer-qemu-command.txt"),
		RuntimeQEMUCommand:   filepath.Join(runDir, "qemu", "runtime-qemu-command.txt"),
		InstallerSerial:      filepath.Join(runDir, "qemu", "installer-serial.log"),
		RuntimeSerial:        filepath.Join(runDir, "qemu", "runtime-serial.log"),
		InstallManifest:      filepath.Join(runDir, "manifests", "install-manifest.json"),
		HandoffRequest:       filepath.Join(runDir, "manifests", "handoff-request.json"),
		HandoffResponse:      filepath.Join(runDir, "manifests", "handoff-response.json"),
		VSockTranscript:      filepath.Join(runDir, "qemu", "vsock-transcript.jsonl"),
		ManifestsDir:         filepath.Join(runDir, "manifests"),
		DisksDir:             filepath.Join(runDir, "disks"),
		GuestDir:             filepath.Join(runDir, "guest"),
	}
}

type scenarioRecord struct {
	Scenario Scenario `json:"scenario"`
	Result   Result   `json:"result"`
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
