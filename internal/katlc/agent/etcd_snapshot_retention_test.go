package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/apivip"
	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestPruneUpgradeSnapshots(t *testing.T) {
	root := t.TempDir()
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	directory := filepath.Join(root, "var/lib/etcd/katl-snapshots")

	// Names sort opposite to age, so ordering by name instead of time fails.
	for name, age := range map[string]time.Duration{
		"kubeadm-upgrade-a.db":         7 * time.Hour,
		"kubeadm-upgrade-b.db":         6 * time.Hour,
		"kubeadm-upgrade-c.db":         5 * time.Hour,
		"kubeadm-upgrade-prepare-d.db": 4 * time.Hour,
		"kubeadm-upgrade-prepare-e.db": 3 * time.Hour,
		"kubeadm-upgrade-y.db":         2 * time.Hour,
		"kubeadm-upgrade-z.db":         1 * time.Hour,
		"manual-20260101T000000Z.db":   48 * time.Hour,
		"kubeadm-upgrade-h.db.part":    48 * time.Hour,
	} {
		path := filepath.Join(directory, name)
		writeTestFile(t, path, "snapshot")
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	for _, record := range []struct {
		id       string
		snapshot string
		terminal bool
		recovery bool
	}{
		{id: "active", snapshot: "kubeadm-upgrade-a.db"},
		{id: "recovery", snapshot: "kubeadm-upgrade-c.db", terminal: true, recovery: true},
		{id: "done", snapshot: "kubeadm-upgrade-b.db", terminal: true},
	} {
		update := &operation.KubernetesSysextUpdate{TargetPayloadVersion: "v1.36.2", TargetSysextPath: "/var/lib/katl/artifacts/kubernetes.raw", TargetSysextSHA256: strings.Repeat("b", 64), TargetSysextSize: 1, CandidateGenerationID: "gen1", UpgradeRole: "apply", SourcePayloadVersion: "v1.36.1", SnapshotRef: "pre-" + record.id, SnapshotDigest: strings.Repeat("c", 64), SnapshotCreatedAt: now.Format(time.RFC3339), CapturedMemberListDigest: strings.Repeat("d", 64), SnapshotStorageLocation: "/var/lib/etcd/katl-snapshots/" + record.snapshot, SnapshotOperatorIdentity: "operator"}
		created := operation.OperationRecord{OperationID: record.id, OperationKind: OperationKindKubeadmUpgradePrepare, Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "accepted", KubernetesSysextUpdate: update, KubeadmUpgradeEvidence: &operation.KubeadmUpgradeEvidence{TargetKubeadmAccessMode: kubeadmAccessOperationPrivate, KubeletActivationGate: kubeletGateOperationReleased, KubeletGateState: "locked", SourceKubeletPolicy: "keep-running"}}
		if record.terminal {
			created.Terminal = true
			created.RecoveryRequired = record.recovery
			created.Result = operation.ResultSucceeded
			if record.recovery {
				created.Result = "failed"
			}
			created.CompletedAt = &now
		}
		if _, err := store.Create(created, "accepted", now); err != nil {
			t.Fatal(err)
		}
	}

	executor := NewExecutor(root, store, "agent-test")
	retained, err := executor.pruneUpgradeSnapshots()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"/var/lib/etcd/katl-snapshots/kubeadm-upgrade-z.db",
		"/var/lib/etcd/katl-snapshots/kubeadm-upgrade-y.db",
		"/var/lib/etcd/katl-snapshots/kubeadm-upgrade-c.db",
		"/var/lib/etcd/katl-snapshots/kubeadm-upgrade-a.db",
	}
	if !slices.Equal(retained, want) {
		t.Fatalf("retained = %v, want %v", retained, want)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var remaining []string
	for _, entry := range entries {
		remaining = append(remaining, entry.Name())
	}
	wantRemaining := []string{"kubeadm-upgrade-a.db", "kubeadm-upgrade-c.db", "kubeadm-upgrade-h.db.part", "kubeadm-upgrade-y.db", "kubeadm-upgrade-z.db", "manual-20260101T000000Z.db"}
	if !slices.Equal(remaining, wantRemaining) {
		t.Fatalf("remaining files = %v, want %v", remaining, wantRemaining)
	}
}

func TestKubeadmUpgradeRetainsNewestSnapshots(t *testing.T) {
	for _, tc := range []struct {
		role         string
		wantRetained []string
		wantFiles    []string
	}{
		{
			role:         "apply",
			wantRetained: []string{"/var/lib/etcd/katl-snapshots/kubeadm-upgrade-prepare-new.db", "/var/lib/etcd/katl-snapshots/kubeadm-upgrade-prepare-mid.db"},
			wantFiles:    []string{"kubeadm-upgrade-prepare-mid.db", "kubeadm-upgrade-prepare-new.db"},
		},
		{
			role:      "worker",
			wantFiles: []string{"kubeadm-upgrade-prepare-mid.db", "kubeadm-upgrade-prepare-new.db", "kubeadm-upgrade-prepare-old.db"},
		},
	} {
		t.Run(tc.role, func(t *testing.T) {
			root, store, record, now := kubeadmUpgradeFixture(t, tc.role)
			writeTestFile(t, filepath.Join(root, apivip.OwnershipEnabledPath), "enabled\n")
			directory := filepath.Join(root, "var/lib/etcd/katl-snapshots")
			for name, age := range map[string]time.Duration{"kubeadm-upgrade-prepare-old.db": 3 * time.Hour, "kubeadm-upgrade-prepare-mid.db": 2 * time.Hour, "kubeadm-upgrade-prepare-new.db": time.Hour} {
				path := filepath.Join(directory, name)
				writeTestFile(t, path, "snapshot")
				if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
					t.Fatal(err)
				}
			}
			var commands [][]string
			executor := newUpgradeExecutor(root, store)
			executor.Async = false
			executor.Now = func() time.Time { return now.Add(time.Minute) }
			executor.WaitBeforeKubeadm = func(context.Context, time.Duration) error { return nil }
			executor.SetBootDefault = func(context.Context, string, string) error { return nil }
			executor.SetBootOneshot = func(context.Context, string, string) error { return nil }
			executor.RunTool = successfulUpgradeTool(&commands)

			if err := executor.Execute(context.Background(), record); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			completed, err := store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if !completed.Terminal || completed.Result != operation.ResultSucceeded {
				t.Fatalf("completed operation = %+v", completed)
			}

			var retained []string
			if etcd := completed.KubeadmUpgradeEvidence.Etcd; etcd != nil {
				retained = etcd.RetainedSnapshots
			}
			if !slices.Equal(retained, tc.wantRetained) {
				t.Fatalf("retained snapshots = %v, want %v", retained, tc.wantRetained)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, entry := range entries {
				files = append(files, entry.Name())
			}
			if !slices.Equal(files, tc.wantFiles) {
				t.Fatalf("snapshot files = %v, want %v", files, tc.wantFiles)
			}
		})
	}
}
