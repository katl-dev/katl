package operation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zariel/katl/internal/installer/persistedrecord"
)

const (
	APIVersion = "katl.dev/v1alpha1"

	RecordKind = "OperationRecord"
	EventKind  = "OperationJournalEvent"

	SchemaVersion = 1

	recordEnvelopeVersion = 1
	RecordTypeOperation   = "katl.operation.record"
	RecordTypeJournal     = "katl.operation.journal-event"

	ResultTimedOut          = "timed-out"
	ResultFailedNeedsRepair = "failed-needs-repair"
	ResultSucceeded         = "succeeded"

	ActivationModeLive        = "live"
	ActivationModeNextBoot    = "next-boot"
	ActivationStatePending    = "pending"
	ActivationStateActivating = "activating"
	ActivationStateActiveLive = "active-live"
	ActivationStateFailed     = "failed"
	ActivationStateRolledBack = "rolled-back"
	GenerationCommitCandidate = "candidate"
	GenerationCommitCommitted = "committed"
	GenerationCommitAbandoned = "abandoned"
	PostKubeadmHealthNotRun   = "not-run"
	PostKubeadmHealthRunning  = "running"
	PostKubeadmHealthPassed   = "passed"
	PostKubeadmHealthFailed   = "failed"

	ResumeHostBookkeeping          = "finish-host-bookkeeping:record-operation-complete"
	HostBookkeepingCompletionPhase = "record-operation-complete"
	HostBookkeepingOperationKind   = "host-bookkeeping"
	HostBookkeepingGenerationScope = "host-generation"

	StaleTerminal     = "terminal"
	StalePreMutation  = "stale-pre-mutation"
	StaleHostOnly     = "stale-host-only"
	StalePostMutation = "stale-post-mutation"
	StaleAmbiguous    = "stale-ambiguous"
	StaleNotStale     = "not-stale"
)

type Store struct {
	Root string
}

type OperationRecord struct {
	APIVersion                  string                  `json:"apiVersion"`
	Kind                        string                  `json:"kind"`
	SchemaVersion               int                     `json:"schemaVersion"`
	OperationID                 string                  `json:"operationID"`
	OperationKind               string                  `json:"operationKind"`
	Scope                       string                  `json:"scope"`
	ParentOperationID           string                  `json:"parentOperationID,omitempty"`
	ClientRequestID             string                  `json:"clientRequestID,omitempty"`
	Actor                       string                  `json:"actor,omitempty"`
	ExpectedMachineID           string                  `json:"expectedMachineID,omitempty"`
	ExpectedCurrentGenerationID string                  `json:"expectedCurrentGenerationID,omitempty"`
	ExpectedClusterIntentDigest string                  `json:"expectedClusterIntentDigest,omitempty"`
	RequestDigest               string                  `json:"requestDigest"`
	RecordRevision              int                     `json:"recordRevision"`
	LatestJournalSeq            int                     `json:"latestJournalSeq"`
	PhasePlan                   []string                `json:"phasePlan,omitempty"`
	PreviousGenerationID        string                  `json:"previousGenerationID,omitempty"`
	CandidateGenerationID       string                  `json:"candidateGenerationID,omitempty"`
	ConfigApplyPhase            string                  `json:"configApplyPhase,omitempty"`
	ChangedDomains              []string                `json:"changedDomains,omitempty"`
	BootstrapRequest            *BootstrapRequest       `json:"bootstrapRequest,omitempty"`
	ConfigApplyRequest          *ConfigApplyRequest     `json:"configApplyRequest,omitempty"`
	KubernetesSysextUpdate      *KubernetesSysextUpdate `json:"kubernetesSysextUpdate,omitempty"`
	DestructiveResetRequest     *DestructiveReset       `json:"destructiveResetRequest,omitempty"`
	HostUpgradeRequest          *HostUpgrade            `json:"hostUpgradeRequest,omitempty"`
	ActivationMode              string                  `json:"activationMode,omitempty"`
	ActivationState             string                  `json:"activationState,omitempty"`
	GenerationCommitState       string                  `json:"generationCommitState,omitempty"`
	PostKubeadmHealthState      string                  `json:"postKubeadmHealthState,omitempty"`
	BootHealthPending           bool                    `json:"bootHealthPending,omitempty"`
	Phase                       string                  `json:"phase,omitempty"`
	PhaseIndex                  int                     `json:"phaseIndex"`
	CompletedPhases             []string                `json:"completedPhases,omitempty"`
	Terminal                    bool                    `json:"terminal"`
	ResourceLocks               []string                `json:"resourceLocks,omitempty"`
	ExecutorPlan                *ExecutorPlan           `json:"executorPlan,omitempty"`
	Invocations                 []InvocationRecord      `json:"invocations,omitempty"`
	ExternalMutationStarted     bool                    `json:"externalMutationStarted"`
	PreExecMutationMarkers      []PreExecMutationMarker `json:"preExecMutationMarkers,omitempty"`
	MutationScopes              []string                `json:"mutationScopes,omitempty"`
	MutatingToolRan             bool                    `json:"mutatingToolRan"`
	MutatingToolInvocations     []string                `json:"mutatingToolInvocations,omitempty"`
	DiagnosticArtifacts         []DiagnosticArtifact    `json:"diagnosticArtifacts,omitempty"`
	HostRollback                string                  `json:"hostRollback,omitempty"`
	PostMutationRollbackAllowed bool                    `json:"postMutationRollbackAllowed"`
	RecoveryRequired            bool                    `json:"recoveryRequired"`
	RetryHint                   string                  `json:"retryHint,omitempty"`
	Interruption                string                  `json:"interruption,omitempty"`
	Resume                      string                  `json:"resume,omitempty"`
	NextAction                  string                  `json:"nextAction,omitempty"`
	Result                      string                  `json:"result,omitempty"`
	CreatedAt                   time.Time               `json:"createdAt"`
	UpdatedAt                   time.Time               `json:"updatedAt"`
	CompletedAt                 *time.Time              `json:"completedAt,omitempty"`
	FailureReason               string                  `json:"failureReason,omitempty"`
}

