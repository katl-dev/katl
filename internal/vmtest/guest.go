package vmtest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

type GuestAgentClient interface {
	RunCommand(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error)
	ReadFile(ctx context.Context, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error)
	WriteFile(ctx context.Context, req *vmtestpb.WriteFileRequest) (*vmtestpb.WriteFileResult, error)
	ExportJournal(ctx context.Context, req *vmtestpb.ExportJournalRequest) (*vmtestpb.JournalResult, error)
}

type GuestControl struct {
	Result Result
	Client GuestAgentClient

	Timeout       time.Duration
	OutputLimit   uint32
	FileLimit     uint32
	JournalLimit  uint32
	diagnosticSeq int
}

type GuestCommandRequest struct {
	Name            string
	Argv            []string
	WorkingDir      string
	Environment     []*vmtestpb.EnvVar
	Timeout         time.Duration
	StdoutLimit     uint32
	StderrLimit     uint32
	SensitiveOutput bool
	AllowFailure    bool
}

type GuestCommandArtifact struct {
	Name            string   `json:"name"`
	Argv            []string `json:"argv"`
	Dir             string   `json:"dir"`
	Stdout          string   `json:"stdout,omitempty"`
	Stderr          string   `json:"stderr,omitempty"`
	ExitStatus      int32    `json:"exitStatus"`
	StdoutBytes     uint32   `json:"stdoutBytes"`
	StderrBytes     uint32   `json:"stderrBytes"`
	StdoutTruncated bool     `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool     `json:"stderrTruncated,omitempty"`
	Redaction       string   `json:"redaction,omitempty"`
	AllowFailure    bool     `json:"allowFailure,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type GuestFileRequest struct {
	Name         string
	Path         string
	Content      []byte
	Mode         fs.FileMode
	Timeout      time.Duration
	MaxBytes     uint32
	StoreContent bool
	Sensitive    bool
	Offset       uint64
	Truncate     bool
}

type GuestFileArtifact struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Dir       string `json:"dir"`
	Artifact  string `json:"artifact,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	SizeBytes uint32 `json:"sizeBytes"`
	Redaction string `json:"redaction,omitempty"`
	Error     string `json:"error,omitempty"`
}

type GuestJournalRequest struct {
	Name     string
	Units    []string
	BootID   string
	Timeout  time.Duration
	MaxBytes uint32
}

type GuestJournalArtifact struct {
	Name      string   `json:"name"`
	Units     []string `json:"units,omitempty"`
	Artifact  string   `json:"artifact,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	SizeBytes uint32   `json:"sizeBytes"`
	Error     string   `json:"error,omitempty"`
}

type GuestDiagnostics struct {
	Commands []GuestCommandRequest
	Files    []GuestFileRequest
	Journals []GuestJournalRequest
	Timeout  time.Duration
}

type GuestDiagnosticReport struct {
	Commands []GuestCommandArtifact `json:"commands,omitempty"`
	Files    []GuestFileArtifact    `json:"files,omitempty"`
	Journals []GuestJournalArtifact `json:"journals,omitempty"`
	Errors   []string               `json:"errors,omitempty"`
}

func NewGuestControl(result Result, client GuestAgentClient) *GuestControl {
	return &GuestControl{
		Result:       result,
		Client:       client,
		Timeout:      10 * time.Second,
		OutputLimit:  256 << 10,
		FileLimit:    256 << 10,
		JournalLimit: 512 << 10,
	}
}

func (g *GuestControl) Systemctl(ctx context.Context, args ...string) (GuestCommandArtifact, error) {
	return g.RunCommand(ctx, GuestCommandRequest{
		Name: "systemctl",
		Argv: append([]string{"systemctl"}, args...),
	})
}

func (g *GuestControl) Findmnt(ctx context.Context, args ...string) (GuestCommandArtifact, error) {
	return g.RunCommand(ctx, GuestCommandRequest{
		Name: "findmnt",
		Argv: append([]string{"findmnt"}, args...),
	})
}

