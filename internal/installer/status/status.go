package status

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "InstallToBootstrapStatus"

	RecordType    = "katl.install.status"
	recordVersion = 1
)

const (
	StateRunning                    = "running"
	StateDebugHold                  = "debug-hold"
	StateWaitingForConfig           = "waiting-for-config"
	StateInstallRefused             = "install-refused"
	StateFailedBeforeMutation       = "failed-before-mutation"
	StateFailedAfterMutation        = "failed-after-mutation"
	StateRebootRequested            = "reboot-requested"
	StateKubeadmReady               = "kubeadm-ready"
	StateWaitingForClusterBootstrap = "waiting-for-cluster-bootstrap"
	StateRuntimeBootedNotReady      = "runtime-booted-not-ready"
	StateRuntimeFailedNeedsRepair   = "runtime-failed-needs-repair"
)

const (
	InputModePXEPreseed   = "pxe-preseed"
	InputModeLocalHandoff = "local-handoff"
	InputModeOfflineMedia = "offline-media"
	InputModeTest         = "test"
)

var errorURLPattern = regexp.MustCompile(`https?://[^\s]+`)

type Record struct {
	APIVersion            string    `json:"apiVersion"`
	Kind                  string    `json:"kind"`
	State                 string    `json:"state"`
	CurrentStep           string    `json:"currentStep,omitempty"`
	CompletedSteps        []string  `json:"completedStates,omitempty"`
	InputMode             string    `json:"inputMode,omitempty"`
	InputSource           string    `json:"inputSource,omitempty"`
	RequestDigest         string    `json:"requestDigest,omitempty"`
	BundleDigest          string    `json:"bundleDigest,omitempty"`
	SourceDigest          string    `json:"sourceDigest,omitempty"`
	NodeMaterialDigest    string    `json:"nodeMaterialDigest,omitempty"`
	InstallMaterialDigest string    `json:"installMaterialDigest,omitempty"`
	KatlosImage           Image     `json:"katlosImage,omitempty"`
	TargetDiskStableID    string    `json:"targetDiskStableID,omitempty"`
	SelectedRootSlot      string    `json:"selectedRootSlot,omitempty"`
	InstalledGeneration   string    `json:"installedGeneration,omitempty"`
	BootArtifactVersion   string    `json:"bootArtifactVersion,omitempty"`
	RefusalReason         string    `json:"refusalReason,omitempty"`
	RetryHint             string    `json:"retryHint,omitempty"`
	LastError             string    `json:"lastError,omitempty"`
	FinalHandoff          string    `json:"finalHandoff,omitempty"`
	WipeTargetAccepted    bool      `json:"wipeTargetAccepted,omitempty"`
	DestructiveMutation   bool      `json:"destructiveMutationStarted,omitempty"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

type Image struct {
	URL              string `json:"url,omitempty"`
	LocalRef         string `json:"localRef,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
	SizeBytes        uint64 `json:"sizeBytes,omitempty"`
	Version          string `json:"version,omitempty"`
	Architecture     string `json:"architecture,omitempty"`
	RuntimeInterface string `json:"runtimeInterface,omitempty"`
	Role             string `json:"role,omitempty"`
}

func New(state string, now time.Time) Record {
	if strings.TrimSpace(state) == "" {
		state = StateRunning
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Record{
		APIVersion: APIVersion,
		Kind:       Kind,
		State:      state,
		UpdatedAt:  now.UTC(),
	}
}

func ImageFromManifest(manifest manifest.Manifest) Image {
	image := manifest.KatlosImage
	return Image{
		URL:              RedactSource(image.URL),
		LocalRef:         image.LocalRef,
		SHA256:           image.SHA256,
		SizeBytes:        image.SizeBytes,
		Version:          image.Version,
		Architecture:     image.Architecture,
		RuntimeInterface: image.RuntimeInterface,
		Role:             image.Role,
	}
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func DigestManifest(manifest manifest.Manifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("normalize install request: %w", err)
	}
	return Digest(data), nil
}

func RedactSource(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func RedactError(err error) string {
	if err == nil {
		return ""
	}
	return errorURLPattern.ReplaceAllStringFunc(err.Error(), RedactSource)
}

func WriteFile(path string, record Record) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("status path is required")
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install status: %w", err)
	}
	data, err := persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    RecordType,
		RecordVersion: recordVersion,
		Payload:       append(payload, '\n'),
	})
	if err != nil {
		return fmt.Errorf("marshal install status envelope: %w", err)
	}
	if err := persistedrecord.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write install status: %w", err)
	}
	return nil
}