type ExecutorPlan struct {
	Phase          string   `json:"phase"`
	MarkerID       string   `json:"markerID,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	MutationScopes []string `json:"mutationScopes,omitempty"`
	Argv           []string `json:"argv"`
}

type BootstrapRequest struct {
	InventoryNodeName              string `json:"inventoryNodeName"`
	SystemRole                     string `json:"systemRole"`
	KubernetesPayloadVersion       string `json:"kubernetesPayloadVersion"`
	KubernetesBundleSource         string `json:"kubernetesBundleSource,omitempty"`
	KubernetesBundleRef            string `json:"kubernetesBundleRef,omitempty"`
	KubernetesBundleManifestDigest string `json:"kubernetesBundleManifestDigest,omitempty"`
	KubernetesSysextPayloadDigest  string `json:"kubernetesSysextPayloadDigest,omitempty"`
	BootstrapProfileRef            string `json:"bootstrapProfileRef"`
	ControlPlaneEndpoint           string `json:"controlPlaneEndpoint,omitempty"`
	StableEndpoint                 string `json:"stableEndpoint,omitempty"`
	CandidateGenerationID          string `json:"candidateGenerationID,omitempty"`
	KubeadmInputDigest             string `json:"kubeadmInputDigest,omitempty"`
	JoinMaterialRef                string `json:"joinMaterialRef,omitempty"`
	JoinMaterialDigest             string `json:"joinMaterialDigest,omitempty"`
	JoinMaterialExpiresAt          string `json:"joinMaterialExpiresAt,omitempty"`
	TemporaryJoinConfigPath        string `json:"temporaryJoinConfigPath,omitempty"`
}

type ConfigApplyRequest struct {
	ApplyMode             string `json:"applyMode"`
	NodeName              string `json:"nodeName,omitempty"`
	CandidateGenerationID string `json:"candidateGenerationID,omitempty"`
	ConfigYAML            string `json:"configYAML"`
}

type KubernetesSysextUpdate struct {
	TargetPayloadVersion string `json:"targetPayloadVersion"`
	TargetSysextPath     string `json:"targetSysextPath"`
	TargetSysextSHA256   string `json:"targetSysextSHA256"`
	TargetSysextSize     uint64 `json:"targetSysextSizeBytes,omitempty"`
	TargetActivationPath string `json:"targetActivationPath,omitempty"`
}

type DestructiveReset struct {
	InventoryNodeName      string   `json:"inventoryNodeName"`
	ResetScope             string   `json:"resetScope"`
	TargetGenerationID     string   `json:"targetGenerationID,omitempty"`
	DiscardClusterIdentity bool     `json:"discardClusterIdentity"`
	WipeSurfaces           []string `json:"wipeSurfaces,omitempty"`
}

type HostUpgrade struct {
	ImageURL              string `json:"imageURL,omitempty"`
	ImageLocalRef         string `json:"imageLocalRef,omitempty"`
	ImageSHA256           string `json:"imageSHA256"`
	ImageSizeBytes        uint64 `json:"imageSizeBytes,omitempty"`
	CandidateGenerationID string `json:"candidateGenerationID"`
}

type InvocationRecord struct {
	InvocationID        string     `json:"invocationID"`
	SystemdInvocationID string     `json:"systemdInvocationID,omitempty"`
	UnitName            string     `json:"unitName,omitempty"`
	BootID              string     `json:"bootID,omitempty"`
	AgentStartID        string     `json:"agentStartID,omitempty"`
	ExecutorAttemptID   string     `json:"executorAttemptID,omitempty"`
	ChildProcess        []string   `json:"childProcess,omitempty"`
	PID                 int        `json:"pid,omitempty"`
	ExitStatus          int        `json:"exitStatus,omitempty"`
	StartedAt           time.Time  `json:"startedAt"`
	CompletedAt         *time.Time `json:"completedAt,omitempty"`
	Result              string     `json:"result,omitempty"`
}

type PreExecMutationMarker struct {
	MarkerID               string    `json:"markerID"`
	InvocationID           string    `json:"invocationID,omitempty"`
	Phase                  string    `json:"phase"`
	Tool                   string    `json:"tool"`
	ArgvDigest             string    `json:"argvDigest"`
	ExpectedMutationScopes []string  `json:"expectedMutationScopes,omitempty"`
	MarkedAt               time.Time `json:"markedAt"`
}

type DiagnosticArtifact struct {
	ArtifactID string    `json:"artifactID"`
	Path       string    `json:"path"`
	SHA256     string    `json:"sha256"`
	Redacted   bool      `json:"redacted"`
	CreatedAt  time.Time `json:"createdAt"`
}

type JournalEvent struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Sequence   int             `json:"sequence"`
	EventID    string          `json:"eventID"`
	EventType  string          `json:"eventType"`
	Record     OperationRecord `json:"record"`
	CreatedAt  time.Time       `json:"createdAt"`
}

type Snapshot struct {
	APIVersion    string          `json:"apiVersion"`
	Kind          string          `json:"kind"`
	OperationID   string          `json:"operationID"`
	LatestSeq     int             `json:"latestJournalSeq"`
	JournalDigest string          `json:"journalDigest"`
	Record        OperationRecord `json:"record"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

type ReconcileReport struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Boot       bool                  `json:"boot"`
	Operations []ReconciledOperation `json:"operations,omitempty"`
	UpdatedAt  time.Time             `json:"updatedAt"`
}

type ReconciledOperation struct {
	OperationID      string `json:"operationID"`
	OperationKind    string `json:"operationKind"`
	Scope            string `json:"scope"`
	StaleClass       string `json:"staleClass"`
	RecoveryRequired bool   `json:"recoveryRequired"`
	NextAction       string `json:"nextAction,omitempty"`
	Result           string `json:"result,omitempty"`
}

type LiveInvocationFunc func(InvocationRecord) bool

func NewStore(root string) (Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Store{}, fmt.Errorf("operation store root is required")
	}
	return Store{Root: root}, nil
}

func (s Store) Create(record OperationRecord, eventID string, now time.Time) (OperationRecord, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	record = normalizeNewRecord(record, now)
	record.RecordRevision = 1
	record.LatestJournalSeq = 1
	if err := ValidateRecord(record); err != nil {
		return OperationRecord{}, err
	}
	dir, err := s.operationDir(record.OperationID)
	if err != nil {
		return OperationRecord{}, err
	}
	return withOperationLock(dir, true, func() (OperationRecord, error) {
		if hasExistingJournal(filepath.Join(dir, "journal")) {
			return OperationRecord{}, fmt.Errorf("operation %q already has a journal", record.OperationID)
		}
		if _, err := os.Stat(filepath.Join(dir, "record.json")); err == nil {
			return OperationRecord{}, fmt.Errorf("operation %q already exists", record.OperationID)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return OperationRecord{}, err
		}
		event := JournalEvent{
			APIVersion: APIVersion,
			Kind:       EventKind,
			Sequence:   1,
			EventID:    cleanEventID(eventID, "accepted"),
			EventType:  "accepted",
			Record:     record,
			CreatedAt:  now.UTC(),
		}
		if err := s.writeEventAndSnapshot(dir, event); err != nil {
			return OperationRecord{}, err
		}
		return record, nil
	})
}

