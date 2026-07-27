package persistedcompat_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	installer "github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

var acceptedRecords = []persistedrecord.Handler{
	{RecordType: "katl.generation.spec", RecordVersion: 1},
	{RecordType: "katl.generation.status", RecordVersion: 1},
	{RecordType: "katl.generation.config-apply-status", RecordVersion: 1},
	{RecordType: "katl.boot.selection", RecordVersion: 1},
	{RecordType: "katl.install.status", RecordVersion: 1},
	{RecordType: "katl.operation.record", RecordVersion: 1},
	{RecordType: "katl.operation.journal-event", RecordVersion: 1},
	{RecordType: "katl.cluster.intent", RecordVersion: 1},
	{RecordType: "katl.config-request.decision", RecordVersion: 1},
}

var acceptedFixtureFiles = []string{
	"katl.boot.selection.json",
	"katl.cluster.intent.json",
	"katl.config-request.decision.json",
	"katl.generation.config-apply-status.json",
	"katl.generation.spec.json",
	"katl.generation.status.json",
	"katl.install.status.json",
	"katl.operation.journal-event.json",
	"katl.operation.record.json",
}

func TestAcceptedRecordFixturesCoverEveryPersistedRecord(t *testing.T) {
	entries, err := os.ReadDir(fixturePath("v1"))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var got []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			got = append(got, entry.Name())
		}
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(acceptedFixtureFiles, "\n") {
		t.Fatalf("accepted fixture files:\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(acceptedFixtureFiles, "\n"))
	}

	seen := map[persistedrecord.Key]string{}
	for _, name := range acceptedFixtureFiles {
		envelope := readEnvelope(t, filepath.Join("v1", name))
		key := persistedrecord.Key{RecordType: envelope.RecordType, RecordVersion: envelope.RecordVersion}
		if previous := seen[key]; previous != "" {
			t.Fatalf("%s duplicates persisted record key already covered by %s", name, previous)
		}
		seen[key] = name
	}

	for _, handler := range acceptedRecords {
		key := persistedrecord.Key{RecordType: handler.RecordType, RecordVersion: handler.RecordVersion}
		if seen[key] == "" {
			t.Fatalf("missing accepted fixture for %s/v%d", handler.RecordType, handler.RecordVersion)
		}
	}
}