func ReadFile(path string) (Record, error) {
	if strings.TrimSpace(path) == "" {
		return Record{}, fmt.Errorf("status path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("read install status: %w", err)
	}
	record, err := decodeRecord(data)
	if err != nil {
		return Record{}, fmt.Errorf("decode install status: %w", err)
	}
	return record, nil
}

func decodeRecord(data []byte) (Record, error) {
	if looksLikeEnvelope(data) {
		envelope, err := persistedrecord.DecodeEnvelope(data)
		if err != nil {
			return Record{}, err
		}
		if envelope.RecordType != RecordType || envelope.RecordVersion != recordVersion {
			return Record{}, fmt.Errorf("%w: %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion)
		}
		return persistedrecord.DecodePayload[Record](envelope)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func looksLikeEnvelope(data []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}
	_, ok := fields["recordType"]
	return ok
}

func RuntimeStatusPath(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("runtime root is required")
	}
	return filepath.Join(root, "var/lib/katl/install/status.json"), nil
}

func WriteRuntimeHandoff(root string, record Record) error {
	path, err := RuntimeStatusPath(root)
	if err != nil {
		return err
	}
	if err := ValidateRuntimeHandoff(record); err != nil {
		return err
	}
	if record.FinalHandoff == StateWaitingForClusterBootstrap {
		return nil
	}
	if err := ValidateCleanGenerationZero(root, record.InstalledGeneration); err != nil {
		return err
	}
	record.APIVersion = APIVersion
	record.Kind = Kind
	record.State = StateWaitingForClusterBootstrap
	record.FinalHandoff = StateWaitingForClusterBootstrap
	record.UpdatedAt = time.Now().UTC()
	return WriteFile(path, record)
}

func WriteRuntimeFailure(root string, record Record, cause error) error {
	path, err := RuntimeStatusPath(root)
	if err != nil {
		return err
	}
	record.APIVersion = APIVersion
	record.Kind = Kind
	record.State = StateRuntimeFailedNeedsRepair
	record.FinalHandoff = ""
	record.LastError = RedactError(cause)
	record.RefusalReason = record.LastError
	record.RetryHint = "repair install status before declaring boot-complete"
	record.UpdatedAt = time.Now().UTC()
	return WriteFile(path, record)
}

func ValidateRuntimeHandoff(record Record) error {
	var missing []string
	required := []struct {
		name  string
		value string
	}{
		{name: "inputMode", value: record.InputMode},
		{name: "inputSource", value: record.InputSource},
		{name: "requestDigest", value: record.RequestDigest},
		{name: "katlosImage.sha256", value: record.KatlosImage.SHA256},
		{name: "katlosImage.version", value: record.KatlosImage.Version},
		{name: "katlosImage.architecture", value: record.KatlosImage.Architecture},
		{name: "katlosImage.runtimeInterface", value: record.KatlosImage.RuntimeInterface},
		{name: "katlosImage.role", value: record.KatlosImage.Role},
		{name: "targetDiskStableID", value: record.TargetDiskStableID},
		{name: "selectedRootSlot", value: record.SelectedRootSlot},
		{name: "installedGeneration", value: record.InstalledGeneration},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("runtime handoff status missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func ValidateCleanGenerationZero(root string, generationID string) error {
	return validateCleanGenerationZero(root, generationID, "")
}

func ValidateCleanGenerationZeroForOperation(root string, generationID string, operationID string) error {
	return validateCleanGenerationZero(root, generationID, operationID)
}

func ValidateCleanPreBootstrapGeneration(root string, generationID string) error {
	return validateCleanPreBootstrapGeneration(root, generationID, "", false)
}

func ValidateCleanPreBootstrapGenerationForOperation(root string, generationID string, operationID string) error {
	return validateCleanPreBootstrapGeneration(root, generationID, operationID, false)
}

func validateCleanGenerationZero(root string, generationID string, allowedOperationID string) error {
	if strings.TrimSpace(generationID) != "0" {
		return nil
	}
	return validateCleanPreBootstrapGeneration(root, generationID, allowedOperationID, true)
}

func validateCleanPreBootstrapGeneration(root string, generationID string, allowedOperationID string, rejectAllMutationEvidence bool) error {
	generationID = strings.TrimSpace(generationID)
	if generationID == "" {
		return fmt.Errorf("current generation id is required")
	}
	generationLabel := "generation " + generationID
	spec, _, err := generation.ReadGeneration(root, generationID)
	if err != nil {
		return fmt.Errorf("read %s records: %w", generationLabel, err)
	}
	if hasKubernetesSysext(spec.Sysexts) {
		return fmt.Errorf("%s is not clean: selected Kubernetes sysexts are forbidden", generationLabel)
	}
	if err := validateKubernetesProjectionUnit(root, generationLabel); err != nil {
		return err
	}
	checks := []struct {
		path     string
		nonEmpty bool
		message  string
	}{
		{path: "/var/lib/katl/kubernetes/etc-kubernetes/pki", nonEmpty: true, message: "kubeadm PKI exists"},
		{path: "/var/lib/katl/kubernetes/etc-kubernetes/manifests", nonEmpty: true, message: "static pod manifests exist"},
		{path: "/var/lib/kubelet/pki", nonEmpty: true, message: "kubelet PKI exists"},
		{path: "/var/lib/etcd", nonEmpty: true, message: "stacked etcd data exists"},
		{path: "/etc/kubernetes", nonEmpty: true, message: "/etc/kubernetes is non-empty"},
		{path: "/var/lib/kubelet/bootstrap-kubeconfig", message: "kubelet bootstrap kubeconfig exists"},
		{path: "/var/lib/kubelet/kubeconfig", message: "kubelet kubeconfig exists"},
		{path: "/var/lib/kubelet/config.yaml", message: "kubelet config exists"},
		{path: "/etc/systemd/system/multi-user.target.wants/kubelet.service", message: "kubelet is enabled"},
	}
	for _, check := range checks {
		dirty, err := dirtyPath(root, check.path, check.nonEmpty)
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("%s is not clean: %s at %s", generationLabel, check.message, check.path)
		}
	}
	kubeconfigs, err := filepath.Glob(filepath.Join(filepath.Clean(root), "var/lib/katl/kubernetes/etc-kubernetes", "*.conf"))
	if err != nil {
		return fmt.Errorf("scan kubeadm kubeconfigs: %w", err)
	}
	if len(kubeconfigs) > 0 {
		return fmt.Errorf("%s is not clean: kubeadm kubeconfigs exist under /var/lib/katl/kubernetes/etc-kubernetes", generationLabel)
	}
	links, err := filepath.Glob(filepath.Join(filepath.Clean(root), "run/extensions", "*kubernetes*"))
	if err != nil {
		return fmt.Errorf("scan active Kubernetes sysext links: %w", err)
	}
	if len(links) > 0 {
		return fmt.Errorf("%s is not clean: active Kubernetes sysext links exist", generationLabel)
	}
	if err := validateNoDirtyPreBootstrapOperations(root, generationLabel, allowedOperationID, rejectAllMutationEvidence); err != nil {
		return err
	}
	return nil
}

func hasKubernetesSysext(refs []generation.ExtensionRef) bool {
	for _, ref := range refs {
		if ref.Name == "kubernetes" || filepath.Base(ref.ActivationPath) == "katl-kubernetes.raw" {
			return true
		}
	}
	return false
}

func validateKubernetesProjectionUnit(root string, generationLabel string) error {
	path := filepath.Join(filepath.Clean(root), "etc/systemd/system/etc-kubernetes.mount")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read /etc/systemd/system/etc-kubernetes.mount: %w", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	if values["What"] != generation.KubernetesSource || values["Where"] != generation.KubernetesTarget {
		return fmt.Errorf("%s is not clean: /etc/kubernetes projection points at %q -> %q, want %q -> %q", generationLabel, values["What"], values["Where"], generation.KubernetesSource, generation.KubernetesTarget)
	}
	return nil
}

func validateNoDirtyPreBootstrapOperations(root string, generationLabel string, allowedOperationID string, rejectAllMutationEvidence bool) error {
	store, err := operation.NewStore(filepath.Join(filepath.Clean(root), "var/lib/katl/operations"))
	if err != nil {
		return err
	}
	ids, err := store.OperationIDs()
	if err != nil {
		return fmt.Errorf("scan operation records: %w", err)
	}
	for _, id := range ids {
		record, err := store.Read(id)
		if err != nil {
			return fmt.Errorf("read operation %s: %w", id, err)
		}
		if !rejectAllMutationEvidence && !kubeadmStateOperation(record.OperationKind) && !containsKubeadmMutationScope(record.MutationScopes) {
			continue
		}
		if record.ExternalMutationStarted || record.MutatingToolRan || len(record.PreExecMutationMarkers) > 0 || len(record.MutationScopes) > 0 {
			return fmt.Errorf("%s is not clean: operation %s has mutation evidence", generationLabel, id)
		}
		if record.OperationID == allowedOperationID && record.Phase == "accepted" && record.PhaseIndex == 0 && len(record.CompletedPhases) == 0 {
			continue
		}
		class := operation.ClassifyStale(record)
		if class == operation.StalePostMutation || class == operation.StaleAmbiguous {
			return fmt.Errorf("%s is not clean: operation %s has stale mutation evidence (%s)", generationLabel, id, class)
		}
	}
	return nil
}

func kubeadmStateOperation(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "bootstrap-init", "bootstrap-join-worker", "bootstrap-join-control-plane", "kubeadm-upgrade":
		return true
	default:
		return false
	}
}

func containsKubeadmMutationScope(scopes []string) bool {
	for _, scope := range scopes {
		switch strings.TrimSpace(scope) {
		case "etc-kubernetes", "kubelet-state", "etcd-state", "cluster-objects", "kubeadm-state":
			return true
		}
	}
	return false
}

func dirtyPath(root string, absolutePath string, nonEmpty bool) (bool, error) {
	path := filepath.Join(filepath.Clean(root), strings.TrimPrefix(filepath.Clean(absolutePath), string(filepath.Separator)))
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", absolutePath, err)
	}
	if !nonEmpty {
		return true, nil
	}
	if !info.IsDir() {
		return true, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", absolutePath, err)
	}
	return len(entries) > 0, nil
}
