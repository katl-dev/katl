package vmtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
)

const DefaultAgentPort uint32 = 10240

type AgentServer struct {
	Version            string
	AllowedCommands    map[string]bool
	AllowedFilePaths   []string
	AllowedWritePaths  []string
	CommandRunner      AgentCommandRunner
	Hostname           func() (string, error)
	BootID             func() string
	DefaultOutputLimit uint32
}

type AgentCommandRunner interface {
	Run(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error)
}

type execAgentCommandRunner struct {
	allowed map[string]bool
	limit   uint32
}

func NewAgentServer(version string) AgentServer {
	return AgentServer{
		Version:            version,
		AllowedCommands:    defaultAgentCommands(),
		AllowedFilePaths:   defaultAgentFilePaths(),
		DefaultOutputLimit: 256 << 10,
	}
}

func (s AgentServer) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	defer conn.Close()
	for {
		var req vmtestpb.VmtestRequest
		if err := readProtoFrame(conn, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		resp := s.Handle(ctx, &req)
		if err := writeProtoFrame(conn, resp); err != nil {
			return err
		}
	}
}

func (s AgentServer) Handle(ctx context.Context, req *vmtestpb.VmtestRequest) *vmtestpb.VmtestResponse {
	started := time.Now()
	opCtx := ctx
	cancel := func() {}
	if req.TimeoutMs > 0 {
		opCtx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	}
	defer cancel()

	resp := &vmtestpb.VmtestResponse{RequestId: req.RequestId}
	defer func() {
		resp.DurationMs = uint64(time.Since(started).Milliseconds())
	}()

	switch op := req.Operation.(type) {
	case *vmtestpb.VmtestRequest_Health:
		resp.Result = &vmtestpb.VmtestResponse_Health{Health: s.health(opCtx, op.Health)}
	case *vmtestpb.VmtestRequest_RunCommand:
		result, err := s.runCommand(opCtx, op.RunCommand)
		if err != nil {
			resp.Result = errorResult("command_failed", err, false)
			return resp
		}
		resp.Result = &vmtestpb.VmtestResponse_Command{Command: result}
	case *vmtestpb.VmtestRequest_ReadFile:
		result, err := s.readFile(op.ReadFile)
		if err != nil {
			resp.Result = errorResult("read_file_failed", err, false)
			return resp
		}
		resp.Result = &vmtestpb.VmtestResponse_File{File: result}
	case *vmtestpb.VmtestRequest_WriteFile:
		result, err := s.writeFile(op.WriteFile)
		if err != nil {
			resp.Result = errorResult("write_file_failed", err, false)
			return resp
		}
		resp.Result = &vmtestpb.VmtestResponse_WriteFile{WriteFile: result}
	case *vmtestpb.VmtestRequest_ExportJournal:
		result, err := s.exportJournal(opCtx, op.ExportJournal)
		if err != nil {
			resp.Result = errorResult("journal_failed", err, false)
			return resp
		}
		resp.Result = &vmtestpb.VmtestResponse_Journal{Journal: result}
	default:
		resp.Result = errorResult("unknown_method", errors.New("request has no supported operation"), false)
	}
	return resp
}

func (s AgentServer) health(_ context.Context, _ *vmtestpb.HealthRequest) *vmtestpb.HealthResponse {
	hostname := ""
	if s.Hostname != nil {
		name, err := s.Hostname()
		if err == nil {
			hostname = name
		}
	} else if name, err := os.Hostname(); err == nil {
		hostname = name
	}
	bootID := ""
	if s.BootID != nil {
		bootID = s.BootID()
	} else {
		bootID = readTrimmed("/proc/sys/kernel/random/boot_id")
	}
	return &vmtestpb.HealthResponse{
		AgentVersion: first(s.Version, "0.0.0-dev"),
		Hostname:     hostname,
		BootId:       bootID,
	}
}

func (s AgentServer) runCommand(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("argv is required")
	}
	runner := s.CommandRunner
	if runner == nil {
		runner = execAgentCommandRunner{allowed: s.commands(), limit: s.outputLimit()}
	}
	return runner.Run(ctx, req)
}