func TestAcceptedRecordFixturesDecodeValidateAndReplay(t *testing.T) {
	root := t.TempDir()
	genDir := filepath.Join(root, "var/lib/katl/generations/gen-0")
	writeFixture(t, filepath.Join(genDir, "spec.json"), "v1", "katl.generation.spec.json")
	writeFixture(t, filepath.Join(genDir, "status.json"), "v1", "katl.generation.status.json")

	spec, status, err := generation.ReadGeneration(root, "gen-0")
	if err != nil {
		t.Fatalf("ReadGeneration() error = %v", err)
	}
	if spec.GenerationID != "gen-0" || status.GenerationID != "gen-0" {
		t.Fatalf("generation fixture ids = %q/%q", spec.GenerationID, status.GenerationID)
	}
	digest, err := generation.CanonicalSpecDigest(spec)
	if err != nil {
		t.Fatalf("CanonicalSpecDigest() error = %v", err)
	}
	if status.SpecDigest != digest {
		t.Fatalf("generation status digest = %q, want %q", status.SpecDigest, digest)
	}

	configStatusPath := filepath.Join(root, "var/lib/katl/generations/gen-1/config-apply-status.json")
	writeFixture(t, configStatusPath, "v1", "katl.generation.config-apply-status.json")
	configStatus, err := generation.ReadConfigApplyStatus(configStatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if configStatus.GenerationID != "gen-1" {
		t.Fatalf("config apply status generationID = %q", configStatus.GenerationID)
	}

	bootPath, err := generation.BootSelectionPath(root)
	if err != nil {
		t.Fatalf("BootSelectionPath() error = %v", err)
	}
	writeFixture(t, bootPath, "v1", "katl.boot.selection.json")
	bootSelection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if bootSelection.DefaultGenerationID != "gen-0" {
		t.Fatalf("boot selection default generation = %q", bootSelection.DefaultGenerationID)
	}

	statusPath, err := installstatus.RuntimeStatusPath(root)
	if err != nil {
		t.Fatalf("RuntimeStatusPath() error = %v", err)
	}
	writeFixture(t, statusPath, "v1", "katl.install.status.json")
	installRecord, err := installstatus.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if err := installstatus.ValidateRuntimeHandoff(installRecord); err != nil {
		t.Fatalf("ValidateRuntimeHandoff() error = %v", err)
	}

	clusterPath := filepath.Join(root, "var/lib/katl/cluster/intent.json")
	writeFixture(t, clusterPath, "v1", "katl.cluster.intent.json")
	intent, intentDigest, err := installer.ReadClusterIntent(root)
	if err != nil {
		t.Fatalf("ReadClusterIntent() error = %v", err)
	}
	if intent.GenerationID != "gen-0" || intentDigest == "" {
		t.Fatalf("cluster intent = %#v digest %q", intent, intentDigest)
	}

	auditEnvelope := readEnvelope(t, filepath.Join("v1", "katl.config-request.decision.json"))
	audit, err := persistedrecord.DecodePayload[configapply.ConfigRequestAudit](auditEnvelope)
	if err != nil {
		t.Fatalf("DecodePayload[ConfigRequestAudit]() error = %v", err)
	}
	if audit.Kind != configapply.ConfigRequestAuditKind || audit.Decision != configapply.DecisionAccepted {
		t.Fatalf("config request audit = %#v", audit)
	}

	assertOperationFixturesReplay(t, root)
}

func TestPersistedStateRollbackCompatibilityEquivalent(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)

	previous := readPayloadFixture[generation.GenerationSpec](t, "v1", "katl.generation.spec.json")
	previousStatus, err := generation.NewGenerationStatus(previous, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("NewGenerationStatus(previous) error = %v", err)
	}
	if err := generation.WriteGeneration(root, previous, previousStatus); err != nil {
		t.Fatalf("WriteGeneration(previous) error = %v", err)
	}
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:            generation.APIVersion,
		Kind:                  generation.BootSelectionKind,
		DefaultGenerationID:   previous.GenerationID,
		BootedGenerationID:    previous.GenerationID,
		Generation0FallbackID: previous.GenerationID,
		DefaultBootEntry:      previous.Boot.LoaderEntryPath,
		BootedBootEntry:       previous.Boot.LoaderEntryPath,
		UpdatedAt:             now.Add(-90 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection(previous) error = %v", err)
	}

	// This read simulates the candidate runtime entering with existing persisted
	// records before it stages its own trial state.
	readPrevious, readPreviousStatus, err := generation.ReadGeneration(root, previous.GenerationID)
	if err != nil {
		t.Fatalf("candidate ReadGeneration(previous) error = %v", err)
	}
	if !generation.IsKnownGood(readPreviousStatus) {
		t.Fatalf("previous status = %#v, want known-good", readPreviousStatus)
	}
	if _, err := generation.ReadBootSelection(root); err != nil {
		t.Fatalf("candidate ReadBootSelection() error = %v", err)
	}

	candidate := readPrevious
	candidate.GenerationID = "gen-1"
	candidate.RuntimeVersion = "0.1.1"
	candidate.PreviousGenerationID = previous.GenerationID
	candidate.Root.Slot = "root-b"
	candidate.Root.PartitionUUID = "22222222-3333-4444-5555-666666666666"
	candidate.Root.RuntimeVersion = "0.1.1"
	candidate.Root.RuntimeArtifactSHA256 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	candidate.Boot.UKIPath = "/efi/EFI/Linux/katl-gen-1.efi"
	candidate.Boot.LoaderEntryPath = "loader/entries/katl-gen-1.conf"
	candidate.CreatedAt = now.Add(-time.Hour)
	candidateStatus, err := generation.NewGenerationStatus(candidate, generation.CommitStateCandidate, generation.BootStateTrying, generation.HealthStateUnknown, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("NewGenerationStatus(candidate) error = %v", err)
	}
	if err := generation.WriteGeneration(root, candidate, candidateStatus); err != nil {
		t.Fatalf("WriteGeneration(candidate) error = %v", err)
	}

	configStatus, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{
		GenerationID:       candidate.GenerationID,
		PreviousGeneration: previous.GenerationID,
		RequestedApplyMode: generation.ApplyModeNextBoot,
		AcceptedApplyMode:  generation.ApplyModeNextBoot,
		ChangedDomains:     []string{"host-configuration"},
		HealthState:        generation.HealthStateUnknown,
		Kubeadm:            generation.KubeadmActionRequired{Required: false},
		UpdatedAt:          now.Add(-50 * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewConfigApplyStatus() error = %v", err)
	}
	configStatus, err = generation.MarkConfigApplyPhase(configStatus, generation.ConfigApplyPhaseNextBoot, now.Add(-45*time.Minute))
	if err != nil {
		t.Fatalf("MarkConfigApplyPhase(next-boot) error = %v", err)
	}
	configStatusPath, err := generation.ConfigApplyStatusPath(root, candidate.GenerationID)
	if err != nil {
		t.Fatalf("ConfigApplyStatusPath() error = %v", err)
	}
	if err := generation.WriteConfigApplyStatus(configStatusPath, configStatus); err != nil {
		t.Fatalf("WriteConfigApplyStatus(next-boot) error = %v", err)
	}

	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           previous.GenerationID,
		TrialGenerationID:             candidate.GenerationID,
		PreviousKnownGoodGenerationID: previous.GenerationID,
		BootedGenerationID:            candidate.GenerationID,
		Generation0FallbackID:         previous.GenerationID,
		DefaultBootEntry:              previous.Boot.LoaderEntryPath,
		TrialBootEntry:                candidate.Boot.LoaderEntryPath,
		PreviousKnownGoodBootEntry:    previous.Boot.LoaderEntryPath,
		BootedBootEntry:               candidate.Boot.LoaderEntryPath,
		PendingTransactionID:          "op-rollback-compat",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    generation.DefaultPromotionPending,
		UpdatedAt:                     now.Add(-40 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteBootSelection(candidate) error = %v", err)
	}

	result, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root:         root,
		GenerationID: candidate.GenerationID,
		CommandLine:  "root=PARTUUID=22222222-3333-4444-5555-666666666666 katl.generation=gen-1",
		Result:       generation.BootHealthFailure,
		Reason:       "rollback compatibility trial failure",
		Now:          now,
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(candidate failure) error = %v", err)
	}
	if !result.Failed || result.RecoveryRequired || result.DefaultGeneration != previous.GenerationID {
		t.Fatalf("failure result = %#v, want rollback to previous known-good", result)
	}

	configStatus, err = generation.MarkConfigApplyRollback(configStatus, previous.GenerationID, generation.ConfigApplyActionPassed, "trial boot failed", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("MarkConfigApplyRollback() error = %v", err)
	}
	if err := generation.WriteConfigApplyStatus(configStatusPath, configStatus); err != nil {
		t.Fatalf("WriteConfigApplyStatus(rollback) error = %v", err)
	}

	recovered, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root:         root,
		GenerationID: previous.GenerationID,
		CommandLine:  "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=gen-0",
		Result:       generation.BootHealthSuccess,
		Reason:       "previous known-good booted after rollback",
		Now:          now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("RecordBootHealth(previous success) error = %v", err)
	}
	if recovered.DefaultGeneration != previous.GenerationID || recovered.RecoveryRequired {
		t.Fatalf("recovered result = %#v, want clear previous known-good state", recovered)
	}

	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection(after rollback) error = %v", err)
	}
	_, previousAfter, err := generation.ReadGeneration(root, previous.GenerationID)
	if err != nil {
		t.Fatalf("ReadGeneration(previous after rollback) error = %v", err)
	}
	_, candidateAfter, err := generation.ReadGeneration(root, candidate.GenerationID)
	if err != nil {
		t.Fatalf("ReadGeneration(candidate after rollback) error = %v", err)
	}
	rolledBackStatus, err := generation.ReadConfigApplyStatus(configStatusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus(rollback) error = %v", err)
	}
	report := rollbackCompatibilityReport(selection, previousAfter, candidateAfter, rolledBackStatus)
	if report != "default=gen-0 booted=gen-0 failed= recovery=false previous=committed/good/healthy candidate=candidate/failed/unhealthy config=rolled-back->gen-0" {
		t.Fatalf("rollback report = %q", report)
	}
}