func (s Store) OperationIDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Clean(s.Root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read operation store: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := cleanSegment("operation id", entry.Name())
		if err != nil {
			return nil, fmt.Errorf("invalid operation directory %q: %w", entry.Name(), err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s Store) ReconcileBoot(now time.Time, bootID string, live LiveInvocationFunc) (ReconcileReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	bootID = strings.TrimSpace(bootID)
	ids, err := s.OperationIDs()
	if err != nil {
		return ReconcileReport{}, err
	}
	report := ReconcileReport{
		APIVersion: APIVersion,
		Kind:       "OperationReconcileReport",
		Boot:       true,
		UpdatedAt:  now.UTC(),
	}
	for _, id := range ids {
		record, err := s.Read(id)
		if err != nil {
			return ReconcileReport{}, fmt.Errorf("reconcile operation %s: %w", id, err)
		}
		class := ClassifyStale(record)
		if class == StaleTerminal || hasLiveInvocation(record, bootID, live) {
			class = StaleNotStale
		} else {
			record, err = s.reconcileStaleRecord(record, class, now)
			if err != nil {
				return ReconcileReport{}, fmt.Errorf("reconcile operation %s: %w", id, err)
			}
		}
		report.Operations = append(report.Operations, ReconciledOperation{
			OperationID:      record.OperationID,
			OperationKind:    record.OperationKind,
			Scope:            record.Scope,
			StaleClass:       class,
			RecoveryRequired: record.RecoveryRequired,
			NextAction:       record.NextAction,
			Result:           record.Result,
		})
	}
	return report, nil
}

func (s Store) reconcileStaleRecord(record OperationRecord, class string, now time.Time) (OperationRecord, error) {
	return s.Update(record.OperationID, "boot-reconcile-"+class, "boot-reconcile", func(next OperationRecord) (OperationRecord, error) {
		next.Interruption = class
		next.UpdatedAt = now.UTC()
		switch class {
		case StalePostMutation, StaleAmbiguous:
			next.RecoveryRequired = true
			next.Result = ResultFailedNeedsRepair
			next.NextAction = "explicit repair or retry required"
		case StaleHostOnly:
			if canCompleteHostBookkeeping(next) {
				completedAt := now.UTC()
				next.Terminal = true
				next.CompletedAt = &completedAt
				next.Result = ResultSucceeded
				next.NextAction = "idempotent host bookkeeping finalizer completed"
			} else {
				next.NextAction = "classified host-only; no automatic resume marker present"
			}
		case StalePreMutation:
			next.NextAction = "resubmit operation request"
		default:
			next.RecoveryRequired = true
			next.Result = ResultFailedNeedsRepair
			next.NextAction = "manual inspection required"
		}
		return next, nil
	})
}

func (s Store) Update(operationID string, eventID string, eventType string, mutate func(OperationRecord) (OperationRecord, error)) (OperationRecord, error) {
	dir, err := s.operationDir(operationID)
	if err != nil {
		return OperationRecord{}, err
	}
	return withOperationLock(dir, false, func() (OperationRecord, error) {
		current, err := s.Read(operationID)
		if err != nil {
			return OperationRecord{}, err
		}
		previous := cloneRecord(current)
		next, err := mutate(cloneRecord(current))
		if err != nil {
			return OperationRecord{}, err
		}
		if err := ValidateTransition(previous, next); err != nil {
			return OperationRecord{}, err
		}
		next.RecordRevision = previous.RecordRevision + 1
		next.LatestJournalSeq = previous.LatestJournalSeq + 1
		if next.UpdatedAt.IsZero() || !next.UpdatedAt.After(previous.UpdatedAt) {
			next.UpdatedAt = time.Now().UTC()
		}
		if err := ValidateRecord(next); err != nil {
			return OperationRecord{}, err
		}
		event := JournalEvent{
			APIVersion: APIVersion,
			Kind:       EventKind,
			Sequence:   next.LatestJournalSeq,
			EventID:    cleanEventID(eventID, eventType),
			EventType:  strings.TrimSpace(eventType),
			Record:     next,
			CreatedAt:  next.UpdatedAt.UTC(),
		}
		if err := s.writeEventAndSnapshot(dir, event); err != nil {
			return OperationRecord{}, err
		}
		return next, nil
	})
}

func (s Store) AddPreExecMutationMarker(operationID string, marker PreExecMutationMarker) (OperationRecord, error) {
	return s.Update(operationID, marker.MarkerID, "pre-exec-mutation", func(record OperationRecord) (OperationRecord, error) {
		if strings.TrimSpace(marker.MarkerID) == "" {
			return OperationRecord{}, fmt.Errorf("mutation marker id is required")
		}
		if strings.TrimSpace(marker.Phase) == "" {
			marker.Phase = record.Phase
		}
		if strings.TrimSpace(marker.Tool) == "" {
			return OperationRecord{}, fmt.Errorf("mutation marker tool is required")
		}
		if strings.TrimSpace(marker.ArgvDigest) == "" {
			return OperationRecord{}, fmt.Errorf("mutation marker argv digest is required")
		}
		if marker.MarkedAt.IsZero() {
			marker.MarkedAt = time.Now().UTC()
		}
		record.ExternalMutationStarted = true
		record.PreExecMutationMarkers = append(record.PreExecMutationMarkers, marker)
		record.MutationScopes = appendMissingStrings(record.MutationScopes, marker.ExpectedMutationScopes...)
		return record, nil
	})
}

func (s Store) AddDiagnosticArtifact(operationID string, artifactID string, content []byte, now time.Time) (OperationRecord, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if strings.TrimSpace(artifactID) == "" {
		return OperationRecord{}, fmt.Errorf("diagnostic artifact id is required")
	}
	var err error
	artifactID, err = cleanSegment("diagnostic artifact id", artifactID)
	if err != nil {
		return OperationRecord{}, err
	}
	current, err := s.Read(operationID)
	if err != nil {
		return OperationRecord{}, err
	}
	if current.Terminal {
		return OperationRecord{}, fmt.Errorf("terminal operation records are immutable")
	}
	for _, artifact := range current.DiagnosticArtifacts {
		if artifact.ArtifactID == artifactID {
			return OperationRecord{}, fmt.Errorf("diagnostic artifact %q already exists", artifactID)
		}
	}
	dir, err := s.operationDir(operationID)
	if err != nil {
		return OperationRecord{}, err
	}
	attachmentPath := filepath.Join(dir, "attachments", artifactID+".log")
	if err := writeFileNoReplace(attachmentPath, content, 0o600, 0o700); err != nil {
		return OperationRecord{}, fmt.Errorf("write diagnostic artifact: %w", err)
	}
	sum := sha256.Sum256(content)
	rel := filepath.ToSlash(filepath.Join("attachments", artifactID+".log"))
	artifact := DiagnosticArtifact{
		ArtifactID: artifactID,
		Path:       rel,
		SHA256:     hex.EncodeToString(sum[:]),
		Redacted:   true,
		CreatedAt:  now.UTC(),
	}
	updated, err := s.Update(operationID, artifactID, "diagnostic-artifact", func(record OperationRecord) (OperationRecord, error) {
		for _, existing := range record.DiagnosticArtifacts {
			if existing.ArtifactID == artifactID {
				return OperationRecord{}, fmt.Errorf("diagnostic artifact %q already exists", artifactID)
			}
		}
		record.DiagnosticArtifacts = append(record.DiagnosticArtifacts, artifact)
		return record, nil
	})
	if err != nil {
		_ = os.Remove(attachmentPath)
		return OperationRecord{}, err
	}
	return updated, nil
}

func (s Store) Read(operationID string) (OperationRecord, error) {
	dir, err := s.operationDir(operationID)
	if err != nil {
		return OperationRecord{}, err
	}
	events, err := readJournal(filepath.Join(dir, "journal"))
	if err != nil {
		return OperationRecord{}, err
	}
	if len(events) == 0 {
		return OperationRecord{}, fmt.Errorf("operation %q has no valid journal events", operationID)
	}
	if snapshot, err := readSnapshot(filepath.Join(dir, "record.json"), events); err == nil {
		return snapshot.Record, nil
	}
	record := events[len(events)-1].Record
	if err := s.writeSnapshot(dir, events); err != nil {
		return OperationRecord{}, err
	}
	return record, nil
}

func (s Store) EventsAfter(operationID string, afterSeq int) ([]JournalEvent, error) {
	dir, err := s.operationDir(operationID)
	if err != nil {
		return nil, err
	}
	events, err := readJournal(filepath.Join(dir, "journal"))
	if err != nil {
		return nil, err
	}
	var out []JournalEvent
	for _, event := range events {
		if event.Sequence > afterSeq {
			out = append(out, event)
		}
	}
	return out, nil
}

func (s Store) writeEventAndSnapshot(dir string, event JournalEvent) error {
	if err := ValidateEvent(event); err != nil {
		return err
	}
	journalDir := filepath.Join(dir, "journal")
	name := fmt.Sprintf("%020d.%s.json", event.Sequence, event.EventID)
	data, err := marshalEnvelope(RecordTypeJournal, event)
	if err != nil {
		return err
	}
	if err := writeFileNoReplace(filepath.Join(journalDir, name), data, 0o600, 0o700); err != nil {
		return fmt.Errorf("write operation journal event: %w", err)
	}
	events, err := readJournal(journalDir)
	if err != nil {
		return err
	}
	return s.writeSnapshot(dir, events)
}

func (s Store) writeSnapshot(dir string, events []JournalEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("operation snapshot requires journal events")
	}
	record := events[len(events)-1].Record
	digest, err := journalDigest(events)
	if err != nil {
		return err
	}
	snapshot := Snapshot{
		APIVersion:    APIVersion,
		Kind:          "OperationRecordSnapshot",
		OperationID:   record.OperationID,
		LatestSeq:     events[len(events)-1].Sequence,
		JournalDigest: digest,
		Record:        record,
		UpdatedAt:     time.Now().UTC(),
	}
	data, err := marshalEnvelope(RecordTypeOperation, snapshot)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "record.json"), data, 0o600); err != nil {
		return fmt.Errorf("write operation snapshot: %w", err)
	}
	return nil
}

