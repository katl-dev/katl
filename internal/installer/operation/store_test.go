package operation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreCreatesAndUpdatesJournalFirstRecord(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-create")

	updated, err := store.Update(created.OperationID, "phase-prepare", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.Phase = "prepare"
		record.PhaseIndex = 1
		record.CompletedPhases = append(record.CompletedPhases, "accepted")
		return record, nil
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.LatestJournalSeq != 2 || updated.RecordRevision != 2 {
		t.Fatalf("updated seq/rev = %d/%d", updated.LatestJournalSeq, updated.RecordRevision)
	}

	dir := filepath.Join(store.Root, created.OperationID)
	assertExists(t, filepath.Join(dir, "journal", "00000000000000000001.accepted.json"))
	assertExists(t, filepath.Join(dir, "journal", "00000000000000000002.phase-prepare.json"))
	assertExists(t, filepath.Join(dir, "record.json"))
	assertDirMode(t, dir, 0o700)
	assertDirMode(t, filepath.Join(dir, "journal"), 0o700)

	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.Phase != "prepare" || read.CompletedPhases[0] != "accepted" {
		t.Fatalf("read record = %#v", read)
	}
}

func TestStoreWritesGoldenAcceptedJournalEvent(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-golden")
	got, err := os.ReadFile(filepath.Join(store.Root, created.OperationID, "journal", "00000000000000000001.accepted.json"))
	if err != nil {
		t.Fatalf("read accepted event: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "golden", "accepted-event.json"))
	if err != nil {
		t.Fatalf("read golden event: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("accepted event mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestStoreRebuildsMissingStaleAndDigestInvalidSnapshots(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(string)
	}{
		{
			name: "missing snapshot",
			mutate: func(dir string) {
				if err := os.Remove(filepath.Join(dir, "record.json")); err != nil {
					t.Fatalf("remove snapshot: %v", err)
				}
			},
		},
		{
			name: "corrupt snapshot",
			mutate: func(dir string) {
				writeFile(t, filepath.Join(dir, "record.json"), "{bad-json")
			},
		},
		{
			name: "stale snapshot",
			mutate: func(dir string) {
				data, err := os.ReadFile(filepath.Join(dir, "record.json"))
				if err != nil {
					t.Fatalf("read snapshot: %v", err)
				}
				var snap Snapshot
				if err := json.Unmarshal(data, &snap); err != nil {
					t.Fatalf("decode snapshot: %v", err)
				}
				snap.LatestSeq = 1
				data, err = marshalJSON(snap)
				if err != nil {
					t.Fatalf("marshal snapshot: %v", err)
				}
				writeFile(t, filepath.Join(dir, "record.json"), string(data))
			},
		},
		{
			name: "digest invalid snapshot",
			mutate: func(dir string) {
				data, err := os.ReadFile(filepath.Join(dir, "record.json"))
				if err != nil {
					t.Fatalf("read snapshot: %v", err)
				}
				var snap Snapshot
				if err := json.Unmarshal(data, &snap); err != nil {
					t.Fatalf("decode snapshot: %v", err)
				}
				snap.JournalDigest = "sha256:" + strings.Repeat("0", 64)
				data, err = marshalJSON(snap)
				if err != nil {
					t.Fatalf("marshal snapshot: %v", err)
				}
				writeFile(t, filepath.Join(dir, "record.json"), string(data))
			},
		},
		{
			name: "tampered snapshot record",
			mutate: func(dir string) {
				data, err := os.ReadFile(filepath.Join(dir, "record.json"))
				if err != nil {
					t.Fatalf("read snapshot: %v", err)
				}
				var snap Snapshot
				if err := json.Unmarshal(data, &snap); err != nil {
					t.Fatalf("decode snapshot: %v", err)
				}
				snap.Record.Phase = "tampered"
				data, err = marshalJSON(snap)
				if err != nil {
					t.Fatalf("marshal snapshot: %v", err)
				}
				writeFile(t, filepath.Join(dir, "record.json"), string(data))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := testStore(t)
			created := mustCreate(t, store, "op-"+tt.name)
			updated, err := store.Update(created.OperationID, "phase", "phase", func(record OperationRecord) (OperationRecord, error) {
				record.Phase = "prepare"
				record.PhaseIndex = 1
				return record, nil
			})
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			dir := filepath.Join(store.Root, created.OperationID)
			tt.mutate(dir)

			read, err := store.Read(created.OperationID)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if read.LatestJournalSeq != updated.LatestJournalSeq || read.Phase != "prepare" {
				t.Fatalf("rebuilt record = %#v, want %#v", read, updated)
			}
			var rebuilt Snapshot
			data, err := os.ReadFile(filepath.Join(dir, "record.json"))
			if err != nil {
				t.Fatalf("read rebuilt snapshot: %v", err)
			}
			if err := json.Unmarshal(data, &rebuilt); err != nil {
				t.Fatalf("decode rebuilt snapshot: %v", err)
			}
			if rebuilt.LatestSeq != updated.LatestJournalSeq || rebuilt.JournalDigest == "" {
				t.Fatalf("rebuilt snapshot = %#v", rebuilt)
			}
		})
	}
}

func TestStoreRejectsCreateWhenOnlyJournalExists(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-partial-create")
	dir := filepath.Join(store.Root, created.OperationID)
	if err := os.Remove(filepath.Join(dir, "record.json")); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	_, err := store.Create(baseRecord(created.OperationID), "accepted-again", time.Date(2026, 6, 15, 12, 5, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "already has a journal") {
		t.Fatalf("Create() error = %v, want existing journal", err)
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.OperationID != created.OperationID || read.LatestJournalSeq != 1 {
		t.Fatalf("recovered record = %#v", read)
	}
}

func TestStoreIgnoresCorruptJournalEventAndUsesHighestValidSequence(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-corrupt-journal")
	updated, err := store.Update(created.OperationID, "phase", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.Phase = "prepare"
		record.PhaseIndex = 1
		return record, nil
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	dir := filepath.Join(store.Root, created.OperationID)
	writeFile(t, filepath.Join(dir, "journal", "00000000000000000003.bad.json"), "{bad-json")
	if err := os.Remove(filepath.Join(dir, "record.json")); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.LatestJournalSeq != updated.LatestJournalSeq || read.Phase != updated.Phase {
		t.Fatalf("read after corrupt journal = %#v", read)
	}
}

func TestStoreRejectsDuplicateValidJournalSequence(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-duplicate-seq")
	if _, err := store.Update(created.OperationID, "phase", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.Phase = "prepare"
		record.PhaseIndex = 1
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	dir := filepath.Join(store.Root, created.OperationID, "journal")
	data, err := os.ReadFile(filepath.Join(dir, "00000000000000000002.phase.json"))
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	writeFile(t, filepath.Join(dir, "00000000000000000002.duplicate.json"), string(data))

	_, err = store.Read(created.OperationID)
	if err == nil || !strings.Contains(err.Error(), "duplicate valid journal sequence") {
		t.Fatalf("Read() error = %v, want duplicate sequence", err)
	}
}

func TestStoreRejectsInvalidTransitionsAndTerminalMutation(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-invalid")
	advanced, err := store.Update(created.OperationID, "phase", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.PhaseIndex = 2
		record.CompletedPhases = []string{"accepted", "prepare"}
		return record, nil
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	_, err = store.Update(advanced.OperationID, "rewind", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.PhaseIndex = 1
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "phaseIndex") {
		t.Fatalf("phase rewind error = %v, want phaseIndex", err)
	}
	_, err = store.Update(advanced.OperationID, "completed-rewrite", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.CompletedPhases = []string{"accepted"}
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "completedPhases") {
		t.Fatalf("completed rewrite error = %v, want completedPhases", err)
	}
	terminal, err := store.Update(advanced.OperationID, "complete", "terminal", func(record OperationRecord) (OperationRecord, error) {
		now := time.Date(2026, 6, 15, 12, 15, 0, 0, time.UTC)
		record.Terminal = true
		record.Result = ResultSucceeded
		record.CompletedAt = &now
		return record, nil
	})
	if err != nil {
		t.Fatalf("terminal update error = %v", err)
	}
	_, err = store.Update(terminal.OperationID, "after-terminal", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.NextAction = "should not change"
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal mutation error = %v, want terminal immutable", err)
	}
}

func TestStoreAppendsMutationMarkersAndScopes(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-marker")
	marked, err := store.AddPreExecMutationMarker(created.OperationID, PreExecMutationMarker{
		MarkerID:               "marker-1",
		Phase:                  "kubeadm-init",
		Tool:                   "kubeadm",
		ArgvDigest:             "sha256:" + strings.Repeat("a", 64),
		ExpectedMutationScopes: []string{"etc-kubernetes", "kubelet-state"},
		MarkedAt:               time.Date(2026, 6, 15, 12, 20, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("AddPreExecMutationMarker() error = %v", err)
	}
	if !marked.ExternalMutationStarted || len(marked.PreExecMutationMarkers) != 1 || len(marked.MutationScopes) != 2 {
		t.Fatalf("marked record = %#v", marked)
	}
	if got := ClassifyStale(marked); got != StalePostMutation {
		t.Fatalf("ClassifyStale() = %s, want %s", got, StalePostMutation)
	}

	_, err = store.Update(created.OperationID, "marker-rewrite", "phase", func(record OperationRecord) (OperationRecord, error) {
		record.PreExecMutationMarkers[0].Tool = "kubectl"
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "pre-exec mutation markers") {
		t.Fatalf("marker rewrite error = %v, want append-only marker error", err)
	}
}

func TestStoreAddsRedactedDiagnosticArtifact(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-artifact")
	updated, err := store.AddDiagnosticArtifact(created.OperationID, "kubeadm-output", []byte("redacted output\n"), time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AddDiagnosticArtifact() error = %v", err)
	}
	if len(updated.DiagnosticArtifacts) != 1 || !updated.DiagnosticArtifacts[0].Redacted {
		t.Fatalf("diagnostic artifacts = %#v", updated.DiagnosticArtifacts)
	}
	assertExists(t, filepath.Join(store.Root, created.OperationID, "attachments", "kubeadm-output.log"))
	assertDirMode(t, filepath.Join(store.Root, created.OperationID, "attachments"), 0o700)
}

func TestStoreRejectsDiagnosticArtifactRewriteAndInvalidMetadata(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-artifact-invalid")
	updated, err := store.AddDiagnosticArtifact(created.OperationID, "kubeadm-output", []byte("redacted output\n"), time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AddDiagnosticArtifact() error = %v", err)
	}

	_, err = store.Update(updated.OperationID, "artifact-remove", "diagnostic-artifact", func(record OperationRecord) (OperationRecord, error) {
		record.DiagnosticArtifacts = nil
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "diagnosticArtifacts") {
		t.Fatalf("artifact removal error = %v, want append-only artifact error", err)
	}
	_, err = store.Update(updated.OperationID, "artifact-rewrite", "diagnostic-artifact", func(record OperationRecord) (OperationRecord, error) {
		record.DiagnosticArtifacts[0].Path = "attachments/changed.log"
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "diagnosticArtifacts") {
		t.Fatalf("artifact rewrite error = %v, want append-only artifact error", err)
	}
	_, err = store.Update(updated.OperationID, "artifact-bad-path", "diagnostic-artifact", func(record OperationRecord) (OperationRecord, error) {
		record.DiagnosticArtifacts = append(record.DiagnosticArtifacts, DiagnosticArtifact{
			ArtifactID: "bad-path",
			Path:       "../bad.log",
			SHA256:     strings.Repeat("a", 64),
			Redacted:   true,
			CreatedAt:  time.Date(2026, 6, 15, 12, 31, 0, 0, time.UTC),
		})
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "under attachments") {
		t.Fatalf("bad artifact path error = %v, want attachments path error", err)
	}
	_, err = store.Update(updated.OperationID, "artifact-bad-digest", "diagnostic-artifact", func(record OperationRecord) (OperationRecord, error) {
		record.DiagnosticArtifacts = append(record.DiagnosticArtifacts, DiagnosticArtifact{
			ArtifactID: "bad-digest",
			Path:       "attachments/bad-digest.log",
			SHA256:     "sha256:" + strings.Repeat("b", 64),
			Redacted:   true,
			CreatedAt:  time.Date(2026, 6, 15, 12, 32, 0, 0, time.UTC),
		})
		return record, nil
	})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("bad artifact digest error = %v, want digest error", err)
	}
	_, err = store.AddDiagnosticArtifact(updated.OperationID, `bad\id`, []byte("redacted\n"), time.Date(2026, 6, 15, 12, 33, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "clean path segment") {
		t.Fatalf("bad artifact id error = %v, want clean path segment error", err)
	}
}

func TestStoreDoesNotWriteDiagnosticArtifactForTerminalRecord(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-terminal-artifact")
	completedAt := time.Date(2026, 6, 15, 12, 35, 0, 0, time.UTC)
	terminal, err := store.Update(created.OperationID, "complete", "terminal", func(record OperationRecord) (OperationRecord, error) {
		record.Terminal = true
		record.Result = ResultSucceeded
		record.CompletedAt = &completedAt
		return record, nil
	})
	if err != nil {
		t.Fatalf("terminal update error = %v", err)
	}

	_, err = store.AddDiagnosticArtifact(terminal.OperationID, "late-output", []byte("late\n"), time.Date(2026, 6, 15, 12, 36, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal artifact error = %v, want terminal immutable", err)
	}
	path := filepath.Join(store.Root, terminal.OperationID, "attachments", "late-output.log")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("terminal artifact path stat error = %v, want not exist", err)
	}
}

func TestClassifyStaleRecords(t *testing.T) {
	base := baseRecord("op-stale")
	base.RecordRevision = 1
	base.LatestJournalSeq = 1
	for _, tt := range []struct {
		name   string
		mutate func(OperationRecord) OperationRecord
		want   string
	}{
		{name: "pre mutation", mutate: func(record OperationRecord) OperationRecord {
			record.OperationKind = "status-report"
			record.Scope = "kubeadm-state"
			return record
		}, want: StalePreMutation},
		{name: "host only", mutate: func(record OperationRecord) OperationRecord {
			record.Scope = "host-generation"
			record.PhaseIndex = 1
			record.CompletedPhases = []string{"activate"}
			return record
		}, want: StaleHostOnly},
		{name: "post mutation", mutate: func(record OperationRecord) OperationRecord {
			record.ExternalMutationStarted = true
			return record
		}, want: StalePostMutation},
		{name: "mutating phase", mutate: func(record OperationRecord) OperationRecord {
			record.Phase = "kubeadm-init"
			return record
		}, want: StalePostMutation},
		{name: "mutating kind without proof", mutate: func(record OperationRecord) OperationRecord {
			record.OperationKind = "bootstrap-join-worker"
			record.Scope = "kubeadm-state"
			record.PhaseIndex = 0
			record.CompletedPhases = nil
			return record
		}, want: StaleAmbiguous},
		{name: "ambiguous", mutate: func(record OperationRecord) OperationRecord {
			record.OperationKind = "status-report"
			record.Scope = "kubeadm-state"
			record.PhaseIndex = 1
			return record
		}, want: StaleAmbiguous},
		{name: "terminal", mutate: func(record OperationRecord) OperationRecord {
			now := time.Date(2026, 6, 15, 12, 40, 0, 0, time.UTC)
			record.Terminal = true
			record.Result = ResultTimedOut
			record.CompletedAt = &now
			return record
		}, want: StaleTerminal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyStale(tt.mutate(base)); got != tt.want {
				t.Fatalf("ClassifyStale() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestReconcileBootClassifiesAndPreservesRecovery(t *testing.T) {
	store := testStore(t)
	post := mustCreate(t, store, "op-post-mutation")
	if _, err := store.AddPreExecMutationMarker(post.OperationID, PreExecMutationMarker{
		MarkerID:               "marker-1",
		Phase:                  "kubeadm-init",
		Tool:                   "kubeadm",
		ArgvDigest:             "sha256:" + strings.Repeat("a", 64),
		ExpectedMutationScopes: []string{"etc-kubernetes"},
		MarkedAt:               time.Date(2026, 6, 15, 12, 50, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("AddPreExecMutationMarker() error = %v", err)
	}
	terminal := mustCreate(t, store, "op-terminal")
	completedAt := time.Date(2026, 6, 15, 12, 55, 0, 0, time.UTC)
	if _, err := store.Update(terminal.OperationID, "complete", "terminal", func(record OperationRecord) (OperationRecord, error) {
		record.Terminal = true
		record.Result = ResultSucceeded
		record.CompletedAt = &completedAt
		return record, nil
	}); err != nil {
		t.Fatalf("terminal update error = %v", err)
	}

	report, err := store.ReconcileBoot(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC), "boot-1", nil)
	if err != nil {
		t.Fatalf("ReconcileBoot() error = %v", err)
	}
	if len(report.Operations) != 2 {
		t.Fatalf("operations = %#v", report.Operations)
	}
	got := map[string]ReconciledOperation{}
	for _, op := range report.Operations {
		got[op.OperationID] = op
	}
	if got[post.OperationID].StaleClass != StalePostMutation || !got[post.OperationID].RecoveryRequired || got[post.OperationID].Result != ResultFailedNeedsRepair {
		t.Fatalf("post-mutation reconcile = %#v", got[post.OperationID])
	}
	if got[terminal.OperationID].StaleClass != StaleNotStale || got[terminal.OperationID].RecoveryRequired {
		t.Fatalf("terminal reconcile = %#v", got[terminal.OperationID])
	}
	read, err := store.Read(post.OperationID)
	if err != nil {
		t.Fatalf("Read(post) error = %v", err)
	}
	if !read.RecoveryRequired || read.Interruption != StalePostMutation || read.Result != ResultFailedNeedsRepair {
		t.Fatalf("post record = %#v", read)
	}
}

func TestReconcileBootLeavesCurrentBootInvocationNotStale(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-live")
	if _, err := store.Update(created.OperationID, "start", "tool-start", func(record OperationRecord) (OperationRecord, error) {
		record.Invocations = append(record.Invocations, InvocationRecord{
			InvocationID:        "marker-1",
			SystemdInvocationID: "systemd-live",
			BootID:              "boot-current",
			StartedAt:           time.Date(2026, 6, 15, 12, 45, 0, 0, time.UTC),
			Result:              "started",
		})
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	report, err := store.ReconcileBoot(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC), "boot-current", func(invocation InvocationRecord) bool {
		return invocation.SystemdInvocationID == "systemd-live"
	})
	if err != nil {
		t.Fatalf("ReconcileBoot() error = %v", err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != StaleNotStale {
		t.Fatalf("report = %#v", report)
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.RecoveryRequired || read.Interruption != "" {
		t.Fatalf("live record was mutated as stale: %#v", read)
	}
}

func TestReconcileBootRequiresProvenLiveCurrentBootInvocation(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-not-live")
	if _, err := store.Update(created.OperationID, "start", "tool-start", func(record OperationRecord) (OperationRecord, error) {
		record.Invocations = append(record.Invocations, InvocationRecord{
			InvocationID:        "marker-1",
			SystemdInvocationID: "systemd-dead",
			BootID:              "boot-current",
			StartedAt:           time.Date(2026, 6, 15, 12, 45, 0, 0, time.UTC),
			Result:              "started",
		})
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	report, err := store.ReconcileBoot(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC), "boot-current", func(InvocationRecord) bool {
		return false
	})
	if err != nil {
		t.Fatalf("ReconcileBoot() error = %v", err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass == StaleNotStale {
		t.Fatalf("report = %#v", report)
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.Interruption == "" {
		t.Fatalf("dead invocation was not reconciled: %#v", read)
	}
}

func TestReconcileBootFinishesResumableHostBookkeeping(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-host-bookkeeping")
	if _, err := store.Update(created.OperationID, "host-bookkeeping", "host-bookkeeping", func(record OperationRecord) (OperationRecord, error) {
		record.OperationKind = HostBookkeepingOperationKind
		record.Scope = HostBookkeepingGenerationScope
		record.Phase = HostBookkeepingCompletionPhase
		record.PhaseIndex = 1
		record.CompletedPhases = []string{"accepted"}
		record.Resume = ResumeHostBookkeeping
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	report, err := store.ReconcileBoot(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC), "boot-1", nil)
	if err != nil {
		t.Fatalf("ReconcileBoot() error = %v", err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != StaleHostOnly || report.Operations[0].Result != ResultSucceeded {
		t.Fatalf("report = %#v", report)
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !read.Terminal || read.CompletedAt == nil || read.Result != ResultSucceeded || read.RecoveryRequired {
		t.Fatalf("host bookkeeping record = %#v", read)
	}
}

func TestReconcileBootDoesNotFinishGenericHostBookkeeping(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-generic-host-bookkeeping")
	if _, err := store.Update(created.OperationID, "host-bookkeeping", "host-bookkeeping", func(record OperationRecord) (OperationRecord, error) {
		record.OperationKind = HostBookkeepingOperationKind
		record.Scope = HostBookkeepingGenerationScope
		record.Phase = "write-status"
		record.PhaseIndex = 1
		record.CompletedPhases = []string{"accepted"}
		record.Resume = ResumeHostBookkeeping
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	report, err := store.ReconcileBoot(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC), "boot-1", nil)
	if err != nil {
		t.Fatalf("ReconcileBoot() error = %v", err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != StaleHostOnly || report.Operations[0].Result == ResultSucceeded {
		t.Fatalf("report = %#v", report)
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.Terminal || read.Result == ResultSucceeded || read.NextAction != "classified host-only; no automatic resume marker present" {
		t.Fatalf("generic host bookkeeping record = %#v", read)
	}
}

func TestReconcileBootDoesNotFinishHostBookkeepingWithToolState(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-host-bookkeeping-tool-state")
	if _, err := store.Update(created.OperationID, "host-bookkeeping", "host-bookkeeping", func(record OperationRecord) (OperationRecord, error) {
		record.OperationKind = HostBookkeepingOperationKind
		record.Scope = HostBookkeepingGenerationScope
		record.Phase = HostBookkeepingCompletionPhase
		record.PhaseIndex = 1
		record.CompletedPhases = []string{"accepted"}
		record.Resume = ResumeHostBookkeeping
		record.MutatingToolInvocations = []string{"kubeadm init"}
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	report, err := store.ReconcileBoot(time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC), "boot-1", nil)
	if err != nil {
		t.Fatalf("ReconcileBoot() error = %v", err)
	}
	if len(report.Operations) != 1 || report.Operations[0].StaleClass != StaleHostOnly || report.Operations[0].Result == ResultSucceeded {
		t.Fatalf("report = %#v", report)
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.Terminal || read.Result == ResultSucceeded {
		t.Fatalf("host bookkeeping with tool state completed: %#v", read)
	}
}

func TestOperationIDsRejectsInvalidDirectory(t *testing.T) {
	store := testStore(t)
	if err := os.MkdirAll(filepath.Join(store.Root, "bad..id"), 0o700); err != nil {
		t.Fatalf("mkdir invalid operation: %v", err)
	}
	_, err := store.OperationIDs()
	if err == nil || !strings.Contains(err.Error(), "invalid operation directory") {
		t.Fatalf("OperationIDs() error = %v, want invalid directory", err)
	}
}

func TestUpdateMissingOperationDoesNotCreateDirectory(t *testing.T) {
	store := testStore(t)
	_, err := store.Update("missing-operation", "event", "event", func(record OperationRecord) (OperationRecord, error) {
		return record, nil
	})
	if err == nil {
		t.Fatal("Update() error = nil, want missing operation")
	}
	if _, statErr := os.Stat(filepath.Join(store.Root, "missing-operation")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing operation dir stat error = %v, want not exist", statErr)
	}
}

func TestStoreSerializesConcurrentUpdates(t *testing.T) {
	store := testStore(t)
	created := mustCreate(t, store, "op-concurrent")
	const updates = 8
	var wg sync.WaitGroup
	errs := make(chan error, updates)
	for i := 0; i < updates; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update(created.OperationID, "event-"+string(rune('a'+i)), "phase", func(record OperationRecord) (OperationRecord, error) {
				record.MutatingToolInvocations = append(record.MutatingToolInvocations, string(rune('a'+i)))
				return record, nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	}
	read, err := store.Read(created.OperationID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.LatestJournalSeq != updates+1 || len(read.MutatingToolInvocations) != updates {
		t.Fatalf("concurrent record = %#v", read)
	}
}

func TestStoreRejectsIncompleteOperationRecord(t *testing.T) {
	store := testStore(t)
	record := baseRecord("op-bad")
	record.RequestDigest = ""
	_, err := store.Create(record, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "requestDigest") {
		t.Fatalf("Create() error = %v, want requestDigest", err)
	}
}

func testStore(t *testing.T) Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}

func mustCreate(t *testing.T, store Store, id string) OperationRecord {
	t.Helper()
	record, err := store.Create(baseRecord(id), "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return record
}

func baseRecord(id string) OperationRecord {
	return OperationRecord{
		OperationID:           id,
		OperationKind:         "bootstrap-init",
		Scope:                 "kubeadm-state",
		Actor:                 "test",
		RequestDigest:         "sha256:" + strings.Repeat("1", 64),
		PhasePlan:             []string{"accepted", "prepare", "run"},
		Phase:                 "accepted",
		ResourceLocks:         []string{"generation-state.lock", "kubeadm-state.lock"},
		PreviousGenerationID:  "0",
		CandidateGenerationID: "1",
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func assertDirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("%s mode = %04o, want %04o", path, got, mode)
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
