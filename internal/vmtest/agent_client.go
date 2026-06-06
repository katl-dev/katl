package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
)

type AgentClient struct {
	Conn           io.ReadWriteCloser
	Transcript     string
	DefaultTimeout time.Duration
	nextID         atomic.Uint64
	now            func() time.Time
}

type transcriptEntry struct {
	RequestID       string   `json:"requestId"`
	Method          string   `json:"method"`
	Started         string   `json:"started"`
	DurationMS      int64    `json:"durationMs"`
	Status          string   `json:"status"`
	Argv            []string `json:"argv,omitempty"`
	ErrorCode       string   `json:"errorCode,omitempty"`
	Error           string   `json:"error,omitempty"`
	ExitStatus      int32    `json:"exitStatus,omitempty"`
	StdoutBytes     uint32   `json:"stdoutBytes,omitempty"`
	StderrBytes     uint32   `json:"stderrBytes,omitempty"`
	FileBytes       uint32   `json:"fileBytes,omitempty"`
	WriteBytes      uint32   `json:"writeBytes,omitempty"`
	JournalBytes    uint32   `json:"journalBytes,omitempty"`
	SensitiveOutput bool     `json:"sensitiveOutput,omitempty"`
	Redaction       string   `json:"redaction,omitempty"`
}

var (
	transcriptBootstrapTokenPattern      = regexp.MustCompile(`(?i)\b[a-z0-9]{6}\.[a-z0-9]{16}\b`)
	transcriptDiscoveryTokenHashPattern  = regexp.MustCompile(`(?i)\bsha256:[a-f0-9]{64}\b`)
	transcriptCertificateKeyValuePattern = regexp.MustCompile(`(?i)^[a-f0-9]{64}$`)
)

func NewAgentClient(conn io.ReadWriteCloser, transcript string) *AgentClient {
	return &AgentClient{
		Conn:           conn,
		Transcript:     transcript,
		DefaultTimeout: 10 * time.Second,
		now:            time.Now,
	}
}

func DialAgent(ctx context.Context, cid, port uint32, transcript string) (*AgentClient, error) {
	conn, err := DialVSock(ctx, cid, port)
	if err != nil {
		return nil, err
	}
	return NewAgentClient(conn, transcript), nil
}

func (c *AgentClient) Health(ctx context.Context) (*vmtestpb.HealthResponse, error) {
	resp, err := c.Do(ctx, &vmtestpb.VmtestRequest{
		TimeoutMs: requestTimeoutMS(ctx),
		Operation: &vmtestpb.VmtestRequest_Health{Health: &vmtestpb.HealthRequest{}},
	})
	if err != nil {
		return nil, err
	}
	result, ok := resp.Result.(*vmtestpb.VmtestResponse_Health)
	if !ok {
		return nil, responseError(resp)
	}
	return result.Health, nil
}

func (c *AgentClient) RunCommand(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	resp, err := c.Do(ctx, &vmtestpb.VmtestRequest{
		TimeoutMs: requestTimeoutMS(ctx),
		Operation: &vmtestpb.VmtestRequest_RunCommand{RunCommand: req},
	})
	if err != nil {
		return nil, err
	}
	result, ok := resp.Result.(*vmtestpb.VmtestResponse_Command)
	if !ok {
		return nil, responseError(resp)
	}
	return result.Command, nil
}

func (c *AgentClient) ReadFile(ctx context.Context, req *vmtestpb.ReadFileRequest) (*vmtestpb.FileResult, error) {
	resp, err := c.Do(ctx, &vmtestpb.VmtestRequest{
		TimeoutMs: requestTimeoutMS(ctx),
		Operation: &vmtestpb.VmtestRequest_ReadFile{ReadFile: req},
	})
	if err != nil {
		return nil, err
	}
	result, ok := resp.Result.(*vmtestpb.VmtestResponse_File)
	if !ok {
		return nil, responseError(resp)
	}
	return result.File, nil
}

