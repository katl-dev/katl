package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	StageHostUpgradeArtifactRequestKind       = "StageHostUpgradeArtifactRequest"
	StageKubernetesUpgradeArtifactRequestKind = "StageKubernetesUpgradeArtifactRequest"
	maxHostUpgradeArtifactSize                = uint64(8 << 30)
	maxHostUpgradeArtifactChunkSize           = 2 << 20
	hostUpgradeUploadDirectory                = "host-upgrade/uploads"
	kubernetesUpgradeUploadDirectory          = "kubernetes-upgrade/uploads"
)

type stagedArtifactTarget struct {
	label     string
	directory string
	suffix    string
}

func (s *Server) StageHostUpgradeArtifact(stream grpc.ClientStreamingServer[agentapi.StageHostUpgradeArtifactRequest, agentapi.HostUpgradeArtifactStaged]) error {
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "artifact metadata and content are required")
	}
	if err != nil {
		return err
	}
	target, err := s.validateStagedArtifactMetadata(first)
	if err != nil {
		return err
	}

	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	active, err := s.activeOperationIDs()
	if err != nil {
		return status.Errorf(codes.Internal, "read operation locks: %v", err)
	}
	if len(active) > 0 {
		return status.Errorf(codes.FailedPrecondition, "cannot stage a %s upgrade while operation %s is active", target.label, strings.Join(active, ","))
	}

	directory := filepath.Join(runtimeRoot(s.Root), "var/lib/katl/artifacts", filepath.FromSlash(target.directory))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return status.Errorf(codes.Internal, "prepare %s artifact staging directory: %v", target.label, err)
	}
	temporary, err := os.CreateTemp(directory, "."+first.Sha256+"-*.partial")
	if err != nil {
		return status.Errorf(codes.Internal, "create %s artifact staging file: %v", target.label, err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return status.Errorf(codes.Internal, "protect KatlOS artifact staging file: %v", err)
	}

	hash := sha256.New()
	var received uint64
	writeChunk := func(chunk []byte) error {
		if len(chunk) > maxHostUpgradeArtifactChunkSize {
			return status.Errorf(codes.ResourceExhausted, "artifact chunk exceeds %d bytes", maxHostUpgradeArtifactChunkSize)
		}
		if received > first.SizeBytes || uint64(len(chunk)) > first.SizeBytes-received {
			return status.Errorf(codes.InvalidArgument, "artifact content exceeds declared size %d", first.SizeBytes)
		}
		if len(chunk) == 0 {
			return nil
		}
		written, err := temporary.Write(chunk)
		if err != nil {
			return status.Errorf(codes.Internal, "write %s artifact: %v", target.label, err)
		}
		if written != len(chunk) {
			return status.Errorf(codes.Internal, "write %s artifact: short write", target.label)
		}
		_, _ = hash.Write(chunk)
		received += uint64(written)
		return nil
	}
	if err := writeChunk(first.Chunk); err != nil {
		return err
	}
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if request.ApiVersion != "" || request.Kind != "" || request.Actor != "" || request.ExpectedMachineId != "" || request.ExpectedEnrollmentId != "" || request.ExpectedInventoryNodeName != "" || request.ExpectedCurrentGenerationId != "" || request.Sha256 != "" || request.SizeBytes != 0 {
			return status.Error(codes.InvalidArgument, "artifact metadata is only allowed in the first chunk")
		}
		if err := writeChunk(request.Chunk); err != nil {
			return err
		}
	}
	if received != first.SizeBytes {
		return status.Errorf(codes.InvalidArgument, "artifact content size %d does not match declared size %d", received, first.SizeBytes)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != first.Sha256 {
		return status.Errorf(codes.InvalidArgument, "artifact SHA-256 %s does not match declared identity", got)
	}
	if err := temporary.Sync(); err != nil {
		return status.Errorf(codes.Internal, "sync %s artifact: %v", target.label, err)
	}
	if err := temporary.Close(); err != nil {
		return status.Errorf(codes.Internal, "close %s artifact: %v", target.label, err)
	}
	filename := first.Sha256 + target.suffix
	finalPath := filepath.Join(directory, filename)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return status.Errorf(codes.Internal, "commit %s artifact: %v", target.label, err)
	}
	committed = true
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	localRef := filepath.ToSlash(filepath.Join(target.directory, filename))
	return stream.SendAndClose(&agentapi.HostUpgradeArtifactStaged{
		LocalRef:  localRef,
		Sha256:    first.Sha256,
		SizeBytes: first.SizeBytes,
	})
}

func (s *Server) validateStagedArtifactMetadata(request *agentapi.StageHostUpgradeArtifactRequest) (stagedArtifactTarget, error) {
	if request == nil {
		return stagedArtifactTarget{}, status.Error(codes.InvalidArgument, "request is required")
	}
	if request.ApiVersion != APIVersion {
		return stagedArtifactTarget{}, status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	var target stagedArtifactTarget
	switch request.Kind {
	case StageHostUpgradeArtifactRequestKind:
		target = stagedArtifactTarget{label: "KatlOS", directory: hostUpgradeUploadDirectory, suffix: ".squashfs"}
	case StageKubernetesUpgradeArtifactRequestKind:
		target = stagedArtifactTarget{label: "Kubernetes", directory: kubernetesUpgradeUploadDirectory, suffix: ".raw"}
	default:
		return stagedArtifactTarget{}, status.Errorf(codes.InvalidArgument, "kind must be %q or %q", StageHostUpgradeArtifactRequestKind, StageKubernetesUpgradeArtifactRequestKind)
	}
	if strings.TrimSpace(request.Actor) == "" {
		return stagedArtifactTarget{}, status.Error(codes.InvalidArgument, "actor is required")
	}
	if err := s.validateMutationTarget(request.ExpectedEnrollmentId, request.ExpectedInventoryNodeName, request.ExpectedMachineId, request.ExpectedCurrentGenerationId); err != nil {
		return stagedArtifactTarget{}, err
	}
	if err := validateArtifactSHA256(request.Sha256); err != nil {
		return stagedArtifactTarget{}, status.Error(codes.InvalidArgument, err.Error())
	}
	if request.SizeBytes == 0 {
		return stagedArtifactTarget{}, status.Error(codes.InvalidArgument, "artifact sizeBytes must be positive")
	}
	if request.SizeBytes > maxHostUpgradeArtifactSize {
		return stagedArtifactTarget{}, status.Errorf(codes.ResourceExhausted, "artifact sizeBytes exceeds %d", maxHostUpgradeArtifactSize)
	}
	return target, nil
}

func validateArtifactSHA256(value string) error {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return fmt.Errorf("artifact sha256 must be %d lowercase hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("artifact sha256 must be %d lowercase hexadecimal characters", sha256.Size*2)
	}
	return nil
}
