package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

func TestGuestCommandRecordsArtifacts(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		command: &vmtestpb.CommandResult{
			ExitStatus:  0,
			Stdout:      []byte("active\n"),
			Stderr:      []byte(""),
			StdoutBytes: 7,
		},
	}
	guest := NewGuestControl(result, client)
	record, err := guest.Systemctl(context.Background(), "is-active", "containerd.service")
	if err != nil {
		t.Fatalf("Systemctl() error = %v", err)
	}
	if client.commandReq.GetArgv()[0] != "systemctl" {
		t.Fatalf("argv = %#v", client.commandReq.GetArgv())
	}
	if got := readFile(t, record.Stdout); got != "active\n" {
		t.Fatalf("stdout artifact = %q", got)
	}
	if _, err := os.Stat(filepath.Join(record.Dir, "command.json")); err != nil {
		t.Fatalf("command record missing: %v", err)
	}
}

func TestGuestSensitiveOutputIsRedacted(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		command: &vmtestpb.CommandResult{
			ExitStatus:  0,
			Stdout:      []byte("token-value\n"),
			StdoutBytes: 12,
		},
	}
	guest := NewGuestControl(result, client)
	record, err := guest.RunCommand(context.Background(), GuestCommandRequest{
		Name:            "kubectl-output",
		Argv:            []string{"kubectl", "get", "configmap"},
		SensitiveOutput: true,
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if record.Stdout != "" || record.Redaction != "output" {
		t.Fatalf("record = %#v", record)
	}
	data, err := os.ReadFile(filepath.Join(record.Dir, "command.json"))
	if err != nil {
		t.Fatalf("read command record: %v", err)
	}
	if strings.Contains(string(data), "token-value") {
		t.Fatalf("record leaked sensitive output: %s", data)
	}
}

func TestGuestCommandAllowsFailure(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		command: &vmtestpb.CommandResult{
			ExitStatus:  1,
			Stderr:      []byte("diagnostic unavailable\n"),
			StderrBytes: 23,
		},
	}
	guest := NewGuestControl(result, client)
	record, err := guest.RunCommand(context.Background(), GuestCommandRequest{
		Name:         "networkctl-status",
		Argv:         []string{"networkctl", "status", "--all"},
		AllowFailure: true,
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if record.ExitStatus != 1 || !record.AllowFailure {
		t.Fatalf("record = %#v", record)
	}
	data, err := os.ReadFile(filepath.Join(record.Dir, "command.json"))
	if err != nil {
		t.Fatalf("read command record: %v", err)
	}
	if !strings.Contains(string(data), `"allowFailure": true`) {
		t.Fatalf("record missing allowFailure: %s", data)
	}
}

func TestGuestFileContentExcludedByDefault(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		file: &vmtestpb.FileResult{
			Content:   []byte("machine secret"),
			SizeBytes: 14,
			Redaction: "sensitive",
		},
	}
	guest := NewGuestControl(result, client)
	record, err := guest.ReadFile(context.Background(), GuestFileRequest{
		Name: "machine-id",
		Path: "/etc/machine-id",
	})
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !client.fileReq.GetSensitive() {
		t.Fatalf("file request was not marked sensitive")
	}
	if record.Artifact != "" || record.Redaction != "sensitive" {
		t.Fatalf("record = %#v", record)
	}
	data, err := os.ReadFile(filepath.Join(recordPath(t, result, "files"), "file.json"))
	if err != nil {
		t.Fatalf("read file record: %v", err)
	}
	if strings.Contains(string(data), "machine secret") {
		t.Fatalf("record leaked file content: %s", data)
	}
}

func TestGuestWriteFileRecordsMetadataOnly(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		writeFile: &vmtestpb.WriteFileResult{
			SizeBytes: 17,
			Redaction: "sensitive",
		},
	}
	guest := NewGuestControl(result, client)
	record, err := guest.WriteFile(context.Background(), GuestFileRequest{
		Name:      "request",
		Path:      "/var/lib/katl/test-artifacts/request.yaml",
		Content:   []byte("secret-token-value"),
		Mode:      0o600,
		Sensitive: true,
	})
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if client.writeFileReq.GetPath() != "/var/lib/katl/test-artifacts/request.yaml" || string(client.writeFileReq.GetContent()) != "secret-token-value" || client.writeFileReq.GetMode() != 0o600 {
		t.Fatalf("write request = %#v", client.writeFileReq)
	}
	if record.SizeBytes != 17 || record.Redaction != "sensitive" || record.Artifact != "" {
		t.Fatalf("record = %#v", record)
	}
	data, err := os.ReadFile(filepath.Join(record.Dir, "file.json"))
	if err != nil {
		t.Fatalf("read file record: %v", err)
	}
	if strings.Contains(string(data), "secret-token-value") {
		t.Fatalf("record leaked written content: %s", data)
	}
}

