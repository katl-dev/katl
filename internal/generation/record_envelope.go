package generation

import (
	"encoding/json"
	"fmt"

	"github.com/katl-dev/katl/internal/persistedrecord"
)

const (
	GenerationSpecRecordType    = "katl.generation.spec"
	GenerationStatusRecordType  = "katl.generation.status"
	ConfigApplyStatusRecordType = "katl.generation.config-apply-status"
	BootSelectionRecordType     = "katl.boot.selection"
	persistedRecordVersion      = 1
)

func marshalRecordEnvelope(recordType string, payload any) ([]byte, error) {
	payloadData, err := MarshalCanonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	return persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    recordType,
		RecordVersion: persistedRecordVersion,
		Payload:       payloadData,
	})
}

func decodeRecordEnvelope[T any](data []byte, recordType string) (T, bool, error) {
	var zero T
	if !looksLikeRecordEnvelope(data) {
		return zero, false, nil
	}
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err != nil {
		return zero, true, err
	}
	if envelope.RecordType != recordType {
		return zero, true, fmt.Errorf("%w: got %s/v%d, want %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion, recordType, persistedRecordVersion)
	}
	if envelope.RecordVersion != persistedRecordVersion {
		return zero, true, fmt.Errorf("%w: %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion)
	}
	payload, err := persistedrecord.DecodePayload[T](envelope)
	if err != nil {
		return zero, true, err
	}
	return payload, true, nil
}

func looksLikeRecordEnvelope(data []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}
	if _, ok := fields["recordType"]; ok {
		return true
	}
	if _, ok := fields["recordVersion"]; ok {
		return true
	}
	if _, ok := fields["payload"]; ok {
		return true
	}
	return false
}