func (c *AgentClient) WriteFile(ctx context.Context, req *vmtestpb.WriteFileRequest) (*vmtestpb.WriteFileResult, error) {
	resp, err := c.Do(ctx, &vmtestpb.VmtestRequest{
		TimeoutMs: requestTimeoutMS(ctx),
		Operation: &vmtestpb.VmtestRequest_WriteFile{WriteFile: req},
	})
	if err != nil {
		return nil, err
	}
	result, ok := resp.Result.(*vmtestpb.VmtestResponse_WriteFile)
	if !ok {
		return nil, responseError(resp)
	}
	return result.WriteFile, nil
}

func (c *AgentClient) ExportJournal(ctx context.Context, req *vmtestpb.ExportJournalRequest) (*vmtestpb.JournalResult, error) {
	resp, err := c.Do(ctx, &vmtestpb.VmtestRequest{
		TimeoutMs: requestTimeoutMS(ctx),
		Operation: &vmtestpb.VmtestRequest_ExportJournal{ExportJournal: req},
	})
	if err != nil {
		return nil, err
	}
	result, ok := resp.Result.(*vmtestpb.VmtestResponse_Journal)
	if !ok {
		return nil, responseError(resp)
	}
	return result.Journal, nil
}

func (c *AgentClient) Do(ctx context.Context, req *vmtestpb.VmtestRequest) (*vmtestpb.VmtestResponse, error) {
	if c.Conn == nil {
		return nil, errors.New("vmtest agent connection is nil")
	}
	if req.RequestId == "" {
		req.RequestId = fmt.Sprintf("req-%d", c.nextID.Add(1))
	}
	if req.TimeoutMs == 0 && c.DefaultTimeout > 0 {
		req.TimeoutMs = uint32(c.DefaultTimeout.Milliseconds())
	}
	timeout := c.DefaultTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	started := c.clock()
	method := requestMethod(req)
	err := writeProtoFrame(c.Conn, req)
	if err != nil {
		c.writeTranscript(summaryForError(req.RequestId, method, started, c.clock(), err))
		return nil, err
	}
	type readResult struct {
		resp *vmtestpb.VmtestResponse
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		var resp vmtestpb.VmtestResponse
		err := readProtoFrame(c.Conn, &resp)
		done <- readResult{resp: &resp, err: err}
	}()
	select {
	case result := <-done:
		finished := c.clock()
		if result.err != nil {
			c.writeTranscript(summaryForError(req.RequestId, method, started, finished, result.err))
			return nil, result.err
		}
		c.writeTranscript(summaryForResponse(req, result.resp, method, started, finished))
		return result.resp, nil
	case <-ctx.Done():
		_ = c.Conn.Close()
		err := ctx.Err()
		c.writeTranscript(summaryForError(req.RequestId, method, started, c.clock(), err))
		return nil, err
	}
}

func requestTimeoutMS(ctx context.Context) uint32 {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return 1
	}
	ms := timeout.Milliseconds()
	if ms <= 0 {
		return 1
	}
	if ms > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(ms)
}

func (c *AgentClient) Close() error {
	if c.Conn == nil {
		return nil
	}
	return c.Conn.Close()
}

func (c *AgentClient) clock() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *AgentClient) writeTranscript(entry transcriptEntry) {
	if c.Transcript == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.Transcript), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(c.Transcript, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
}

func requestMethod(req *vmtestpb.VmtestRequest) string {
	switch req.Operation.(type) {
	case *vmtestpb.VmtestRequest_Health:
		return "Health"
	case *vmtestpb.VmtestRequest_RunCommand:
		return "RunCommand"
	case *vmtestpb.VmtestRequest_ReadFile:
		return "ReadFile"
	case *vmtestpb.VmtestRequest_ExportJournal:
		return "ExportJournal"
	case *vmtestpb.VmtestRequest_WriteFile:
		return "WriteFile"
	default:
		return "Unknown"
	}
}

func responseError(resp *vmtestpb.VmtestResponse) error {
	if errResult, ok := resp.Result.(*vmtestpb.VmtestResponse_Error); ok {
		return fmt.Errorf("%s: %s", errResult.Error.Code, errResult.Error.Message)
	}
	return fmt.Errorf("unexpected vmtest agent response: %T", resp.Result)
}

