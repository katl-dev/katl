package generation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMachineID = "0123456789abcdef0123456789abcdef"

func TestRenderEntryA(t *testing.T) {
	record := abRecord(t, "2026.06.01-001", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})
	record.KernelCommandLine = []string{"console=ttyS0,115200n8", "root=PARTUUID=" + record.Root.PartitionUUID, "ro"}

	entry, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID})
	if err != nil {
		t.Fatalf("RenderEntry() error = %v", err)
	}
	want := `title Katl 2026.06.01-001
version 0.1.0
sort-key katl
machine-id 0123456789abcdef0123456789abcdef
efi /EFI/Linux/katl-2026.06.01-001.efi
options root=PARTUUID=11111111-2222-3333-4444-555555555555 rootfstype=squashfs ro systemd.gpt_auto=no systemd.machine_id=0123456789abcdef0123456789abcdef katl.generation=2026.06.01-001 katl.root-slot=root-a console=ttyS0,115200n8
`
	if entry.Name != "katl-2026.06.01-001.conf" || entry.Content != want {
		t.Fatalf("entry = %#v\nwant content:\n%s", entry, want)
	}
}

func TestRenderEntryB(t *testing.T) {
	record := abRecord(t, "2026.06.01-002", "root-b", "66666666-7777-8888-9999-000000000000", "0.2.0", "v1.35.1", time.Time{})

	entry, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID, Title: "Katl candidate"})
	if err != nil {
		t.Fatalf("RenderEntry() error = %v", err)
	}
	want := `title Katl candidate
version 0.2.0
sort-key katl
machine-id 0123456789abcdef0123456789abcdef
efi /EFI/Linux/katl-2026.06.01-002.efi
options root=PARTUUID=66666666-7777-8888-9999-000000000000 rootfstype=squashfs ro systemd.gpt_auto=no systemd.machine_id=0123456789abcdef0123456789abcdef katl.generation=2026.06.01-002 katl.root-slot=root-b
`
	if entry.Content != want {
		t.Fatalf("entry content:\n%s\nwant:\n%s", entry.Content, want)
	}
}

func TestRenderEntryRejectsRoot(t *testing.T) {
	record := abRecord(t, "2026.06.01-003", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})
	record.KernelCommandLine = []string{"root=gpt-auto"}

	_, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID})
	if err == nil || !strings.Contains(err.Error(), "root option") {
		t.Fatalf("RenderEntry() error = %v, want root option failure", err)
	}
}

func TestRenderEntryRejectsGPTAutoDiscovery(t *testing.T) {
	record := abRecord(t, "2026.06.01-003", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})
	record.KernelCommandLine = []string{"systemd.gpt_auto=yes"}

	_, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID})
	if err == nil || !strings.Contains(err.Error(), "systemd.gpt_auto") {
		t.Fatalf("RenderEntry() error = %v, want gpt-auto failure", err)
	}
}

func TestRenderEntryRejectsInjectedUUID(t *testing.T) {
	record := abRecord(t, "2026.06.01-004", "root-a", "11111111-2222-3333-4444-555555555555 rw", "0.1.0", "v1.34.8", time.Time{})

	_, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID})
	if err == nil || !strings.Contains(err.Error(), "root partition UUID") {
		t.Fatalf("RenderEntry() error = %v, want root UUID validation failure", err)
	}
}

func TestRenderEntryRejectsTitleNewline(t *testing.T) {
	record := abRecord(t, "2026.06.01-004", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})

	_, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID, Title: "Katl\noptions rw"})
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("RenderEntry() error = %v, want title validation failure", err)
	}
}

func TestRenderEntryRejectsGenerationPath(t *testing.T) {
	record := abRecord(t, "2026.06.01-004", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})
	record.GenerationID = "../escape"

	_, err := RenderEntry(LoaderRequest{Record: record, MachineID: testMachineID})
	if err == nil || !strings.Contains(err.Error(), "single path segment") {
		t.Fatalf("RenderEntry() error = %v, want generation path validation failure", err)
	}
}

func TestWriteEntry(t *testing.T) {
	record := abRecord(t, "2026.06.01-004", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})
	root := t.TempDir()

	path, err := WriteEntry(root, LoaderRequest{Record: record, MachineID: testMachineID})
	if err != nil {
		t.Fatalf("WriteEntry() error = %v", err)
	}
	if path != filepath.Join(root, "loader", "entries", "katl-2026.06.01-004.conf") {
		t.Fatalf("path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if !strings.Contains(string(data), "systemd.machine_id="+testMachineID) {
		t.Fatalf("entry content = %q", data)
	}
}
