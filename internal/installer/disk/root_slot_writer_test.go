package disk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWriteRootSlotOK(t *testing.T) {
	artifact := []byte("runtime-root")
	target := newMemSlot(len(artifact) + 4096)
	copy(target.data[len(artifact):], []byte("trailing bytes stay outside digest"))

	result, err := WriteRootSlot(RootSlotInstallRequest{
		Plan:     writePlan(artifact),
		Artifact: bytes.NewReader(artifact),
		Target:   target,
	})
	if err != nil {
		t.Fatalf("WriteRootSlot() error = %v", err)
	}

	if result.BytesWritten != int64(len(artifact)) || result.SHA256 != digest(artifact) {
		t.Fatalf("result = %#v", result)
	}
	if got := string(target.data[:len(artifact)]); got != string(artifact) {
		t.Fatalf("written bytes = %q, want artifact", got)
	}
	if !target.synced {
		t.Fatal("target was not synced")
	}
}

func TestWriteRootSlotRejectsCorruptArtifact(t *testing.T) {
	target := newMemSlot(4096)

	_, err := WriteRootSlot(RootSlotInstallRequest{
		Plan:     writePlan([]byte("expected")),
		Artifact: bytes.NewReader([]byte("corrupt!")),
		Target:   target,
	})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("WriteRootSlot() error = %v, want digest mismatch", err)
	}
	if target.writes != 0 {
		t.Fatalf("writes = %d, want 0 before digest passes", target.writes)
	}
}

func TestWriteRootSlotRejectsShortWrite(t *testing.T) {
	artifact := []byte("runtime-root")
	target := newMemSlot(len(artifact))
	target.short = true

	_, err := WriteRootSlot(RootSlotInstallRequest{
		Plan:     writePlan(artifact),
		Artifact: bytes.NewReader(artifact),
		Target:   target,
	})
	if err == nil || !strings.Contains(err.Error(), "short root slot write") {
		t.Fatalf("WriteRootSlot() error = %v, want short write", err)
	}
}

func TestWriteRootSlotRejectsVerifyMismatch(t *testing.T) {
	artifact := []byte("runtime-root")
	target := newMemSlot(len(artifact))
	target.corrupt = true

	_, err := WriteRootSlot(RootSlotInstallRequest{
		Plan:     writePlan(artifact),
		Artifact: bytes.NewReader(artifact),
		Target:   target,
	})
	if err == nil || !strings.Contains(err.Error(), "root slot digest mismatch") {
		t.Fatalf("WriteRootSlot() error = %v, want verify mismatch", err)
	}
}

func writePlan(data []byte) RootSlotWritePlan {
	return RootSlotWritePlan{
		Slot: RootSlotA,
		TargetPartition: RootSlotTarget{
			Name:     "root-a",
			GPTLabel: GPTLabelRootA,
		},
		ArtifactDigest:    digest(data),
		ExpectedSizeBytes: int64(len(data)),
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type memSlot struct {
	data    []byte
	writes  int
	synced  bool
	short   bool
	corrupt bool
}

func newMemSlot(size int) *memSlot {
	return &memSlot{data: make([]byte, size)}
}

func (m *memSlot) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memSlot) WriteAt(p []byte, off int64) (int, error) {
	m.writes++
	if m.short {
		return len(p) - 1, nil
	}
	if off < 0 || off+int64(len(p)) > int64(len(m.data)) {
		return 0, errors.New("out of range")
	}
	n := copy(m.data[off:], p)
	if m.corrupt && n > 0 {
		m.data[0] ^= 0xff
	}
	return n, nil
}

func (m *memSlot) Sync() error {
	m.synced = true
	return nil
}