func (g *GuestControl) RunCommand(ctx context.Context, req GuestCommandRequest) (GuestCommandArtifact, error) {
	if g.Client == nil {
		return GuestCommandArtifact{}, errors.New("vmtest guest client is nil")
	}
	if len(req.Argv) == 0 {
		return GuestCommandArtifact{}, errors.New("guest command argv is required")
	}
	name := first(req.Name, filepath.Base(req.Argv[0]))
	dir := g.nextDir("commands", name)
	record := GuestCommandArtifact{
		Name:         name,
		Argv:         append([]string(nil), req.Argv...),
		Dir:          dir,
		ExitStatus:   -1,
		AllowFailure: req.AllowFailure,
	}
	ctx, cancel := g.withTimeout(ctx, req.Timeout)
	defer cancel()
	result, err := g.Client.RunCommand(ctx, &vmtestpb.RunCommandRequest{
		Argv:             req.Argv,
		WorkingDirectory: req.WorkingDir,
		Environment:      req.Environment,
		StdoutLimit:      firstLimit(req.StdoutLimit, g.OutputLimit),
		StderrLimit:      firstLimit(req.StderrLimit, g.OutputLimit),
		SensitiveOutput:  req.SensitiveOutput,
	})
	if err != nil {
		record.Error = err.Error()
		_ = g.writeRecord(filepath.Join(dir, "command.json"), record)
		return record, err
	}
	record.ExitStatus = result.ExitStatus
	record.StdoutBytes = result.StdoutBytes
	record.StderrBytes = result.StderrBytes
	record.StdoutTruncated = result.StdoutTruncated
	record.StderrTruncated = result.StderrTruncated
	if req.SensitiveOutput {
		record.Redaction = "output"
	} else {
		record.Stdout = filepath.Join(dir, "stdout")
		record.Stderr = filepath.Join(dir, "stderr")
		if err := writeArtifact(record.Stdout, result.Stdout, 0o644); err != nil {
			return record, err
		}
		if err := writeArtifact(record.Stderr, result.Stderr, 0o644); err != nil {
			return record, err
		}
	}
	if err := g.writeRecord(filepath.Join(dir, "command.json"), record); err != nil {
		return record, err
	}
	if result.ExitStatus != 0 && !req.AllowFailure {
		return record, fmt.Errorf("guest command %q exited %d", name, result.ExitStatus)
	}
	return record, nil
}

func (g *GuestControl) ReadFile(ctx context.Context, req GuestFileRequest) (GuestFileArtifact, error) {
	if g.Client == nil {
		return GuestFileArtifact{}, errors.New("vmtest guest client is nil")
	}
	if req.Path == "" {
		return GuestFileArtifact{}, errors.New("guest file path is required")
	}
	name := first(req.Name, filepath.Base(req.Path))
	dir := g.nextDir("files", name)
	record := GuestFileArtifact{
		Name: name,
		Path: req.Path,
		Dir:  dir,
	}
	ctx, cancel := g.withTimeout(ctx, req.Timeout)
	defer cancel()
	result, err := readGuestFileWithCarrierState(ctx, g.Client, &vmtestpb.ReadFileRequest{
		Path:      req.Path,
		MaxBytes:  firstLimit(req.MaxBytes, g.FileLimit),
		Sensitive: !req.StoreContent,
	})
	if err != nil {
		record.Error = err.Error()
		_ = g.writeRecord(filepath.Join(dir, "file.json"), record)
		return record, err
	}
	record.Truncated = result.Truncated
	record.SizeBytes = result.SizeBytes
	record.Redaction = result.Redaction
	if req.StoreContent {
		record.Artifact = filepath.Join(dir, "content")
		if err := writeArtifact(record.Artifact, result.Content, 0o600); err != nil {
			return record, err
		}
	} else if record.Redaction == "" || record.Redaction == "none" {
		record.Redaction = "content"
	}
	if err := g.writeRecord(filepath.Join(dir, "file.json"), record); err != nil {
		return record, err
	}
	return record, nil
}