func TestInvalidPersistedRecordFixturesAreRejected(t *testing.T) {
	registry := mustRegistry(t)

	t.Run("unsupported newer version", func(t *testing.T) {
		envelope := readEnvelope(t, filepath.Join("invalid", "unsupported-newer-version.json"))
		_, err := registry.Dispatch(envelope)
		assertErr(t, err, persistedrecord.ErrUnsupportedRecord, "katl.boot.selection/v2")
	})

	t.Run("missing payload", func(t *testing.T) {
		_, err := persistedrecord.DecodeEnvelope(readFixture(t, "invalid", "missing-payload.json"))
		assertErr(t, err, persistedrecord.ErrInvalidEnvelope, "payload is required")
	})

	t.Run("unknown payload field", func(t *testing.T) {
		envelope := readEnvelope(t, filepath.Join("invalid", "unknown-payload-field.json"))
		_, err := persistedrecord.DecodePayload[generation.GenerationSpec](envelope)
		assertErr(t, err, persistedrecord.ErrInvalidEnvelope, "unknown field")
	})

	t.Run("path record id mismatch", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "gen-0")
		writeFixture(t, filepath.Join(dir, "spec.json"), "invalid", "path-id-mismatch.spec.json")
		_, _, err := generation.ReadSplitRecords(dir)
		assertErr(t, err, nil, "path id")
	})

	t.Run("digest mismatch", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "gen-0")
		writeFixture(t, filepath.Join(dir, "spec.json"), "v1", "katl.generation.spec.json")
		writeFixture(t, filepath.Join(dir, "status.json"), "invalid", "digest-mismatch.status.json")
		_, _, err := generation.ReadSplitRecords(dir)
		assertErr(t, err, nil, "specDigest mismatch")
	})

	t.Run("invalid enum value", func(t *testing.T) {
		root := t.TempDir()
		path, err := generation.BootSelectionPath(root)
		if err != nil {
			t.Fatalf("BootSelectionPath() error = %v", err)
		}
		writeFixture(t, path, "invalid", "invalid-enum.boot-selection.json")
		_, err = generation.ReadBootSelection(root)
		assertErr(t, err, nil, "unsupported persistent default promotion state")
	})

	t.Run("malformed timestamp", func(t *testing.T) {
		_, err := persistedrecord.DecodeEnvelope(readFixture(t, "invalid", "malformed-timestamp.json"))
		assertErr(t, err, persistedrecord.ErrInvalidEnvelope, "parsing time")
	})
}

