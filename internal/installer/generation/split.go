package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	SpecKind   = "GenerationSpec"
	StatusKind = "GenerationStatus"

	CommitStateCandidate  = "candidate"
	CommitStateCommitted  = "committed"
	CommitStateSuperseded = "superseded"
	CommitStateAbandoned  = "abandoned"

	BootStatePending = "pending"
	BootStateTrying  = "trying"
	BootStateGood    = "good"
	BootStateFailed  = "failed"

	HealthStateUnknown   = "unknown"
	HealthStateHealthy   = "healthy"
	HealthStateUnhealthy = "unhealthy"
	HealthStateDeferred  = "deferred"
)

type GenerationSpec struct {
	APIVersion           string             `json:"apiVersion"`
	Kind                 string             `json:"kind"`
	GenerationID         string             `json:"generationID"`
	RuntimeVersion       string             `json:"runtimeVersion"`
	PreviousGenerationID string             `json:"previousGenerationID,omitempty"`
	Root                 RootSelection      `json:"root"`
	Boot                 BootSelection      `json:"boot"`
	Sysexts              []ExtensionRef     `json:"sysexts"`
	Confexts             []GeneratedConfext `json:"confexts"`
	KernelCommandLine    []string           `json:"kernelCommandLine"`
	KubernetesUpgrade    *KubernetesUpgrade `json:"kubernetesUpgrade,omitempty"`
	CreatedAt            time.Time          `json:"createdAt"`
}

type GenerationStatus struct {
	APIVersion           string             `json:"apiVersion"`
	Kind                 string             `json:"kind"`
	GenerationID         string             `json:"generationID"`
	SpecDigest           string             `json:"specDigest"`
	CommitState          string             `json:"commitState"`
	BootState            string             `json:"bootState"`
	HealthState          string             `json:"healthState"`
	UpdatedAt            time.Time          `json:"updatedAt"`
	StatusTransitions    []StatusTransition `json:"statusTransitions,omitempty"`
	CommittedAt          *time.Time         `json:"committedAt,omitempty"`
	CommittedByOperation string             `json:"committedByOperationID,omitempty"`
}