func (g *GuestControl) WriteFile(ctx context.Context, req GuestFileRequest) (GuestFileArtifact, error) {
	if g.Client == nil {
		return GuestFileArtifact{}, errors.New("vmtest guest client is nil")
	}
	if req.Path == "" {
		return GuestFileArtifact{}, errors.New("guest file path is required")
	}
	name := first(req.Name, filepath.Base(req.Path))
	dir := g.nextDir("files", name)
	record := GuestFileArtifact{
		Name: name,
		Path: req.Path,
		Dir:  dir,
	}
	ctx, cancel := g.withTimeout(ctx, req.Timeout)
	defer cancel()
	result, err := g.Client.WriteFile(ctx, &vmtestpb.WriteFileRequest{
		Path:      req.Path,
		Content:   req.Content,
		Mode:      uint32(req.Mode.Perm()),
		Sensitive: req.Sensitive,
		Offset:    req.Offset,
		Truncate:  req.Truncate,
	})
	if err != nil {
		record.Error = err.Error()
		_ = g.writeRecord(filepath.Join(dir, "file.json"), record)
		return record, err
	}
	record.SizeBytes = result.SizeBytes
	record.Redaction = result.Redaction
	if record.Redaction == "" || record.Redaction == "none" {
		record.Redaction = "content"
	}
	if err := g.writeRecord(filepath.Join(dir, "file.json"), record); err != nil {
		return record, err
	}
	return record, nil
}

func (g *GuestControl) ExportJournal(ctx context.Context, req GuestJournalRequest) (GuestJournalArtifact, error) {
	if g.Client == nil {
		return GuestJournalArtifact{}, errors.New("vmtest guest client is nil")
	}
	name := first(req.Name, "journal")
	dir := g.nextDir("journals", name)
	record := GuestJournalArtifact{
		Name:  name,
		Units: append([]string(nil), req.Units...),
	}
	ctx, cancel := g.withTimeout(ctx, req.Timeout)
	defer cancel()
	result, err := g.Client.ExportJournal(ctx, &vmtestpb.ExportJournalRequest{
		Units:    req.Units,
		BootId:   req.BootID,
		MaxBytes: firstLimit(req.MaxBytes, g.JournalLimit),
	})
	if err != nil {
		record.Error = err.Error()
		_ = g.writeRecord(filepath.Join(dir, "journal.json"), record)
		return record, err
	}
	record.Artifact = filepath.Join(dir, "journal.log")
	record.Truncated = result.Truncated
	record.SizeBytes = result.SizeBytes
	if err := writeArtifact(record.Artifact, []byte(result.Text), 0o644); err != nil {
		return record, err
	}
	if err := g.writeRecord(filepath.Join(dir, "journal.json"), record); err != nil {
		return record, err
	}
	return record, nil
}

// CollectDiagnostics gathers the plan's evidence even when ctx is already done,
// as it is after a test fails on its deadline. Journals are exported first so a
// command that hangs until the plan timeout cannot starve them.
func (g *GuestControl) CollectDiagnostics(ctx context.Context, plan GuestDiagnostics) GuestDiagnosticReport {
	ctx, cancel := g.withTimeout(context.WithoutCancel(ctx), plan.Timeout)
	defer cancel()
	var report GuestDiagnosticReport
	for _, req := range plan.Journals {
		record, err := g.ExportJournal(ctx, req)
		report.Journals = append(report.Journals, record)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	for _, req := range plan.Commands {
		record, err := g.RunCommand(ctx, req)
		report.Commands = append(report.Commands, record)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	for _, req := range plan.Files {
		record, err := g.ReadFile(ctx, req)
		report.Files = append(report.Files, record)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	_ = g.writeRecord(filepath.Join(g.guestDir(), "diagnostics.json"), report)
	return report
}

func (g *GuestControl) withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		timeout = g.Timeout
	}
	if timeout == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func (g *GuestControl) nextDir(kind, name string) string {
	g.diagnosticSeq++
	return filepath.Join(g.guestDir(), kind, fmt.Sprintf("%03d-%s", g.diagnosticSeq, clean(name)))
}

func (g *GuestControl) guestDir() string {
	if g.Result.Artifacts.GuestDir != "" {
		return g.Result.Artifacts.GuestDir
	}
	return filepath.Join(g.Result.RunDir, "guest")
}

func (g *GuestControl) writeRecord(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, value)
}

func writeArtifact(path string, content []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, mode)
}

func firstLimit(values ...uint32) uint32 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