func assertOperationFixturesReplay(t *testing.T, root string) {
	t.Helper()

	snapshotEnvelope := readEnvelope(t, filepath.Join("v1", "katl.operation.record.json"))
	snapshot, err := persistedrecord.DecodePayload[operation.Snapshot](snapshotEnvelope)
	if err != nil {
		t.Fatalf("DecodePayload[operation.Snapshot]() error = %v", err)
	}
	if err := operation.ValidateRecord(snapshot.Record); err != nil {
		t.Fatalf("ValidateRecord(snapshot.Record) error = %v", err)
	}

	eventEnvelope := readEnvelope(t, filepath.Join("v1", "katl.operation.journal-event.json"))
	event, err := persistedrecord.DecodePayload[operation.JournalEvent](eventEnvelope)
	if err != nil {
		t.Fatalf("DecodePayload[operation.JournalEvent]() error = %v", err)
	}
	if err := operation.ValidateEvent(event); err != nil {
		t.Fatalf("ValidateEvent() error = %v", err)
	}
	wantDigest := fixtureJournalDigest(t, event)
	if snapshot.JournalDigest != wantDigest {
		t.Fatalf("operation snapshot journalDigest = %q, want %q", snapshot.JournalDigest, wantDigest)
	}

	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	operationDir := filepath.Join(store.Root, "op-fixture")
	recordPath := filepath.Join(operationDir, "record.json")
	writeFixture(t, recordPath, "v1", "katl.operation.record.json")
	writeFixture(t, filepath.Join(operationDir, "journal", "00000000000000000001.accepted.json"), "v1", "katl.operation.journal-event.json")

	before := readFile(t, recordPath)
	record, err := store.Read("op-fixture")
	if err != nil {
		t.Fatalf("Store.Read() error = %v", err)
	}
	if record.OperationID != "op-fixture" {
		t.Fatalf("operation id = %q", record.OperationID)
	}
	after := readFile(t, recordPath)
	if string(after) != string(before) {
		t.Fatalf("operation snapshot fixture was rewritten during replay")
	}
}

