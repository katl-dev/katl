package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const rootWriteChunk = 1024 * 1024

type RootSlotDevice interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

type RootSlotInstallRequest struct {
	Plan     RootSlotWritePlan
	Artifact io.ReaderAt
	Target   RootSlotDevice
}

type RootSlotInstallResult struct {
	Slot         RootSlot
	GPTLabel     string
	BytesWritten int64
	SHA256       string
}

func WriteRootSlot(request RootSlotInstallRequest) (RootSlotInstallResult, error) {
	if request.Artifact == nil {
		return RootSlotInstallResult{}, fmt.Errorf("runtime artifact reader is required")
	}
	if request.Target == nil {
		return RootSlotInstallResult{}, fmt.Errorf("root slot target is required")
	}
	if request.Plan.ExpectedSizeBytes <= 0 {
		return RootSlotInstallResult{}, fmt.Errorf("expected artifact size must be positive")
	}
	want := strings.ToLower(strings.TrimSpace(request.Plan.ArtifactDigest))
	if want == "" {
		return RootSlotInstallResult{}, fmt.Errorf("artifact digest is required")
	}

	got, err := hashRange(request.Artifact, request.Plan.ExpectedSizeBytes)
	if err != nil {
		return RootSlotInstallResult{}, fmt.Errorf("hash runtime artifact: %w", err)
	}
	if got != want {
		return RootSlotInstallResult{}, fmt.Errorf("runtime artifact digest mismatch: got %s want %s", got, want)
	}

	written, err := writeRange(request.Target, request.Artifact, request.Plan.ExpectedSizeBytes)
	if err != nil {
		return RootSlotInstallResult{}, err
	}
	if err := request.Target.Sync(); err != nil {
		return RootSlotInstallResult{}, fmt.Errorf("flush root slot: %w", err)
	}

	verified, err := hashRange(request.Target, request.Plan.ExpectedSizeBytes)
	if err != nil {
		return RootSlotInstallResult{}, fmt.Errorf("verify root slot: %w", err)
	}
	if verified != want {
		return RootSlotInstallResult{}, fmt.Errorf("root slot digest mismatch: got %s want %s", verified, want)
	}

	return RootSlotInstallResult{
		Slot:         request.Plan.Slot,
		GPTLabel:     request.Plan.TargetPartition.GPTLabel,
		BytesWritten: written,
		SHA256:       verified,
	}, nil
}

func hashRange(reader io.ReaderAt, size int64) (string, error) {
	hash := sha256.New()
	section := io.NewSectionReader(reader, 0, size)
	written, err := io.Copy(hash, section)
	if err != nil {
		return "", err
	}
	if written != size {
		return "", fmt.Errorf("read %d bytes, want %d", written, size)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeRange(target io.WriterAt, artifact io.ReaderAt, size int64) (int64, error) {
	buf := make([]byte, rootWriteChunk)
	var offset int64
	for offset < size {
		want := int64(len(buf))
		remaining := size - offset
		if remaining < want {
			want = remaining
		}
		chunk := buf[:want]
		n, err := artifact.ReadAt(chunk, offset)
		if err != nil && err != io.EOF {
			return offset, fmt.Errorf("read artifact at %d: %w", offset, err)
		}
		if int64(n) != want {
			return offset, fmt.Errorf("read artifact at %d: got %d bytes want %d", offset, n, want)
		}
		n, err = target.WriteAt(chunk, offset)
		if err != nil {
			return offset + int64(n), fmt.Errorf("write root slot at %d: %w", offset, err)
		}
		if int64(n) != want {
			return offset + int64(n), fmt.Errorf("short root slot write at %d: wrote %d bytes want %d", offset, n, want)
		}
		offset += want
	}
	return offset, nil
}