func (s Store) operationDir(operationID string) (string, error) {
	operationID, err := cleanSegment("operation id", operationID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(s.Root), operationID), nil
}

func ValidateEvent(event JournalEvent) error {
	if event.APIVersion != APIVersion {
		return fmt.Errorf("operation journal event apiVersion must be %q", APIVersion)
	}
	if event.Kind != EventKind {
		return fmt.Errorf("operation journal event kind must be %q", EventKind)
	}
	if event.Sequence < 1 {
		return fmt.Errorf("operation journal event sequence must be positive")
	}
	if _, err := cleanSegment("event id", event.EventID); err != nil {
		return err
	}
	if strings.TrimSpace(event.EventType) == "" {
		return fmt.Errorf("operation journal event type is required")
	}
	if err := ValidateRecord(event.Record); err != nil {
		return err
	}
	if event.Record.LatestJournalSeq != event.Sequence {
		return fmt.Errorf("operation journal event sequence does not match record latestJournalSeq")
	}
	if event.CreatedAt.IsZero() {
		return fmt.Errorf("operation journal event createdAt is required")
	}
	return nil
}

func ValidateRecord(record OperationRecord) error {
	if record.APIVersion != APIVersion {
		return fmt.Errorf("operation record apiVersion must be %q", APIVersion)
	}
	if record.Kind != RecordKind {
		return fmt.Errorf("operation record kind must be %q", RecordKind)
	}
	if record.SchemaVersion != SchemaVersion {
		return fmt.Errorf("operation record schemaVersion must be %d", SchemaVersion)
	}
	if _, err := cleanSegment("operation id", record.OperationID); err != nil {
		return err
	}
	if strings.TrimSpace(record.OperationKind) == "" {
		return fmt.Errorf("operation kind is required")
	}
	if strings.TrimSpace(record.Scope) == "" {
		return fmt.Errorf("operation scope is required")
	}
	if strings.TrimSpace(record.RequestDigest) == "" {
		return fmt.Errorf("operation requestDigest is required")
	}
	if record.RecordRevision < 1 {
		return fmt.Errorf("operation recordRevision must be positive")
	}
	if record.LatestJournalSeq < 1 {
		return fmt.Errorf("operation latestJournalSeq must be positive")
	}
	if record.PhaseIndex < 0 {
		return fmt.Errorf("operation phaseIndex must not be negative")
	}
	if record.Terminal && strings.TrimSpace(record.Result) == "" {
		return fmt.Errorf("terminal operation result is required")
	}
	if record.Terminal && record.CompletedAt == nil {
		return fmt.Errorf("terminal operation completedAt is required")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return fmt.Errorf("operation createdAt and updatedAt are required")
	}
	if record.BootstrapRequest != nil {
		if err := validateBootstrapRequest(*record.BootstrapRequest); err != nil {
			return err
		}
		if strings.TrimSpace(record.BootstrapRequest.JoinMaterialDigest) != "" {
			if err := validateSHA256Hex("bootstrapRequest joinMaterialDigest", record.BootstrapRequest.JoinMaterialDigest); err != nil {
				return err
			}
		}
	}
	if record.ConfigApplyRequest != nil {
		if err := validateConfigApplyRequest(*record.ConfigApplyRequest); err != nil {
			return err
		}
	}
	if record.KubernetesSysextUpdate != nil {
		if err := validateKubernetesSysextUpdate(*record.KubernetesSysextUpdate); err != nil {
			return err
		}
	}
	if record.DestructiveResetRequest != nil {
		if err := ValidateDestructiveReset(*record.DestructiveResetRequest); err != nil {
			return err
		}
	}
	if record.HostUpgradeRequest != nil {
		if err := ValidateHostUpgrade(*record.HostUpgradeRequest); err != nil {
			return err
		}
	}
	if err := validateRequestBodyConsistency(record); err != nil {
		return err
	}
	if err := validateOptionalEnum("activationMode", record.ActivationMode, ActivationModeLive, ActivationModeNextBoot); err != nil {
		return err
	}
	if err := validateOptionalEnum("activationState", record.ActivationState, ActivationStatePending, ActivationStateActivating, ActivationStateActiveLive, ActivationStateFailed, ActivationStateRolledBack); err != nil {
		return err
	}
	if err := validateOptionalEnum("generationCommitState", record.GenerationCommitState, GenerationCommitCandidate, GenerationCommitCommitted, GenerationCommitAbandoned); err != nil {
		return err
	}
	if err := validateOptionalEnum("postKubeadmHealthState", record.PostKubeadmHealthState, PostKubeadmHealthNotRun, PostKubeadmHealthRunning, PostKubeadmHealthPassed, PostKubeadmHealthFailed); err != nil {
		return err
	}
	if record.ExecutorPlan != nil {
		if strings.TrimSpace(record.ExecutorPlan.Phase) == "" {
			return fmt.Errorf("operation executorPlan phase is required")
		}
		if len(record.ExecutorPlan.Argv) == 0 || strings.TrimSpace(record.ExecutorPlan.Argv[0]) == "" {
			return fmt.Errorf("operation executorPlan argv is required")
		}
	}
	for _, marker := range record.PreExecMutationMarkers {
		if strings.TrimSpace(marker.MarkerID) == "" || strings.TrimSpace(marker.Tool) == "" || strings.TrimSpace(marker.ArgvDigest) == "" || marker.MarkedAt.IsZero() {
			return fmt.Errorf("pre-exec mutation marker is incomplete")
		}
	}
	for _, artifact := range record.DiagnosticArtifacts {
		if strings.TrimSpace(artifact.ArtifactID) == "" || strings.TrimSpace(artifact.Path) == "" || strings.TrimSpace(artifact.SHA256) == "" || !artifact.Redacted {
			return fmt.Errorf("diagnostic artifact must be redacted and complete")
		}
		if err := validateArtifactPath(artifact.Path); err != nil {
			return err
		}
		if err := validateSHA256Hex("diagnostic artifact", artifact.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func validateRequestBodyConsistency(record OperationRecord) error {
	bodyCount := 0
	if record.BootstrapRequest != nil {
		bodyCount++
	}
	if record.ConfigApplyRequest != nil {
		bodyCount++
	}
	if record.KubernetesSysextUpdate != nil {
		bodyCount++
	}
	if record.DestructiveResetRequest != nil {
		bodyCount++
	}
	if record.HostUpgradeRequest != nil {
		bodyCount++
	}
	if bodyCount > 1 {
		return fmt.Errorf("operation record has multiple request bodies")
	}
	if record.KubernetesSysextUpdate != nil && record.OperationKind != "kubeadm-upgrade" {
		return fmt.Errorf("operation kind %q cannot include kubernetesSysextUpdate", record.OperationKind)
	}
	if record.OperationKind == "kubeadm-upgrade" && record.KubernetesSysextUpdate == nil {
		return fmt.Errorf("kubeadm-upgrade operation requires kubernetesSysextUpdate")
	}
	if record.DestructiveResetRequest != nil && record.OperationKind != "destructive-reset" {
		return fmt.Errorf("operation kind %q cannot include destructiveResetRequest", record.OperationKind)
	}
	if record.OperationKind == "destructive-reset" && record.DestructiveResetRequest == nil {
		return fmt.Errorf("destructive-reset operation requires destructiveResetRequest")
	}
	if record.HostUpgradeRequest != nil && record.OperationKind != "host-upgrade" {
		return fmt.Errorf("operation kind %q cannot include hostUpgradeRequest", record.OperationKind)
	}
	if record.OperationKind == "host-upgrade" && record.HostUpgradeRequest == nil {
		return fmt.Errorf("host-upgrade operation requires hostUpgradeRequest")
	}
	return nil
}

func ValidateTransition(previous OperationRecord, next OperationRecord) error {
	if previous.OperationID != next.OperationID {
		return fmt.Errorf("operation transition cannot change operationID")
	}
	if previous.Terminal {
		return fmt.Errorf("terminal operation records are immutable")
	}
	if next.RecordRevision != previous.RecordRevision && next.RecordRevision != previous.RecordRevision+1 {
		return fmt.Errorf("operation recordRevision must advance by one")
	}
	if next.LatestJournalSeq != previous.LatestJournalSeq && next.LatestJournalSeq != previous.LatestJournalSeq+1 {
		return fmt.Errorf("operation latestJournalSeq must advance by one")
	}
	if next.ExpectedMachineID != previous.ExpectedMachineID {
		return fmt.Errorf("operation expectedMachineID is immutable")
	}
	if next.ExpectedCurrentGenerationID != previous.ExpectedCurrentGenerationID {
		return fmt.Errorf("operation expectedCurrentGenerationID is immutable")
	}
	if next.ExpectedClusterIntentDigest != previous.ExpectedClusterIntentDigest {
		return fmt.Errorf("operation expectedClusterIntentDigest is immutable")
	}
	if next.PhaseIndex < previous.PhaseIndex {
		return fmt.Errorf("operation phaseIndex must not decrease")
	}
	if !hasPrefix(next.CompletedPhases, previous.CompletedPhases) {
		return fmt.Errorf("operation completedPhases must be append-only")
	}
	if previous.ExternalMutationStarted && !next.ExternalMutationStarted {
		return fmt.Errorf("operation externalMutationStarted cannot be cleared")
	}
	if !hasPrefixMarkers(next.PreExecMutationMarkers, previous.PreExecMutationMarkers) {
		return fmt.Errorf("operation pre-exec mutation markers must be append-only")
	}
	if !hasPrefix(next.MutationScopes, previous.MutationScopes) {
		return fmt.Errorf("operation mutationScopes must be append-only")
	}
	if previous.MutatingToolRan && !next.MutatingToolRan {
		return fmt.Errorf("operation mutatingToolRan cannot be cleared")
	}
	if !hasPrefix(next.MutatingToolInvocations, previous.MutatingToolInvocations) {
		return fmt.Errorf("operation mutatingToolInvocations must be append-only")
	}
	if !hasPrefixArtifacts(next.DiagnosticArtifacts, previous.DiagnosticArtifacts) {
		return fmt.Errorf("operation diagnosticArtifacts must be append-only")
	}
	if !bootstrapRequestTransitionAllowed(previous.BootstrapRequest, next.BootstrapRequest) {
		return fmt.Errorf("operation bootstrapRequest is immutable")
	}
	if !reflect.DeepEqual(next.ConfigApplyRequest, previous.ConfigApplyRequest) {
		return fmt.Errorf("operation configApplyRequest is immutable")
	}
	if !reflect.DeepEqual(next.KubernetesSysextUpdate, previous.KubernetesSysextUpdate) {
		return fmt.Errorf("operation kubernetesSysextUpdate is immutable")
	}
	if !reflect.DeepEqual(next.DestructiveResetRequest, previous.DestructiveResetRequest) {
		return fmt.Errorf("operation destructiveResetRequest is immutable")
	}
	if !reflect.DeepEqual(next.HostUpgradeRequest, previous.HostUpgradeRequest) {
		return fmt.Errorf("operation hostUpgradeRequest is immutable")
	}
	return nil
}

func bootstrapRequestTransitionAllowed(previous *BootstrapRequest, next *BootstrapRequest) bool {
	if previous == nil || next == nil {
		return previous == nil && next == nil
	}
	previousComparable := *previous
	nextComparable := *next
	previousComparable.KubernetesBundleManifestDigest = ""
	nextComparable.KubernetesBundleManifestDigest = ""
	previousComparable.KubernetesSysextPayloadDigest = ""
	nextComparable.KubernetesSysextPayloadDigest = ""
	if !reflect.DeepEqual(previousComparable, nextComparable) {
		return false
	}
	return resolvedDigestTransitionAllowed(previous.KubernetesBundleManifestDigest, next.KubernetesBundleManifestDigest) &&
		resolvedDigestTransitionAllowed(previous.KubernetesSysextPayloadDigest, next.KubernetesSysextPayloadDigest)
}

func resolvedDigestTransitionAllowed(previous string, next string) bool {
	previous = strings.TrimSpace(previous)
	next = strings.TrimSpace(next)
	return previous == next || previous == "" && next != ""
}

func ClassifyStale(record OperationRecord) string {
	if record.Terminal {
		return StaleTerminal
	}
	if record.ExternalMutationStarted || record.MutatingToolRan || len(record.PreExecMutationMarkers) > 0 || len(record.MutationScopes) > 0 {
		return StalePostMutation
	}
	if mutatingPhase(record.Phase) {
		return StalePostMutation
	}
	switch record.Scope {
	case "host-generation", "install-state":
		if record.PhaseIndex > 0 || len(record.CompletedPhases) > 0 {
			return StaleHostOnly
		}
	}
	if mutatingOperationKind(record.OperationKind) {
		return StaleAmbiguous
	}
	if record.PhaseIndex == 0 && len(record.CompletedPhases) == 0 {
		return StalePreMutation
	}
	return StaleAmbiguous
}

func normalizeNewRecord(record OperationRecord, now time.Time) OperationRecord {
	record.APIVersion = APIVersion
	record.Kind = RecordKind
	record.SchemaVersion = SchemaVersion
	record.OperationID = strings.TrimSpace(record.OperationID)
	record.OperationKind = strings.TrimSpace(record.OperationKind)
	record.Scope = strings.TrimSpace(record.Scope)
	record.RequestDigest = strings.TrimSpace(record.RequestDigest)
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now.UTC()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	return record
}

func cloneRecord(record OperationRecord) OperationRecord {
	record.PhasePlan = cloneStrings(record.PhasePlan)
	record.CompletedPhases = cloneStrings(record.CompletedPhases)
	record.ResourceLocks = cloneStrings(record.ResourceLocks)
	if record.BootstrapRequest != nil {
		request := *record.BootstrapRequest
		record.BootstrapRequest = &request
	}
	if record.ConfigApplyRequest != nil {
		request := *record.ConfigApplyRequest
		record.ConfigApplyRequest = &request
	}
	if record.KubernetesSysextUpdate != nil {
		request := *record.KubernetesSysextUpdate
		record.KubernetesSysextUpdate = &request
	}
	if record.DestructiveResetRequest != nil {
		request := *record.DestructiveResetRequest
		request.WipeSurfaces = cloneStrings(request.WipeSurfaces)
		record.DestructiveResetRequest = &request
	}
	if record.HostUpgradeRequest != nil {
		request := *record.HostUpgradeRequest
		record.HostUpgradeRequest = &request
	}
	if record.ExecutorPlan != nil {
		plan := *record.ExecutorPlan
		plan.MutationScopes = cloneStrings(plan.MutationScopes)
		plan.Argv = cloneStrings(plan.Argv)
		record.ExecutorPlan = &plan
	}
	record.Invocations = cloneInvocations(record.Invocations)
	record.PreExecMutationMarkers = cloneMarkers(record.PreExecMutationMarkers)
	record.MutationScopes = cloneStrings(record.MutationScopes)
	record.MutatingToolInvocations = cloneStrings(record.MutatingToolInvocations)
	record.DiagnosticArtifacts = append([]DiagnosticArtifact(nil), record.DiagnosticArtifacts...)
	if record.CompletedAt != nil {
		completedAt := *record.CompletedAt
		record.CompletedAt = &completedAt
	}
	return record
}

func cloneInvocations(values []InvocationRecord) []InvocationRecord {
	out := append([]InvocationRecord(nil), values...)
	for i := range out {
		out[i].ChildProcess = cloneStrings(out[i].ChildProcess)
		if out[i].CompletedAt != nil {
			completedAt := *out[i].CompletedAt
			out[i].CompletedAt = &completedAt
		}
	}
	return out
}

func cloneMarkers(values []PreExecMutationMarker) []PreExecMutationMarker {
	out := append([]PreExecMutationMarker(nil), values...)
	for i := range out {
		out[i].ExpectedMutationScopes = cloneStrings(out[i].ExpectedMutationScopes)
	}
	return out
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}

func hasLiveInvocation(record OperationRecord, bootID string, live LiveInvocationFunc) bool {
	if live == nil {
		return false
	}
	for _, invocation := range record.Invocations {
		if invocation.CompletedAt == nil && strings.TrimSpace(bootID) != "" && invocation.BootID == bootID && strings.TrimSpace(invocation.AgentStartID) != "" && invocation.PID > 0 && live(invocation) {
			return true
		}
		if strings.TrimSpace(bootID) == "" {
			continue
		}
		if invocation.CompletedAt == nil && strings.TrimSpace(invocation.SystemdInvocationID) != "" && invocation.BootID == bootID && live(invocation) {
			return true
		}
	}
	return false
}

func canCompleteHostBookkeeping(record OperationRecord) bool {
	return record.Resume == ResumeHostBookkeeping &&
		record.OperationKind == HostBookkeepingOperationKind &&
		record.Scope == HostBookkeepingGenerationScope &&
		record.Phase == HostBookkeepingCompletionPhase &&
		!record.ExternalMutationStarted &&
		!record.MutatingToolRan &&
		len(record.MutatingToolInvocations) == 0 &&
		len(record.PreExecMutationMarkers) == 0 &&
		len(record.MutationScopes) == 0
}

func readSnapshot(path string, events []JournalEvent) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return Snapshot{}, err
	}
	if len(events) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot validation requires journal events")
	}
	digest, err := journalDigest(events)
	if err != nil {
		return Snapshot{}, err
	}
	latest := events[len(events)-1]
	if snapshot.APIVersion != APIVersion || snapshot.OperationID != latest.Record.OperationID || snapshot.LatestSeq != latest.Sequence || snapshot.JournalDigest != digest {
		return Snapshot{}, fmt.Errorf("operation snapshot is stale or digest-invalid")
	}
	if snapshot.Record.LatestJournalSeq != latest.Record.LatestJournalSeq || snapshot.Record.RecordRevision != latest.Record.RecordRevision {
		return Snapshot{}, fmt.Errorf("operation snapshot record is stale")
	}
	if !reflect.DeepEqual(snapshot.Record, latest.Record) {
		return Snapshot{}, fmt.Errorf("operation snapshot record differs from journal")
	}
	if err := ValidateRecord(snapshot.Record); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func readJournal(dir string) ([]JournalEvent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read operation journal: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	events := make([]JournalEvent, 0, len(names))
	seenSeq := map[int]string{}
	for _, name := range names {
		seq, ok := journalSeq(name)
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		event, err := decodeJournalEvent(data)
		if err != nil {
			continue
		}
		if event.Sequence != seq {
			continue
		}
		if err := ValidateEvent(event); err != nil {
			continue
		}
		if previousName, ok := seenSeq[event.Sequence]; ok {
			return nil, fmt.Errorf("duplicate valid journal sequence %d in %s and %s", event.Sequence, previousName, name)
		}
		seenSeq[event.Sequence] = name
		if len(events) > 0 {
			prev := events[len(events)-1]
			if event.Sequence != prev.Sequence+1 {
				break
			}
			if err := ValidateTransition(prev.Record, event.Record); err != nil {
				break
			}
		} else if event.Sequence != 1 {
			break
		}
		events = append(events, event)
	}
	return events, nil
}

func journalSeq(name string) (int, bool) {
	prefix, _, ok := strings.Cut(name, ".")
	if !ok {
		return 0, false
	}
	seq, err := strconv.Atoi(prefix)
	if err != nil || seq < 1 {
		return 0, false
	}
	return seq, true
}

func journalDigest(events []JournalEvent) (string, error) {
	hash := sha256.New()
	for _, event := range events {
		data, err := marshalJSON(event)
		if err != nil {
			return "", err
		}
		hash.Write(data)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func marshalEnvelope(recordType string, payload any) ([]byte, error) {
	payloadData, err := marshalJSON(payload)
	if err != nil {
		return nil, err
	}
	return persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    recordType,
		RecordVersion: recordEnvelopeVersion,
		Payload:       payloadData,
	})
}

func decodeSnapshot(data []byte) (Snapshot, error) {
	if snapshot, ok, err := decodeEnvelope[Snapshot](data, RecordTypeOperation); ok {
		return snapshot, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func decodeJournalEvent(data []byte) (JournalEvent, error) {
	if event, ok, err := decodeEnvelope[JournalEvent](data, RecordTypeJournal); ok {
		return event, err
	}
	var event JournalEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return JournalEvent{}, err
	}
	return event, nil
}

func decodeEnvelope[T any](data []byte, recordType string) (T, bool, error) {
	var zero T
	if !looksLikeEnvelope(data) {
		return zero, false, nil
	}
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err != nil {
		return zero, true, err
	}
	if envelope.RecordType != recordType {
		return zero, true, fmt.Errorf("%w: got %s/v%d, want %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion, recordType, recordEnvelopeVersion)
	}
	if envelope.RecordVersion != recordEnvelopeVersion {
		return zero, true, fmt.Errorf("%w: %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion)
	}
	payload, err := persistedrecord.DecodePayload[T](envelope)
	return payload, true, err
}

func looksLikeEnvelope(data []byte) bool {
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

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDirMode(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
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
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	return dirHandle.Sync()
}

func writeFileNoReplace(path string, data []byte, mode os.FileMode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDirMode(dir, dirMode); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
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
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	if err := fsyncDir(dir); err != nil {
		return err
	}
	cleanup = false
	_ = os.Remove(tmpPath)
	return nil
}

func ensureDir(dir string) error {
	return ensureDirMode(dir, 0o755)
}

func ensureDirMode(dir string, mode os.FileMode) error {
	dir = filepath.Clean(dir)
	existing := dir
	for {
		info, err := os.Stat(existing)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", existing)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return err
		}
		existing = parent
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	if err := os.Chmod(dir, mode); err != nil {
		return err
	}
	for current := dir; ; current = filepath.Dir(current) {
		if err := fsyncDir(current); err != nil {
			return err
		}
		if current == existing {
			break
		}
	}
	return nil
}

func withOperationLock(dir string, create bool, fn func() (OperationRecord, error)) (OperationRecord, error) {
	if create {
		if err := ensureDirMode(filepath.Dir(dir), 0o750); err != nil {
			return OperationRecord{}, err
		}
		if err := ensureDirMode(dir, 0o700); err != nil {
			return OperationRecord{}, err
		}
	} else {
		info, err := os.Stat(dir)
		if err != nil {
			return OperationRecord{}, err
		}
		if !info.IsDir() {
			return OperationRecord{}, fmt.Errorf("%s exists and is not a directory", dir)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return OperationRecord{}, err
		}
	}
	lockPath := filepath.Join(dir, ".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return OperationRecord{}, fmt.Errorf("open operation lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return OperationRecord{}, fmt.Errorf("lock operation: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func fsyncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func cleanSegment(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || value == "." || value == ".." || strings.Contains(value, "..") {
		return "", fmt.Errorf("%s must be a clean path segment", name)
	}
	return value, nil
}

func cleanEventID(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ReplaceAll(value, "_", "-")
	return value
}

func appendMissingStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		values = append(values, value)
		seen[value] = struct{}{}
	}
	return values
}

func hasPrefix(values []string, prefix []string) bool {
	if len(values) < len(prefix) {
		return false
	}
	for i := range prefix {
		if values[i] != prefix[i] {
			return false
		}
	}
	return true
}

func hasPrefixMarkers(values []PreExecMutationMarker, prefix []PreExecMutationMarker) bool {
	if len(values) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !reflect.DeepEqual(values[i], prefix[i]) {
			return false
		}
	}
	return true
}

func hasPrefixArtifacts(values []DiagnosticArtifact, prefix []DiagnosticArtifact) bool {
	if len(values) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !reflect.DeepEqual(values[i], prefix[i]) {
			return false
		}
	}
	return true
}

func hasExistingJournal(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && !strings.HasPrefix(entry.Name(), ".") {
			return true
		}
	}
	return false
}

func validateArtifactPath(path string) error {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return fmt.Errorf("diagnostic artifact path must be relative")
	}
	cleaned := filepath.ToSlash(filepath.Clean(path))
	if cleaned != path || !strings.HasPrefix(cleaned, "attachments/") || strings.Contains(cleaned, "..") {
		return fmt.Errorf("diagnostic artifact path must be under attachments")
	}
	return nil
}

