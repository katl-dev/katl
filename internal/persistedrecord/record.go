package persistedrecord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrInvalidEnvelope   = errors.New("invalid persisted record envelope")
	ErrUnsupportedRecord = errors.New("unsupported persisted record")
)

type Envelope struct {
	RecordType    string          `json:"recordType"`
	RecordVersion int             `json:"recordVersion"`
	WrittenBy     *Writer         `json:"writtenBy,omitempty"`
	WrittenAt     *time.Time      `json:"writtenAt,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

type Writer struct {
	KatlVersion      string `json:"katlVersion,omitempty"`
	RuntimeInterface string `json:"runtimeInterface,omitempty"`
}

type Key struct {
	RecordType    string
	RecordVersion int
}

type PathValidator func(path string, envelope Envelope) error

type Handler struct {
	RecordType    string
	RecordVersion int
	ValidatePath  PathValidator
}

type Registry struct {
	handlers map[Key]Handler
}

func DecodeEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := strictDecode(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("%w: decode envelope: %v", ErrInvalidEnvelope, err)
	}
	if err := ValidateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func ReadEnvelope(path string) (Envelope, error) {
	if strings.TrimSpace(path) == "" {
		return Envelope{}, fmt.Errorf("%w: path is required", ErrInvalidEnvelope)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Envelope{}, fmt.Errorf("read persisted record envelope: %w", err)
	}
	return DecodeEnvelope(data)
}

func ValidateEnvelope(envelope Envelope) error {
	if strings.TrimSpace(envelope.RecordType) == "" {
		return fmt.Errorf("%w: recordType is required", ErrInvalidEnvelope)
	}
	if envelope.RecordType != strings.TrimSpace(envelope.RecordType) {
		return fmt.Errorf("%w: recordType must not contain leading or trailing whitespace", ErrInvalidEnvelope)
	}
	if envelope.RecordVersion <= 0 {
		return fmt.Errorf("%w: recordVersion must be positive", ErrInvalidEnvelope)
	}
	if len(envelope.Payload) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Payload), []byte("null")) {
		return fmt.Errorf("%w: payload is required", ErrInvalidEnvelope)
	}
	if !json.Valid(envelope.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidEnvelope)
	}
	return nil
}

func DecodePayload[T any](envelope Envelope) (T, error) {
	var payload T
	if err := ValidateEnvelope(envelope); err != nil {
		return payload, err
	}
	if err := strictDecode(envelope.Payload, &payload); err != nil {
		return payload, fmt.Errorf("%w: decode %s/v%d payload: %v", ErrInvalidEnvelope, envelope.RecordType, envelope.RecordVersion, err)
	}
	return payload, nil
}

func NewRegistry(handlers ...Handler) (Registry, error) {
	registry := Registry{handlers: make(map[Key]Handler, len(handlers))}
	for _, handler := range handlers {
		if strings.TrimSpace(handler.RecordType) == "" {
			return Registry{}, fmt.Errorf("%w: handler recordType is required", ErrInvalidEnvelope)
		}
		if handler.RecordType != strings.TrimSpace(handler.RecordType) {
			return Registry{}, fmt.Errorf("%w: handler recordType must not contain leading or trailing whitespace", ErrInvalidEnvelope)
		}
		if handler.RecordVersion <= 0 {
			return Registry{}, fmt.Errorf("%w: handler recordVersion must be positive", ErrInvalidEnvelope)
		}
		key := Key{RecordType: handler.RecordType, RecordVersion: handler.RecordVersion}
		if _, ok := registry.handlers[key]; ok {
			return Registry{}, fmt.Errorf("%w: duplicate handler for %s/v%d", ErrInvalidEnvelope, key.RecordType, key.RecordVersion)
		}
		registry.handlers[key] = handler
	}
	return registry, nil
}

func (r Registry) Dispatch(envelope Envelope) (Handler, error) {
	if err := ValidateEnvelope(envelope); err != nil {
		return Handler{}, err
	}
	key := Key{RecordType: envelope.RecordType, RecordVersion: envelope.RecordVersion}
	handler, ok := r.handlers[key]
	if !ok {
		return Handler{}, fmt.Errorf("%w: %s/v%d", ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion)
	}
	return handler, nil
}

func (r Registry) ValidatePath(path string, envelope Envelope) error {
	handler, err := r.Dispatch(envelope)
	if err != nil {
		return err
	}
	if handler.ValidatePath == nil {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidEnvelope)
	}
	if err := handler.ValidatePath(path, envelope); err != nil {
		return fmt.Errorf("%w: %s/v%d path %q: %v", ErrInvalidEnvelope, envelope.RecordType, envelope.RecordVersion, path, err)
	}
	return nil
}

func (r Registry) Read(path string) (Envelope, Handler, error) {
	envelope, err := ReadEnvelope(path)
	if err != nil {
		return Envelope{}, Handler{}, err
	}
	handler, err := r.Dispatch(envelope)
	if err != nil {
		return Envelope{}, Handler{}, err
	}
	if handler.ValidatePath != nil {
		if err := r.ValidatePath(path, envelope); err != nil {
			return Envelope{}, Handler{}, err
		}
	}
	return envelope, handler, nil
}

func MarshalCanonical(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func MarshalEnvelope(envelope Envelope) ([]byte, error) {
	if err := ValidateEnvelope(envelope); err != nil {
		return nil, err
	}
	data, err := MarshalCanonical(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal persisted record envelope: %w", err)
	}
	return data, nil
}

func WriteEnvelopeAtomic(path string, envelope Envelope, mode os.FileMode) error {
	data, err := MarshalEnvelope(envelope)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, mode)
}

func WriteEnvelopeNoReplace(path string, envelope Envelope, mode os.FileMode, dirMode os.FileMode) error {
	data, err := MarshalEnvelope(envelope)
	if err != nil {
		return err
	}
	return WriteFileNoReplace(path, data, mode, dirMode)
}

func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted record path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create persisted record directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create persisted record temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write persisted record temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod persisted record temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync persisted record temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close persisted record temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace persisted record: %w", err)
	}
	cleanup = false
	return syncDir(dir)
}

func WriteFileNoReplace(path string, data []byte, mode os.FileMode, dirMode os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted record path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create persisted record directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("persisted record already exists: %s", path)
		}
		return fmt.Errorf("create persisted record: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write persisted record: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync persisted record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close persisted record: %w", err)
	}
	return syncDir(dir)
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open persisted record directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync persisted record directory: %w", err)
	}
	return nil
}
