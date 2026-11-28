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

	"github.com/katl-dev/katl/internal/generation"
	installer "github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

var acceptedRecords = []persistedrecord.Handler{
	{RecordType: "katl.generation.spec", RecordVersion: 1},
	{RecordType: "katl.generation.status", RecordVersion: 1},
	{RecordType: "katl.generation.config-apply-status", RecordVersion: 1},
	{RecordType: "katl.boot.selection", RecordVersion: 1},
	{RecordType: "katl.boot.move-intent", RecordVersion: 1},
	{RecordType: "katl.install.status", RecordVersion: 1},
	{RecordType: "katl.operation.record", RecordVersion: 1},
	{RecordType: "katl.operation.journal-event", RecordVersion: 1},
	{RecordType: "katl.cluster.intent", RecordVersion: 1},
	{RecordType: "katl.config-request.decision", RecordVersion: 1},
}

var acceptedFixtureFiles = []string{
	"katl.boot.move-intent.json",
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

	statusPath, err := installstatus.TargetStatusPath(root)
	if err != nil {
		t.Fatalf("TargetStatusPath() error = %v", err)
	}
	writeFixture(t, statusPath, "v1", "katl.install.status.json")
	installRecord, err := installstatus.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if installRecord.InstalledGeneration != "gen-0" || installRecord.KatlosImage.Version != "0.1.0" {
		t.Fatalf("archived install identity = %#v", installRecord)
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