func validateBootstrapRequest(request BootstrapRequest) error {
	if strings.TrimSpace(request.InventoryNodeName) == "" {
		return fmt.Errorf("bootstrapRequest inventoryNodeName is required")
	}
	switch strings.TrimSpace(request.SystemRole) {
	case "control-plane", "worker":
	default:
		return fmt.Errorf("bootstrapRequest systemRole must be control-plane or worker")
	}
	if strings.TrimSpace(request.KubernetesPayloadVersion) == "" {
		return fmt.Errorf("bootstrapRequest kubernetesPayloadVersion is required")
	}
	if (strings.TrimSpace(request.KubernetesBundleSource) == "") != (strings.TrimSpace(request.KubernetesBundleRef) == "") {
		return fmt.Errorf("bootstrapRequest kubernetesBundleSource and kubernetesBundleRef must be set together")
	}
	if strings.TrimSpace(request.KubernetesBundleManifestDigest) != "" {
		if err := validateSHA256Digest("bootstrapRequest kubernetesBundleManifestDigest", request.KubernetesBundleManifestDigest); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.KubernetesSysextPayloadDigest) != "" {
		if err := validateSHA256Digest("bootstrapRequest kubernetesSysextPayloadDigest", request.KubernetesSysextPayloadDigest); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.BootstrapProfileRef) == "" {
		return fmt.Errorf("bootstrapRequest bootstrapProfileRef is required")
	}
	return nil
}