func TestGuestJournalRecordsArtifact(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		journal: &vmtestpb.JournalResult{
			Text:      "runtime ready\n",
			SizeBytes: 14,
		},
	}
	guest := NewGuestControl(result, client)
	record, err := guest.ExportJournal(context.Background(), GuestJournalRequest{
		Name:  "runtime",
		Units: []string{"katl-runtime-boot-signal.service"},
	})
	if err != nil {
		t.Fatalf("ExportJournal() error = %v", err)
	}
	if got := readFile(t, record.Artifact); got != "runtime ready\n" {
		t.Fatalf("journal artifact = %q", got)
	}
	if client.journalReq.GetUnits()[0] != "katl-runtime-boot-signal.service" {
		t.Fatalf("journal request = %#v", client.journalReq)
	}
}

func TestGuestDiagnosticsAreBestEffort(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		commandErr: errors.New("agent unavailable"),
		file:       &vmtestpb.FileResult{Redaction: "sensitive"},
	}
	guest := NewGuestControl(result, client)
	report := guest.CollectDiagnostics(context.Background(), GuestDiagnostics{
		Commands: []GuestCommandRequest{{
			Name: "systemctl",
			Argv: []string{"systemctl", "status"},
		}},
		Files: []GuestFileRequest{{
			Name: "cmdline",
			Path: "/proc/cmdline",
		}},
	})
	if len(report.Errors) != 1 || len(report.Commands) != 1 || len(report.Files) != 1 {
		t.Fatalf("report = %#v", report)
	}
	data, err := os.ReadFile(filepath.Join(result.Artifacts.GuestDir, "diagnostics.json"))
	if err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	var decoded GuestDiagnosticReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode diagnostics: %v", err)
	}
	if len(decoded.Errors) != 1 {
		t.Fatalf("decoded = %#v", decoded)
	}
}

type fakeGuestClient struct {
	command      *vmtestpb.CommandResult
	commandErr   error
	commandReq   *vmtestpb.RunCommandRequest
	file         *vmtestpb.FileResult
	fileErr      error
	fileReq      *vmtestpb.ReadFileRequest
	writeFile    *vmtestpb.WriteFileResult
	writeFileErr error
	writeFileReq *vmtestpb.WriteFileRequest
	journal      *vmtestpb.JournalResult
	journalErr   error
	journalReq   *vmtestpb.ExportJournalRequest
}

func (c *fakeGuestClient) RunCommand(_ context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	c.commandReq = req
	if c.commandErr != nil {
		return nil, c.commandErr
	}
	return c.command, nil
}

func (c *fakeGuestClient) ReadFile(_ context.Context, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	c.fileReq = req
	if c.fileErr != nil {
		return nil, c.fileErr
	}
	return c.file, nil
}

func (c *fakeGuestClient) WriteFile(_ context.Context, req *vmtestpb.WriteFileRequest) (*vmtestpb.WriteFileResult, error) {
	c.writeFileReq = req
	if c.writeFileErr != nil {
		return nil, c.writeFileErr
	}
	return c.writeFile, nil
}

func (c *fakeGuestClient) ExportJournal(_ context.Context, req *vmtestpb.ExportJournalRequest) (*vmtestpb.JournalResult, error) {
	c.journalReq = req
	if c.journalErr != nil {
		return nil, c.journalErr
	}
	return c.journal, nil
}

func guestResult(t *testing.T) Result {
	t.Helper()
	root := t.TempDir()
	return Result{
		RunDir: root,
		Artifacts: ArtifactPaths{
			GuestDir: filepath.Join(root, "guest"),
		},
	}
}

func recordPath(t *testing.T, result Result, kind string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(result.Artifacts.GuestDir, kind))
	if err != nil {
		t.Fatalf("read artifact dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	return filepath.Join(result.Artifacts.GuestDir, kind, entries[0].Name())
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestSysupdateFailureEvidenceKeepsGuestJournal(t *testing.T) {
	result := guestResult(t)
	client := &fakeGuestClient{
		command: &vmtestpb.CommandResult{},
		journal: &vmtestpb.JournalResult{Text: "kernel: vdb3\n", SizeBytes: 13},
	}

	collectSysupdateFailureEvidence(context.Background(), NewGuestControl(result, client))

	data, err := os.ReadFile(filepath.Join(result.Artifacts.GuestDir, "diagnostics.json"))
	if err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	var report GuestDiagnosticReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode diagnostics: %v", err)
	}
	// The whole journal is kept, not only the agent's units, so kernel and
	// udev messages about the partitions are available.
	var whole bool
	for _, journal := range report.Journals {
		if len(journal.Units) == 0 && journal.Artifact != "" && readFile(t, journal.Artifact) == "kernel: vdb3\n" {
			whole = true
		}
	}
	if !whole {
		t.Fatalf("diagnostics = %#v, want a journal artifact that is not limited to units", report)
	}
}
