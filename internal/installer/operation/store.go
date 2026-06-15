package operation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	APIVersion = "katl.dev/v1alpha1"

	RecordKind = "OperationRecord"
	EventKind  = "OperationJournalEvent"

	SchemaVersion = 1

	ResultTimedOut          = "timed-out"
	ResultFailedNeedsRepair = "failed-needs-repair"
	ResultSucceeded         = "succeeded"

	StaleTerminal     = "terminal"
	StalePreMutation  = "stale-pre-mutation"
	StaleHostOnly     = "stale-host-only"
	StalePostMutation = "stale-post-mutation"
	StaleAmbiguous    = "stale-ambiguous"
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
	RequestDigest               string                  `json:"requestDigest"`
	RecordRevision              int                     `json:"recordRevision"`
	LatestJournalSeq            int                     `json:"latestJournalSeq"`
	PhasePlan                   []string                `json:"phasePlan,omitempty"`
	PreviousGenerationID        string                  `json:"previousGenerationID,omitempty"`
	CandidateGenerationID       string                  `json:"candidateGenerationID,omitempty"`
	Phase                       string                  `json:"phase,omitempty"`
	PhaseIndex                  int                     `json:"phaseIndex"`
	CompletedPhases             []string                `json:"completedPhases,omitempty"`
	Terminal                    bool                    `json:"terminal"`
	ResourceLocks               []string                `json:"resourceLocks,omitempty"`
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

type InvocationRecord struct {
	InvocationID        string     `json:"invocationID"`
	SystemdInvocationID string     `json:"systemdInvocationID,omitempty"`
	UnitName            string     `json:"unitName,omitempty"`
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
}

func (s Store) Update(operationID string, eventID string, eventType string, mutate func(OperationRecord) (OperationRecord, error)) (OperationRecord, error) {
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
	dir, err := s.operationDir(operationID)
	if err != nil {
		return OperationRecord{}, err
	}
	if err := s.writeEventAndSnapshot(dir, event); err != nil {
		return OperationRecord{}, err
	}
	return next, nil
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
	if err := writeFileNoReplace(attachmentPath, content, 0o600); err != nil {
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

func (s Store) writeEventAndSnapshot(dir string, event JournalEvent) error {
	if err := ValidateEvent(event); err != nil {
		return err
	}
	journalDir := filepath.Join(dir, "journal")
	name := fmt.Sprintf("%020d.%s.json", event.Sequence, event.EventID)
	data, err := marshalJSON(event)
	if err != nil {
		return err
	}
	if err := writeFileNoReplace(filepath.Join(journalDir, name), data, 0o600); err != nil {
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
	data, err := marshalJSON(snapshot)
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
	return nil
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

func readSnapshot(path string, events []JournalEvent) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
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
		var event JournalEvent
		if err := json.Unmarshal(data, &event); err != nil {
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

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
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

func writeFileNoReplace(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

func mutatingPhase(phase string) bool {
	phase = strings.ToLower(strings.TrimSpace(phase))
	return strings.Contains(phase, "kubeadm") || strings.Contains(phase, "kubectl") || strings.Contains(phase, "etcd-running")
}

func mutatingOperationKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return strings.Contains(kind, "bootstrap") || strings.Contains(kind, "join") || strings.Contains(kind, "kubeadm") || strings.Contains(kind, "etcd")
}