func (s AgentServer) readFile(req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	if req.Path == "" || !filepath.IsAbs(req.Path) {
		return nil, errors.New("absolute file path is required")
	}
	path := filepath.Clean(req.Path)
	if !pathAllowed(path, s.files()) {
		return nil, fmt.Errorf("file path is not allowlisted: %s", path)
	}
	limit := int64(req.MaxBytes)
	if limit <= 0 {
		limit = int64(s.outputLimit())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	redaction := "none"
	if req.Sensitive {
		redaction = "sensitive"
	}
	return &vmtestpb.FileResult{
		Content:   data,
		Truncated: truncated,
		SizeBytes: uint32(len(data)),
		Redaction: redaction,
	}, nil
}

func (s AgentServer) writeFile(req *vmtestpb.WriteFileRequest) (*vmtestpb.WriteFileResult, error) {
	if req.Path == "" || !filepath.IsAbs(req.Path) {
		return nil, errors.New("absolute file path is required")
	}
	path := filepath.Clean(req.Path)
	if !pathAllowed(path, s.writeFiles()) {
		return nil, fmt.Errorf("file path is not allowlisted: %s", path)
	}
	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o600
	}
	if mode&^os.FileMode(0o777) != 0 {
		return nil, fmt.Errorf("file mode %04o contains unsupported bits", mode)
	}
	parent := filepath.Dir(path)
	if err := prepareWriteParent(parent); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to write through symlink: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := writeNoFollow(path, req.Content, mode.Perm()); err != nil {
		return nil, err
	}
	redaction := "none"
	if req.Sensitive {
		redaction = "sensitive"
	}
	return &vmtestpb.WriteFileResult{
		SizeBytes: uint32(len(req.Content)),
		Redaction: redaction,
	}, nil
}

func prepareWriteParent(parent string) error {
	parent = filepath.Clean(parent)
	existing := parent
	var missing []string
	for {
		info, err := os.Lstat(existing)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to write through symlink parent: %s", existing)
			}
			if !info.IsDir() {
				return fmt.Errorf("write parent is not a directory: %s", existing)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, existing)
		next := filepath.Dir(existing)
		if next == existing {
			return err
		}
		existing = next
	}
	if err := rejectSymlinkPath(existing); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := rejectSymlinkPath(missing[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeNoFollow(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|noFollowFlag(), mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		return err
	}
	return file.Chmod(mode)
}

func (s AgentServer) exportJournal(ctx context.Context, req *vmtestpb.ExportJournalRequest) (*vmtestpb.JournalResult, error) {
	argv := []string{"journalctl", "--no-pager", "--output=short-monotonic"}
	if req.BootId != "" {
		argv = append(argv, "-b", req.BootId)
	} else {
		argv = append(argv, "-b")
	}
	for _, unit := range req.Units {
		if unit == "" || strings.Contains(unit, "/") {
			return nil, fmt.Errorf("invalid journal unit: %q", unit)
		}
		argv = append(argv, "-u", unit)
	}
	limit := req.MaxBytes
	if limit == 0 {
		limit = s.outputLimit()
	}
	result, err := s.runCommand(ctx, &vmtestpb.RunCommandRequest{
		Argv:        argv,
		StdoutLimit: limit,
		StderrLimit: 64 << 10,
	})
	if err != nil {
		return nil, err
	}
	if result.ExitStatus != 0 {
		return nil, fmt.Errorf("journalctl exited %d: %s", result.ExitStatus, strings.TrimSpace(string(result.Stderr)))
	}
	return &vmtestpb.JournalResult{
		Text:      string(result.Stdout),
		Truncated: result.StdoutTruncated,
		SizeBytes: result.StdoutBytes,
	}, nil
}

func rejectSymlinkPath(path string) error {
	path = filepath.Clean(path)
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink parent: %s", path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func (s AgentServer) commands() map[string]bool {
	if len(s.AllowedCommands) == 0 {
		return defaultAgentCommands()
	}
	return s.AllowedCommands
}

func (s AgentServer) files() []string {
	if len(s.AllowedFilePaths) == 0 {
		return defaultAgentFilePaths()
	}
	return s.AllowedFilePaths
}

func (s AgentServer) writeFiles() []string {
	if len(s.AllowedWritePaths) == 0 {
		return defaultAgentWritePaths()
	}
	return s.AllowedWritePaths
}

func (s AgentServer) outputLimit() uint32 {
	if s.DefaultOutputLimit == 0 {
		return 256 << 10
	}
	return s.DefaultOutputLimit
}

func (r execAgentCommandRunner) Run(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("argv is required")
	}
	if !commandAllowed(req.Argv[0], r.allowed) {
		return nil, fmt.Errorf("command is not allowlisted: %s", req.Argv[0])
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	if req.WorkingDirectory != "" {
		cmd.Dir = req.WorkingDirectory
	}
	if len(req.Environment) > 0 {
		env := os.Environ()
		for _, entry := range req.Environment {
			if !safeEnvName(entry.Name) {
				return nil, fmt.Errorf("environment variable is not allowlisted: %s", entry.Name)
			}
			env = append(env, entry.Name+"="+entry.Value)
		}
		cmd.Env = env
	}
	stdout := limitedBuffer{limit: limitOrDefault(req.StdoutLimit, r.limit)}
	stderr := limitedBuffer{limit: limitOrDefault(req.StderrLimit, r.limit)}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitStatus := int32(0)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exitStatus = int32(exit.ExitCode())
		} else {
			return nil, err
		}
	}
	return &vmtestpb.CommandResult{
		ExitStatus:      exitStatus,
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
		StdoutBytes:     uint32(stdout.Written()),
		StderrBytes:     uint32(stderr.Written()),
	}, nil
}