func summaryForError(requestID, method string, started, finished time.Time, err error) transcriptEntry {
	return transcriptEntry{
		RequestID:  requestID,
		Method:     method,
		Started:    started.Format(time.RFC3339Nano),
		DurationMS: finished.Sub(started).Milliseconds(),
		Status:     "transport-error",
		Error:      err.Error(),
	}
}

func summaryForResponse(req *vmtestpb.VmtestRequest, resp *vmtestpb.VmtestResponse, method string, started, finished time.Time) transcriptEntry {
	entry := transcriptEntry{
		RequestID:  resp.RequestId,
		Method:     method,
		Started:    started.Format(time.RFC3339Nano),
		DurationMS: finished.Sub(started).Milliseconds(),
		Status:     "ok",
	}
	switch result := resp.Result.(type) {
	case *vmtestpb.VmtestResponse_Error:
		entry.Status = "error"
		entry.ErrorCode = result.Error.Code
		entry.Error = result.Error.Message
	case *vmtestpb.VmtestResponse_Command:
		entry.ExitStatus = result.Command.ExitStatus
		entry.StdoutBytes = result.Command.StdoutBytes
		entry.StderrBytes = result.Command.StderrBytes
		if run, ok := req.Operation.(*vmtestpb.VmtestRequest_RunCommand); ok {
			entry.Argv = redactTranscriptArgv(run.RunCommand.Argv)
			entry.SensitiveOutput = run.RunCommand.SensitiveOutput
			if run.RunCommand.SensitiveOutput {
				entry.Redaction = "output"
			}
		}
	case *vmtestpb.VmtestResponse_File:
		entry.FileBytes = result.File.SizeBytes
		entry.Redaction = result.File.Redaction
		if file, ok := req.Operation.(*vmtestpb.VmtestRequest_ReadFile); ok {
			entry.SensitiveOutput = file.ReadFile.Sensitive
		}
	case *vmtestpb.VmtestResponse_WriteFile:
		entry.WriteBytes = result.WriteFile.SizeBytes
		entry.Redaction = result.WriteFile.Redaction
		if file, ok := req.Operation.(*vmtestpb.VmtestRequest_WriteFile); ok {
			entry.SensitiveOutput = file.WriteFile.Sensitive
		}
	case *vmtestpb.VmtestResponse_Journal:
		entry.JournalBytes = result.Journal.SizeBytes
	}
	return entry
}

func redactTranscriptArgv(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	redacted := append([]string(nil), argv...)
	for i, arg := range redacted {
		if flag, _, ok := strings.Cut(arg, "="); ok && sensitiveArgvFlag(flag) {
			redacted[i] = flag + "=" + replacementForArgvFlag(flag)
			continue
		}
		if i > 0 && sensitiveArgvFlag(redacted[i-1]) {
			redacted[i] = replacementForArgvFlag(redacted[i-1])
			continue
		}
		redacted[i] = redactTranscriptTokenText(arg)
	}
	return redacted
}

func sensitiveArgvFlag(flag string) bool {
	switch flag {
	case "--token", "--discovery-token-ca-cert-hash", "--certificate-key":
		return true
	default:
		return false
	}
}

func replacementForArgvFlag(flag string) string {
	switch flag {
	case "--discovery-token-ca-cert-hash":
		return "[REDACTED DISCOVERY TOKEN HASH]"
	case "--certificate-key":
		return "[REDACTED CERTIFICATE KEY]"
	default:
		return "[REDACTED BOOTSTRAP TOKEN]"
	}
}

func redactTranscriptTokenText(value string) string {
	value = transcriptBootstrapTokenPattern.ReplaceAllString(value, "[REDACTED BOOTSTRAP TOKEN]")
	value = transcriptDiscoveryTokenHashPattern.ReplaceAllString(value, "[REDACTED DISCOVERY TOKEN HASH]")
	if transcriptCertificateKeyValuePattern.MatchString(value) {
		return "[REDACTED CERTIFICATE KEY]"
	}
	return value
}