func validateConfigApplyRequest(request ConfigApplyRequest) error {
	switch strings.TrimSpace(request.ApplyMode) {
	case "auto", "live", "next-boot":
	default:
		return fmt.Errorf("configApplyRequest applyMode must be auto, live, or next-boot")
	}
	if strings.TrimSpace(request.CandidateGenerationID) != "" {
		if _, err := cleanSegment("configApplyRequest candidateGenerationID", request.CandidateGenerationID); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.ConfigYAML) == "" {
		return fmt.Errorf("configApplyRequest configYAML is required")
	}
	return nil
}

func validateKubernetesSysextUpdate(request KubernetesSysextUpdate) error {
	if strings.TrimSpace(request.TargetPayloadVersion) == "" {
		return fmt.Errorf("kubernetesSysextUpdate targetPayloadVersion is required")
	}
	if strings.TrimSpace(request.TargetSysextPath) == "" {
		return fmt.Errorf("kubernetesSysextUpdate targetSysextPath is required")
	}
	if err := validateSHA256Hex("kubernetesSysextUpdate targetSysext", strings.TrimSpace(request.TargetSysextSHA256)); err != nil {
		return err
	}
	return nil
}

func ValidateDestructiveReset(request DestructiveReset) error {
	if strings.TrimSpace(request.InventoryNodeName) == "" {
		return fmt.Errorf("destructiveResetRequest inventoryNodeName is required")
	}
	switch strings.TrimSpace(request.ResetScope) {
	case "cluster", "node":
	default:
		return fmt.Errorf("destructiveResetRequest resetScope must be cluster or node")
	}
	if strings.TrimSpace(request.TargetGenerationID) != "" {
		if _, err := cleanSegment("destructiveResetRequest targetGenerationID", request.TargetGenerationID); err != nil {
			return err
		}
	}
	if !request.DiscardClusterIdentity {
		return fmt.Errorf("destructiveResetRequest discardClusterIdentity must be true")
	}
	for i, surface := range request.WipeSurfaces {
		if strings.TrimSpace(surface) == "" {
			return fmt.Errorf("destructiveResetRequest wipeSurfaces[%d] is empty", i)
		}
	}
	return nil
}

