package generation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInitialHandoff(t *testing.T) {
	for _, boundary := range []string{"fresh", "specification", "generation"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
			record := abRecord(t, "0", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", now)
			record.Boot.LoaderEntryPath = "loader/entries/katl-0.conf"
			spec := SpecFromRecord(record)
			if boundary != "fresh" {
				status, err := NewGenerationStatus(spec, CommitStateCommitted, BootStatePending, HealthStateUnknown, now)
				if err != nil {
					t.Fatal(err)
				}
				if err := WriteGeneration(root, spec, status); err != nil {
					t.Fatal(err)
				}
				if boundary == "specification" {
					if err := os.Remove(filepath.Join(root, "var/lib/katl/generations/0/status.json")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := Initialize(root, spec); err != nil {
				t.Fatal(err)
			}
			if err := Initialize(root, spec); err != nil {
				t.Fatalf("repeat: %v", err)
			}
			selection, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if selection.BootedGenerationID != "" || selection.ActiveGenerationID != "" || selection.BootedBootEntry != "" || !selection.PendingHealthValidation || selection.TargetBootGenerationID != "0" {
				t.Fatalf("offline installation claimed observed boot: %+v", selection)
			}
			_, status, err := ReadGeneration(root, "0")
			if err != nil {
				t.Fatal(err)
			}
			if IsKnownGood(status) {
				t.Fatal("unbooted generation is healthy")
			}

			request := BootHealthRequest{Root: root, GenerationID: "0", CommandLine: "katl.generation=0 root=PARTUUID=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Result: BootHealthSuccess, Now: now.Add(time.Minute)}
			if _, err := RecordBootHealth(request); err == nil {
				t.Fatal("health for another root was accepted")
			}
			request.CommandLine = "katl.generation=0 root=PARTUUID=11111111-2222-3333-4444-555555555555"
			if _, err := RecordBootHealth(request); err != nil {
				t.Fatalf("first boot: %v", err)
			}
			if err := Initialize(root, spec); err != nil {
				t.Fatalf("retry after first boot: %v", err)
			}
			selection, err = ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			if selection.BootedGenerationID != "0" || selection.PendingHealthValidation || selection.ActiveGenerationID != "0" {
				t.Fatalf("first boot evidence lost: %+v", selection)
			}
			_, status, err = ReadGeneration(root, "0")
			if err != nil || !IsKnownGood(status) {
				t.Fatalf("first boot health = %+v, error = %v", status, err)
			}

			conflicting := spec
			conflicting.RuntimeVersion = "different"
			if err := Initialize(root, conflicting); err == nil {
				t.Fatal("reinitialization replaced original generation")
			}
		})
	}
}