func fixtureJournalDigest(t *testing.T, events ...operation.JournalEvent) string {
	t.Helper()

	hash := sha256.New()
	for _, event := range events {
		data, err := json.MarshalIndent(event, "", "  ")
		if err != nil {
			t.Fatalf("MarshalIndent() error = %v", err)
		}
		hash.Write(append(data, '\n'))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func mustRegistry(t *testing.T) persistedrecord.Registry {
	t.Helper()

	registry, err := persistedrecord.NewRegistry(acceptedRecords...)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	return registry
}

func readPayloadFixture[T any](t *testing.T, parts ...string) T {
	t.Helper()

	envelope, err := persistedrecord.DecodeEnvelope(readFixture(t, parts...))
	if err != nil {
		t.Fatalf("DecodeEnvelope(%s) error = %v", filepath.Join(parts...), err)
	}
	payload, err := persistedrecord.DecodePayload[T](envelope)
	if err != nil {
		t.Fatalf("DecodePayload(%s) error = %v", filepath.Join(parts...), err)
	}
	return payload
}

func rollbackCompatibilityReport(selection generation.BootSelectionRecord, previous generation.GenerationStatus, candidate generation.GenerationStatus, configStatus generation.ConfigApplyStatus) string {
	target := ""
	if configStatus.Rollback != nil {
		target = configStatus.Rollback.TargetGenerationID
	}
	return fmt.Sprintf("default=%s booted=%s failed=%s recovery=%t previous=%s/%s/%s candidate=%s/%s/%s config=%s->%s",
		selection.DefaultGenerationID,
		selection.BootedGenerationID,
		selection.FailedBootGenerationID,
		selection.RecoveryRequired,
		previous.CommitState,
		previous.BootState,
		previous.HealthState,
		candidate.CommitState,
		candidate.BootState,
		candidate.HealthState,
		configStatus.Phase,
		target,
	)
}

func readEnvelope(t *testing.T, rel string) persistedrecord.Envelope {
	t.Helper()

	envelope, err := persistedrecord.DecodeEnvelope(readFile(t, fixturePath(rel)))
	if err != nil {
		t.Fatalf("DecodeEnvelope(%s) error = %v", rel, err)
	}
	return envelope
}

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()

	return readFile(t, fixturePath(parts...))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return data
}

func writeFixture(t *testing.T, dst string, parts ...string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, readFixture(t, parts...), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", dst, err)
	}
}

func fixturePath(parts ...string) string {
	all := append([]string{"..", "testdata", "persisted"}, parts...)
	return filepath.Join(all...)
}

func assertErr(t *testing.T, err error, target error, contains string) {
	t.Helper()

	if err == nil {
		t.Fatalf("error = nil, want %q", contains)
	}
	if target != nil && !errors.Is(err, target) {
		t.Fatalf("error = %v, want target %v", err, target)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %v, want substring %q", err, contains)
	}
}

func TestMain(m *testing.M) {
	if err := validateFixtureNames(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func validateFixtureNames() error {
	for _, name := range acceptedFixtureFiles {
		if filepath.Base(name) != name {
			return fmt.Errorf("accepted fixture file must be a base name: %s", name)
		}
	}
	return nil
}