type limitedBuffer struct {
	buf     bytes.Buffer
	limit   uint32
	written uint32
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	b.written += uint32(n)
	remaining := int(b.limit) - b.buf.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.buf.Write(data)
	}
	return n, nil
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *limitedBuffer) Written() uint32 {
	return b.written
}

func (b *limitedBuffer) Truncated() bool {
	return b.written > b.limit
}

func limitOrDefault(value, fallback uint32) uint32 {
	if value == 0 {
		return fallback
	}
	return value
}

func defaultAgentCommands() map[string]bool {
	return map[string]bool{
		"crictl":            true,
		"chmod":             true,
		"configapply-smoke": true,
		"find":              true,
		"findmnt":           true,
		"getent":            true,
		"install":           true,
		"ip":                true,
		"journalctl":        true,
		"kubeadm":           true,
		"kubectl":           true,
		"networkctl":        true,
		"readlink":          true,
		"resolvectl":        true,
		"systemctl":         true,
		"test":              true,
		"true":              true,
		"uname":             true,
	}
}

func defaultAgentFilePaths() []string {
	return []string{
		"/etc/katl/",
		"/etc/kubernetes/admin.conf",
		"/etc/kubernetes/kubelet.conf",
		"/etc/os-release",
		"/proc/cmdline",
		"/run/katl/",
		"/var/lib/katl/config-requests/",
		"/usr/lib/os-release",
		"/var/lib/katl/generations/",
		"/var/lib/katl/test-artifacts/",
	}
}

func defaultAgentWritePaths() []string {
	return []string{
		"/var/lib/katl/test-artifacts/",
	}
}

func commandAllowed(command string, allowed map[string]bool) bool {
	base := filepath.Base(command)
	return allowed[command] || allowed[base]
}

func pathAllowed(path string, allowed []string) bool {
	for _, entry := range allowed {
		prefix := strings.HasSuffix(entry, string(os.PathSeparator))
		entry = filepath.Clean(entry)
		if prefix {
			if path == entry || strings.HasPrefix(path, entry+string(os.PathSeparator)) {
				return true
			}
			continue
		}
		if path == entry {
			return true
		}
	}
	return false
}

func safeEnvName(name string) bool {
	switch name {
	case "KUBECONFIG", "PATH", "SYSTEMD_LOG_LEVEL":
		return true
	default:
		return strings.HasPrefix(name, "KATL_")
	}
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func errorResult(code string, err error, retryable bool) *vmtestpb.VmtestResponse_Error {
	return &vmtestpb.VmtestResponse_Error{Error: &vmtestpb.VmtestError{
		Code:      code,
		Message:   err.Error(),
		Retryable: retryable,
	}}
}