func ValidateHostUpgrade(request HostUpgrade) error {
	if (strings.TrimSpace(request.ImageURL) == "") == (strings.TrimSpace(request.ImageLocalRef) == "") {
		return fmt.Errorf("hostUpgradeRequest requires exactly one imageURL or imageLocalRef")
	}
	if strings.TrimSpace(request.ImageURL) != "" {
		parsed, err := url.Parse(request.ImageURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("hostUpgradeRequest imageURL must be an HTTPS URL without credentials, query, or fragment")
		}
	}
	if strings.TrimSpace(request.ImageLocalRef) != "" {
		ref := filepath.ToSlash(filepath.Clean(request.ImageLocalRef))
		if filepath.IsAbs(request.ImageLocalRef) || ref != request.ImageLocalRef || ref == "." || ref == ".." || strings.HasPrefix(ref, "../") {
			return fmt.Errorf("hostUpgradeRequest imageLocalRef must be a clean relative path")
		}
	}
	if err := validateSHA256Hex("hostUpgradeRequest image", strings.TrimSpace(request.ImageSHA256)); err != nil {
		return err
	}
	if _, err := cleanSegment("hostUpgradeRequest candidateGenerationID", request.CandidateGenerationID); err != nil {
		return err
	}
	return nil
}

func validateOptionalEnum(name string, value string, allowed ...string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("unsupported operation %s %q", name, value)
}

func validateSHA256Hex(name string, value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s SHA-256 must be %d lowercase hex characters", name, sha256.Size*2)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("%s SHA-256 must be lowercase hex", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s SHA-256 is invalid: %w", name, err)
	}
	return nil
}

func validateSHA256Digest(name string, value string) error {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") {
		return fmt.Errorf("%s must start with sha256:", name)
	}
	return validateSHA256Hex(name, strings.TrimPrefix(value, "sha256:"))
}

func mutatingPhase(phase string) bool {
	phase = strings.ToLower(strings.TrimSpace(phase))
	return strings.Contains(phase, "kubeadm") || strings.Contains(phase, "kubectl") || strings.Contains(phase, "etcd-running")
}

func mutatingOperationKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return strings.Contains(kind, "bootstrap") || strings.Contains(kind, "join") || strings.Contains(kind, "kubeadm") || strings.Contains(kind, "etcd") || strings.Contains(kind, "destructive-reset")
}
