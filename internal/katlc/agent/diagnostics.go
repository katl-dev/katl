package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) GetKubeconfig(ctx context.Context, _ *agentapi.GetKubeconfigRequest) (*agentapi.KubeconfigResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	data, err := s.readAdminKubeconfig()
	if err != nil {
		return nil, err
	}
	return &agentapi.KubeconfigResponse{Kubeconfig: data}, nil
}

func (s *Server) readAdminKubeconfig() ([]byte, error) {
	file, err := os.Open(filepath.Join(runtimeRoot(s.Root), "etc/kubernetes/admin.conf"))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "Kubernetes admin kubeconfig is unavailable; select a bootstrapped control-plane node")
	}
	defer file.Close()
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, status.Error(codes.Internal, "read Kubernetes admin kubeconfig")
	}
	if len(bytes.TrimSpace(data)) == 0 || len(data) > limit {
		return nil, status.Error(codes.FailedPrecondition, "Kubernetes admin kubeconfig is empty or exceeds 1 MiB")
	}
	return data, nil
}

func (s *Server) ReadJournal(request *agentapi.JournalRequest, stream grpc.ServerStreamingServer[agentapi.JournalEntry]) error {
	args, err := journalArguments(request)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	command := exec.CommandContext(ctx, "journalctl", args...)
	var diagnostic journalDiagnostic
	command.Stderr = &diagnostic
	output, err := command.StdoutPipe()
	if err != nil {
		return status.Error(codes.Internal, "open journal output")
	}
	if err := command.Start(); err != nil {
		return status.Errorf(codes.Unavailable, "start journalctl: %v", err)
	}
	// Cancellation closes the child even if the reader disconnects during follow.
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if err := stream.Send(&agentapi.JournalEntry{Line: scanner.Text()}); err != nil {
			cancel()
			_ = command.Wait()
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		cancel()
		_ = command.Wait()
		return status.Errorf(codes.Internal, "read journal: %v", err)
	}
	if err := command.Wait(); err != nil {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		message := strings.TrimSpace(diagnostic.String())
		return status.Errorf(codes.FailedPrecondition, "journalctl: %s", message)
	}
	return nil
}

type journalDiagnostic struct{ bytes.Buffer }

func (w *journalDiagnostic) Write(data []byte) (int, error) {
	n := len(data)
	remaining := 4096 - w.Len()
	if len(data) > remaining {
		data = data[:remaining]
	}
	_, _ = w.Buffer.Write(data)
	return n, nil
}

func journalArguments(request *agentapi.JournalRequest) ([]string, error) {
	if request == nil || request.Lines < 0 || request.Lines > 10000 {
		return nil, status.Error(codes.InvalidArgument, "journal lines must be between 0 and 10000")
	}
	format := "short-iso"
	if request.Format == "json" {
		format = "json"
	} else if request.Format != "" && request.Format != "text" {
		return nil, status.Error(codes.InvalidArgument, "journal format must be text or json")
	}
	boot := request.Boot
	if boot == "" {
		boot = "0"
	}
	if _, err := strconv.ParseInt(boot, 10, 32); err != nil {
		decoded, err := hex.DecodeString(boot)
		if err != nil || len(decoded) != 16 {
			return nil, status.Error(codes.InvalidArgument, "journal boot must be an offset (0 or -1) or a boot ID")
		}
	}
	if len(request.Units) > 32 || len(request.Since) > 256 || strings.ContainsAny(request.Since, "\x00\r\n") {
		return nil, status.Error(codes.InvalidArgument, "invalid journal filter")
	}
	args := []string{"--no-pager", "--quiet", "--output=" + format, "--lines=" + strconv.Itoa(int(request.Lines)), "--boot=" + boot}
	for _, unit := range request.Units {
		if unit == "" || len(unit) > 256 || strings.ContainsAny(unit, "\x00\r\n") {
			return nil, status.Error(codes.InvalidArgument, "journal unit must be a non-empty unit name or pattern")
		}
		args = append(args, "--unit="+unit)
	}
	if request.Since != "" {
		args = append(args, "--since="+request.Since)
	}
	if request.Follow {
		args = append(args, "--follow")
	}
	return args, nil
}
