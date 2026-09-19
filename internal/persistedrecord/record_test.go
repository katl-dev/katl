package persistedrecord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type generationSpecPayload struct {
	GenerationID   string `json:"generationID"`
	RuntimeVersion string `json:"runtimeVersion"`
}

func TestDecodeEnvelopePreservesPayloadForDispatch(t *testing.T) {
	envelope := readFixture(t, filepath.Join("v1", "katl.generation.spec.json"))
	if envelope.RecordType != "katl.generation.spec" || envelope.RecordVersion != 1 {
		t.Fatalf("envelope identity = %s/v%d", envelope.RecordType, envelope.RecordVersion)
	}
	if envelope.WrittenAt == nil || !envelope.WrittenAt.Equal(time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("writtenAt = %#v", envelope.WrittenAt)
	}
	if !strings.Contains(string(envelope.Payload), `"generationID": "gen-0"`) {
		t.Fatalf("payload was not preserved: %s", envelope.Payload)
	}
}

func TestDecodeEnvelopeRejectsInvalidEnvelopeFixtures(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{name: "missing-payload.json", want: "payload is required"},
		{name: "unknown-envelope-field.json", want: "unknown field"},
		{name: "malformed-timestamp.json", want: "parsing time"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := readFixtureBytes(t, tt.name)
			_, err := DecodeEnvelope(data)
			if err == nil || !errors.Is(err, ErrInvalidEnvelope) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeEnvelope() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateEnvelopeRejectsMissingIdentity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		envelope Envelope
		want     string
	}{
		{
			name:     "missing type",
			envelope: Envelope{RecordVersion: 1, Payload: json.RawMessage(`{}`)},
			want:     "recordType is required",
		},
		{
			name:     "missing version",
			envelope: Envelope{RecordType: "katl.generation.spec", Payload: json.RawMessage(`{}`)},
			want:     "recordVersion must be positive",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnvelope(tt.envelope)
			if err == nil || !errors.Is(err, ErrInvalidEnvelope) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateEnvelope() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRegistryDispatchRejectsUnsupportedVersion(t *testing.T) {
	registry := mustRegistry(t, Handler{RecordType: "katl.generation.spec", RecordVersion: 1})
	envelope := readFixture(t, "unsupported-version.json")

	_, err := registry.Dispatch(envelope)
	if err == nil || !errors.Is(err, ErrUnsupportedRecord) || !strings.Contains(err.Error(), "katl.generation.spec/v2") {
		t.Fatalf("Dispatch() error = %v, want unsupported v2", err)
	}
}

func TestDecodePayloadRejectsUnknownPayloadFields(t *testing.T) {
	envelope := Envelope{
		RecordType:    "katl.generation.spec",
		RecordVersion: 1,
		Payload:       json.RawMessage(`{"generationID":"gen-0","runtimeVersion":"0.1.0","metadata":{}}`),
	}
	_, err := DecodePayload[generationSpecPayload](envelope)
	if err == nil || !errors.Is(err, ErrInvalidEnvelope) || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodePayload() error = %v, want unknown payload field", err)
	}
}

func TestRegistryPathMismatchHook(t *testing.T) {
	registry := mustRegistry(t, Handler{
		RecordType:    "katl.generation.spec",
		RecordVersion: 1,
		ValidatePath: func(path string, envelope Envelope) error {
			payload, err := DecodePayload[generationSpecPayload](envelope)
			if err != nil {
				return err
			}
			if filepath.Base(filepath.Dir(path)) != payload.GenerationID {
				return errors.New("generation id does not match path")
			}
			return nil
		},
	})
	envelope := readFixture(t, "path-type-mismatch.json")

	err := registry.ValidatePath("/var/lib/katl/generations/gen-0/spec.json", envelope)
	if err == nil || !errors.Is(err, ErrInvalidEnvelope) || !strings.Contains(err.Error(), "generation id does not match path") {
		t.Fatalf("ValidatePath() error = %v, want path mismatch", err)
	}
}

func TestMarshalCanonicalAndAtomicWrites(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "var/lib/katl/generations/gen-0/spec.json")
	envelope := readFixture(t, filepath.Join("v1", "katl.generation.spec.json"))

	data, err := MarshalEnvelope(envelope)
	if err != nil {
		t.Fatalf("MarshalEnvelope() error = %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") || !strings.Contains(string(data), `"recordType": "katl.generation.spec"`) {
		t.Fatalf("canonical envelope JSON = %q", data)
	}

	if err := WriteEnvelopeAtomic(path, envelope, 0o600); err != nil {
		t.Fatalf("WriteEnvelopeAtomic() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat atomic record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	read, err := ReadEnvelope(path)
	if err != nil {
		t.Fatalf("ReadEnvelope() error = %v", err)
	}
	if read.RecordType != envelope.RecordType {
		t.Fatalf("read record type = %q", read.RecordType)
	}
}

func TestWriteEnvelopeNoReplaceRefusesExistingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	envelope := readFixture(t, filepath.Join("v1", "katl.generation.spec.json"))
	if err := WriteEnvelopeNoReplace(path, envelope, 0o644, 0o755); err != nil {
		t.Fatalf("WriteEnvelopeNoReplace() first error = %v", err)
	}
	err := WriteEnvelopeNoReplace(path, envelope, 0o644, 0o755)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("WriteEnvelopeNoReplace() second error = %v, want already exists", err)
	}
}

func readFixture(t *testing.T, name string) Envelope {
	t.Helper()
	envelope, err := DecodeEnvelope(readFixtureBytes(t, name))
	if err != nil {
		t.Fatalf("DecodeEnvelope(%s) error = %v", name, err)
	}
	return envelope
}

func readFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func mustRegistry(t *testing.T, handlers ...Handler) Registry {
	t.Helper()
	registry, err := NewRegistry(handlers...)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	return registry
}