type StatusTransition struct {
	At          time.Time `json:"at"`
	OperationID string    `json:"operationID,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	CommitState string    `json:"commitState,omitempty"`
	BootState   string    `json:"bootState,omitempty"`
	HealthState string    `json:"healthState,omitempty"`
}

func SpecFromRecord(record Record) GenerationSpec {
	previous := ""
	if record.ConfigApply != nil {
		previous = strings.TrimSpace(record.ConfigApply.PreviousGeneration)
	}
	if previous == "" {
		previous = strings.TrimSpace(record.PreviousGenerationID)
	}
	return GenerationSpec{
		APIVersion:           APIVersion,
		Kind:                 SpecKind,
		GenerationID:         strings.TrimSpace(record.GenerationID),
		RuntimeVersion:       strings.TrimSpace(record.RuntimeVersion),
		PreviousGenerationID: previous,
		Root:                 record.Root,
		Boot:                 record.Boot,
		Sysexts:              append([]ExtensionRef{}, record.Sysexts...),
		Confexts:             append([]GeneratedConfext{}, record.Confexts...),
		KernelCommandLine:    append([]string{}, record.KernelCommandLine...),
		KubernetesUpgrade:    record.KubernetesUpgrade,
		CreatedAt:            record.CreatedAt.UTC(),
	}
}

func StatusFromRecord(record Record, specDigest string) GenerationStatus {
	commitState := CommitStateCommitted
	if record.ConfigApply != nil {
		commitState = CommitStateCandidate
	}
	updatedAt := record.CreatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	bootState := strings.TrimSpace(record.BootState)
	if bootState == "" {
		bootState = BootStatePending
	}
	healthState := strings.TrimSpace(record.HealthState)
	if healthState == "" {
		healthState = HealthStateUnknown
	}
	return GenerationStatus{
		APIVersion:   APIVersion,
		Kind:         StatusKind,
		GenerationID: strings.TrimSpace(record.GenerationID),
		SpecDigest:   specDigest,
		CommitState:  commitState,
		BootState:    bootState,
		HealthState:  healthState,
		UpdatedAt:    updatedAt.UTC(),
	}
}

func RecordFromSplit(spec GenerationSpec, status GenerationStatus) Record {
	return Record{
		APIVersion:           APIVersion,
		Kind:                 Kind,
		GenerationID:         spec.GenerationID,
		RuntimeVersion:       spec.RuntimeVersion,
		PreviousGenerationID: spec.PreviousGenerationID,
		Root:                 spec.Root,
		Boot:                 spec.Boot,
		Sysexts:              append([]ExtensionRef(nil), spec.Sysexts...),
		Confexts:             append([]GeneratedConfext(nil), spec.Confexts...),
		KernelCommandLine:    append([]string(nil), spec.KernelCommandLine...),
		KubernetesUpgrade:    spec.KubernetesUpgrade,
		CreatedAt:            spec.CreatedAt,
		BootState:            status.BootState,
		HealthState:          status.HealthState,
	}
}

func NewGenerationStatus(spec GenerationSpec, commitState string, bootState string, healthState string, updatedAt time.Time) (GenerationStatus, error) {
	digest, err := CanonicalSpecDigest(spec)
	if err != nil {
		return GenerationStatus{}, err
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	status := GenerationStatus{
		APIVersion:   APIVersion,
		Kind:         StatusKind,
		GenerationID: spec.GenerationID,
		SpecDigest:   digest,
		CommitState:  strings.TrimSpace(commitState),
		BootState:    strings.TrimSpace(bootState),
		HealthState:  strings.TrimSpace(healthState),
		UpdatedAt:    updatedAt.UTC(),
	}
	if err := ValidateGenerationStatus(spec, status); err != nil {
		return GenerationStatus{}, err
	}
	return status, nil
}

func GenerationDir(root string, generationID string) (string, error) {
	generationID, err := cleanSegment("generation id", generationID)
	if err != nil {
		return "", err
	}
	return rootedPath(root, filepath.Join(GenerationRecordsDir, generationID))
}

func ReadGeneration(root string, generationID string) (GenerationSpec, GenerationStatus, error) {
	dir, err := GenerationDir(root, generationID)
	if err != nil {
		return GenerationSpec{}, GenerationStatus{}, err
	}
	return ReadSplitRecords(dir)
}

func WriteGeneration(root string, spec GenerationSpec, status GenerationStatus) error {
	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		return err
	}
	return WriteSplitRecords(dir, spec, status)
}

func ReadSplitRecords(dir string) (GenerationSpec, GenerationStatus, error) {
	specPath := filepath.Join(dir, "spec.json")
	statusPath := filepath.Join(dir, "status.json")
	spec, err := readGenerationSpecFile(specPath)
	if err != nil {
		if os.IsNotExist(err) {
			legacy, legacyErr := readRecordFile(filepath.Join(dir, "metadata.json"))
			if legacyErr != nil {
				return GenerationSpec{}, GenerationStatus{}, fmt.Errorf("read generation spec: %w", err)
			}
			spec := SpecFromRecord(legacy)
			digest, digestErr := CanonicalSpecDigest(spec)
			if digestErr != nil {
				return GenerationSpec{}, GenerationStatus{}, digestErr
			}
			status := StatusFromRecord(legacy, digest)
			if validateErr := ValidateGenerationStatus(spec, status); validateErr != nil {
				return GenerationSpec{}, GenerationStatus{}, validateErr
			}
			return spec, status, nil
		}
		return GenerationSpec{}, GenerationStatus{}, fmt.Errorf("read generation spec: %w", err)
	}
	if err := validateGenerationDirID(dir, spec.GenerationID); err != nil {
		return GenerationSpec{}, GenerationStatus{}, err
	}
	status, err := readGenerationStatusFile(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			legacy, legacyErr := readRecordFile(filepath.Join(dir, "metadata.json"))
			if legacyErr == nil {
				digest, digestErr := CanonicalSpecDigest(spec)
				if digestErr != nil {
					return GenerationSpec{}, GenerationStatus{}, digestErr
				}
				status := StatusFromRecord(legacy, digest)
				if validateErr := ValidateGenerationStatus(spec, status); validateErr != nil {
					return GenerationSpec{}, GenerationStatus{}, validateErr
				}
				return spec, status, nil
			}
		}
		return GenerationSpec{}, GenerationStatus{}, fmt.Errorf("read generation status: %w", err)
	}
	if err := ValidateGenerationStatus(spec, status); err != nil {
		return GenerationSpec{}, GenerationStatus{}, err
	}
	return spec, status, nil
}

func WriteSplitRecords(dir string, spec GenerationSpec, status GenerationStatus) error {
	if err := ValidateGenerationSpec(spec); err != nil {
		return err
	}
	digest, err := CanonicalSpecDigest(spec)
	if err != nil {
		return err
	}
	if strings.TrimSpace(status.SpecDigest) == "" {
		status.SpecDigest = digest
	}
	if err := ValidateGenerationStatus(spec, status); err != nil {
		return err
	}
	specPath := filepath.Join(dir, "spec.json")
	if _, err := os.Stat(specPath); err == nil {
		return fmt.Errorf("generation spec already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing generation spec: %w", err)
	}
	specData, err := marshalRecordEnvelope(GenerationSpecRecordType, spec)
	if err != nil {
		return fmt.Errorf("marshal generation spec: %w", err)
	}
	statusData, err := marshalRecordEnvelope(GenerationStatusRecordType, status)
	if err != nil {
		return fmt.Errorf("marshal generation status: %w", err)
	}
	if err := writeFileAtomic(specPath, specData, 0o644); err != nil {
		return fmt.Errorf("write generation spec: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "status.json"), statusData, 0o644); err != nil {
		return fmt.Errorf("write generation status: %w", err)
	}
	return nil
}

func WriteGenerationStatus(root string, spec GenerationSpec, status GenerationStatus) error {
	dir, err := GenerationDir(root, spec.GenerationID)
	if err != nil {
		return err
	}
	return WriteStatusRecord(dir, spec, status)
}

func WriteStatusRecord(dir string, spec GenerationSpec, status GenerationStatus) error {
	if err := ValidateGenerationStatus(spec, status); err != nil {
		return err
	}
	existingSpec, existingStatus, err := ReadSplitRecords(dir)
	if err != nil {
		return err
	}
	existingDigest, err := CanonicalSpecDigest(existingSpec)
	if err != nil {
		return err
	}
	newDigest, err := CanonicalSpecDigest(spec)
	if err != nil {
		return err
	}
	if existingDigest != newDigest {
		return fmt.Errorf("generation spec selection fields are immutable")
	}
	if err := ValidateStatusTransition(existingStatus, status); err != nil {
		return err
	}
	statusData, err := marshalRecordEnvelope(GenerationStatusRecordType, status)
	if err != nil {
		return fmt.Errorf("marshal generation status: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "status.json"), statusData, 0o644); err != nil {
		return fmt.Errorf("write generation status: %w", err)
	}
	return nil
}

func MarshalCanonicalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func readGenerationSpecFile(path string) (GenerationSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GenerationSpec{}, err
	}
	if spec, ok, err := decodeRecordEnvelope[GenerationSpec](data, GenerationSpecRecordType); ok {
		if err != nil {
			return GenerationSpec{}, fmt.Errorf("decode generation spec envelope: %w", err)
		}
		if err := ValidateGenerationSpec(spec); err != nil {
			return GenerationSpec{}, err
		}
		return spec, nil
	}
	var spec GenerationSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return GenerationSpec{}, fmt.Errorf("decode generation spec: %w", err)
	}
	if err := ValidateGenerationSpec(spec); err != nil {
		return GenerationSpec{}, err
	}
	return spec, nil
}

func readGenerationStatusFile(path string) (GenerationStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GenerationStatus{}, err
	}
	if status, ok, err := decodeRecordEnvelope[GenerationStatus](data, GenerationStatusRecordType); ok {
		if err != nil {
			return GenerationStatus{}, fmt.Errorf("decode generation status envelope: %w", err)
		}
		return status, nil
	}
	var status GenerationStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return GenerationStatus{}, fmt.Errorf("decode generation status: %w", err)
	}
	return status, nil
}

func validateGenerationDirID(dir string, generationID string) error {
	if filepath.Base(filepath.Clean(dir)) != generationID {
		return fmt.Errorf("generation record path id %q does not match payload id %q", filepath.Base(filepath.Clean(dir)), generationID)
	}
	return nil
}

func CanonicalSpecDigest(spec GenerationSpec) (string, error) {
	if err := ValidateGenerationSpec(spec); err != nil {
		return "", err
	}
	data, err := MarshalCanonicalJSON(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func ValidateGenerationSpec(spec GenerationSpec) error {
	if spec.APIVersion != APIVersion {
		return fmt.Errorf("generation spec apiVersion must be %q", APIVersion)
	}
	if spec.Kind != SpecKind {
		return fmt.Errorf("generation spec kind must be %q", SpecKind)
	}
	if _, err := cleanSegment("generation id", spec.GenerationID); err != nil {
		return err
	}
	if strings.TrimSpace(spec.RuntimeVersion) == "" {
		return fmt.Errorf("runtime version is required")
	}
	if strings.TrimSpace(spec.Root.Slot) == "" {
		return fmt.Errorf("root slot is required")
	}
	if strings.TrimSpace(spec.Root.PartitionUUID) == "" {
		return fmt.Errorf("root partition UUID is required")
	}
	if strings.TrimSpace(spec.Root.RuntimeInterface) == "" {
		return fmt.Errorf("runtime interface is required")
	}
	if strings.TrimSpace(spec.Root.Architecture) == "" {
		return fmt.Errorf("runtime architecture is required")
	}
	if err := validateSHA256("runtime artifact", spec.Root.RuntimeArtifactSHA256); err != nil {
		return err
	}
	if strings.TrimSpace(spec.Boot.UKIPath) == "" {
		return fmt.Errorf("UKI path is required")
	}
	for _, sysext := range spec.Sysexts {
		if err := ValidatePair(spec.Root, sysext); err != nil {
			return err
		}
	}
	for _, confext := range spec.Confexts {
		if _, err := normalizeGeneratedConfext(confext); err != nil {
			return err
		}
	}
	if spec.KubernetesUpgrade != nil {
		if _, err := cleanSegment("Kubernetes upgrade operation id", spec.KubernetesUpgrade.OperationID); err != nil {
			return err
		}
		if strings.TrimSpace(spec.KubernetesUpgrade.TargetKubeadmAccessMode) == "" || strings.TrimSpace(spec.KubernetesUpgrade.KubeletActivationGate) == "" {
			return fmt.Errorf("Kubernetes upgrade access mode and kubelet activation gate are required")
		}
	}
	if spec.CreatedAt.IsZero() {
		return fmt.Errorf("generation spec createdAt is required")
	}
	return nil
}

func ValidateGenerationStatus(spec GenerationSpec, status GenerationStatus) error {
	if err := ValidateGenerationSpec(spec); err != nil {
		return err
	}
	if status.APIVersion != APIVersion {
		return fmt.Errorf("generation status apiVersion must be %q", APIVersion)
	}
	if status.Kind != StatusKind {
		return fmt.Errorf("generation status kind must be %q", StatusKind)
	}
	if status.GenerationID != spec.GenerationID {
		return fmt.Errorf("generation status id %q does not match spec id %q", status.GenerationID, spec.GenerationID)
	}
	wantDigest, err := CanonicalSpecDigest(spec)
	if err != nil {
		return err
	}
	if status.SpecDigest != wantDigest {
		return fmt.Errorf("generation status specDigest mismatch: got %q, want %q", status.SpecDigest, wantDigest)
	}
	if !validCommitState(status.CommitState) {
		return fmt.Errorf("unsupported generation commitState %q", status.CommitState)
	}
	if !validBootState(status.BootState) {
		return fmt.Errorf("unsupported generation bootState %q", status.BootState)
	}
	if !validHealthState(status.HealthState) {
		return fmt.Errorf("unsupported generation healthState %q", status.HealthState)
	}
	if status.UpdatedAt.IsZero() {
		return fmt.Errorf("generation status updatedAt is required")
	}
	return nil
}

func ValidateStatusTransition(previous GenerationStatus, next GenerationStatus) error {
	if previous.GenerationID != next.GenerationID {
		return fmt.Errorf("generation status transition cannot change generation id")
	}
	if previous.SpecDigest != next.SpecDigest {
		return fmt.Errorf("generation status transition cannot change specDigest")
	}
	if previous.CommitState != next.CommitState {
		if err := ValidateCommitTransition(previous.CommitState, next.CommitState); err != nil {
			return err
		}
	}
	if previous.BootState != next.BootState {
		if err := ValidateBootTransition(previous.BootState, next.BootState); err != nil {
			return err
		}
	}
	return nil
}

func ValidateCommitTransition(from string, to string) error {
	switch from + "->" + to {
	case CommitStateCandidate + "->" + CommitStateCommitted,
		CommitStateCandidate + "->" + CommitStateAbandoned,
		CommitStateCommitted + "->" + CommitStateSuperseded,
		CommitStateSuperseded + "->" + CommitStateCommitted:
		return nil
	default:
		return fmt.Errorf("invalid commitState transition %s -> %s", from, to)
	}
}

func ValidateBootTransition(from string, to string) error {
	switch from + "->" + to {
	case BootStatePending + "->" + BootStateTrying,
		BootStatePending + "->" + BootStateGood,
		BootStateTrying + "->" + BootStateGood,
		BootStatePending + "->" + BootStateFailed,
		BootStateTrying + "->" + BootStateFailed:
		return nil
	default:
		return fmt.Errorf("invalid bootState transition %s -> %s", from, to)
	}
}

func IsKnownGood(status GenerationStatus) bool {
	return (status.CommitState == CommitStateCommitted || status.CommitState == CommitStateSuperseded) &&
		status.BootState == BootStateGood &&
		status.HealthState == HealthStateHealthy
}

func validCommitState(state string) bool {
	switch state {
	case CommitStateCandidate, CommitStateCommitted, CommitStateSuperseded, CommitStateAbandoned:
		return true
	default:
		return false
	}
}

func validBootState(state string) bool {
	switch state {
	case BootStatePending, BootStateTrying, BootStateGood, BootStateFailed:
		return true
	default:
		return false
	}
}

func validHealthState(state string) bool {
	switch state {
	case HealthStateUnknown, HealthStateHealthy, HealthStateUnhealthy, HealthStateDeferred:
		return true
	default:
		return false
	}
}
