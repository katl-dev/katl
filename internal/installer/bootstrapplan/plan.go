package bootstrapplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
	installstatus "github.com/zariel/katl/internal/installer/status"
)

const (
	OperationKindInit             = "bootstrap-init"
	OperationKindJoinWorker       = "bootstrap-join-worker"
	OperationKindJoinControlPlane = "bootstrap-join-control-plane"

	defaultGenerationID = "0"
	operationScope      = "kubeadm-state"
)

type Request struct {
	Root        string
	StoreRoot   string
	OperationID string
	Kind        string
	Actor       string
	ClientID    string
	Now         time.Time
	Bootstrap   operation.BootstrapRequest
}

type Plan struct {
	Operation     operation.OperationRecord
	Previous      generation.GenerationSpec
	PreviousState generation.GenerationStatus
	RuntimeInputs RuntimeInputs
}

type RuntimeInputs struct {
	SelectedKubernetesSysext SelectedKubernetesSysext `json:"selectedKubernetesSysext"`
	HostConfig               HostConfig               `json:"hostConfig"`
	KubeadmInput             KubeadmInput             `json:"kubeadmInput"`
	KubernetesProjection     KubernetesProjection     `json:"kubernetesProjection"`
}

type SelectedKubernetesSysext struct {
	Path             string `json:"path"`
	SHA256           string `json:"sha256"`
	SizeBytes        uint64 `json:"sizeBytes"`
	PayloadVersion   string `json:"payloadVersion"`
	ActivationPath   string `json:"activationPath"`
	Architecture     string `json:"architecture"`
	RuntimeInterface string `json:"runtimeInterface"`
}

type HostConfig struct {
	NodeName             string `json:"nodeName"`
	SystemRole           string `json:"systemRole"`
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	NodeMetadataPath     string `json:"nodeMetadataPath"`
}

type KubeadmInput struct {
	ConfigRef string `json:"configRef"`
	Intent    string `json:"intent"`
	Path      string `json:"path"`
	Digest    string `json:"digest"`
}

type KubernetesProjection struct {
	What  string `json:"what"`
	Where string `json:"where"`
}

func Create(request Request) (Plan, error) {
	root := strings.TrimSpace(request.Root)
	if root == "" {
		return Plan{}, fmt.Errorf("runtime root is required")
	}
	kind := strings.TrimSpace(request.Kind)
	if kind == "" {
		return Plan{}, fmt.Errorf("bootstrap operation kind is required")
	}
	if kind == OperationKindJoinControlPlane {
		return Plan{}, fmt.Errorf("%s is not supported in day-one bootstrap planning", OperationKindJoinControlPlane)
	}
	if kind != OperationKindInit && kind != OperationKindJoinWorker {
		return Plan{}, fmt.Errorf("unsupported bootstrap operation kind %q", kind)
	}
	if err := installstatus.ValidateCleanGenerationZero(root, defaultGenerationID); err != nil {
		return Plan{}, err
	}
	intent, intentDigest, err := installer.ReadClusterIntent(root)
	if err != nil {
		return Plan{}, err
	}
	if err := validateIntent(intent); err != nil {
		return Plan{}, err
	}
	bootstrap := *cloneBootstrapRequest(request.Bootstrap)
	if err := validateRequest(kind, intent, bootstrap); err != nil {
		return Plan{}, err
	}
	previous, previousState, err := generation.ReadGeneration(root, defaultGenerationID)
	if err != nil {
		return Plan{}, fmt.Errorf("read generation 0 records: %w", err)
	}
	inputs, err := runtimeInputs(root, previous, intent, bootstrap)
	if err != nil {
		return Plan{}, err
	}

	storeRoot := strings.TrimSpace(request.StoreRoot)
	if storeRoot == "" {
		storeRoot = filepath.Join(filepath.Clean(root), "var/lib/katl/operations")
	}
	store, err := operation.NewStore(storeRoot)
	if err != nil {
		return Plan{}, err
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	record := operation.OperationRecord{
		OperationID:                 strings.TrimSpace(request.OperationID),
		OperationKind:               kind,
		Scope:                       operationScope,
		ClientRequestID:             strings.TrimSpace(request.ClientID),
		Actor:                       strings.TrimSpace(request.Actor),
		ExpectedCurrentGenerationID: defaultGenerationID,
		ExpectedClusterIntentDigest: intentDigest,
		RequestDigest:               requestDigest(kind, bootstrap),
		PhasePlan:                   phasePlan(kind),
		PreviousGenerationID:        defaultGenerationID,
		CandidateGenerationID:       bootstrap.CandidateGenerationID,
		BootstrapRequest:            &bootstrap,
		ActivationMode:              operation.ActivationModeLive,
		ActivationState:             operation.ActivationStatePending,
		GenerationCommitState:       operation.GenerationCommitCandidate,
		PostKubeadmHealthState:      operation.PostKubeadmHealthNotRun,
		Phase:                       "accepted",
		ResourceLocks:               []string{"generation:0", "kubeadm-state"},
		HostRollback:                "generation-0",
		PostMutationRollbackAllowed: false,
	}
	created, err := store.Create(record, "accepted", now)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Operation:     created,
		Previous:      previous,
		PreviousState: previousState,
		RuntimeInputs: inputs,
	}, nil
}

func FromOperation(root string, record operation.OperationRecord) (Plan, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Plan{}, fmt.Errorf("runtime root is required")
	}
	if err := installstatus.ValidateCleanGenerationZeroForOperation(root, defaultGenerationID, record.OperationID); err != nil {
		return Plan{}, err
	}
	if record.OperationKind == OperationKindJoinControlPlane {
		return Plan{}, fmt.Errorf("%s is not supported in day-one bootstrap planning", OperationKindJoinControlPlane)
	}
	if record.OperationKind != OperationKindInit && record.OperationKind != OperationKindJoinWorker {
		return Plan{}, fmt.Errorf("unsupported bootstrap operation kind %q", record.OperationKind)
	}
	intent, intentDigest, err := installer.ReadClusterIntent(root)
	if err != nil {
		return Plan{}, err
	}
	if strings.TrimSpace(record.ExpectedClusterIntentDigest) != "" && record.ExpectedClusterIntentDigest != intentDigest {
		return Plan{}, fmt.Errorf("operation expectedClusterIntentDigest does not match stored cluster intent")
	}
	if err := validateIntent(intent); err != nil {
		return Plan{}, err
	}
	if record.BootstrapRequest == nil {
		return Plan{}, fmt.Errorf("operation bootstrapRequest is required")
	}
	bootstrap := *cloneBootstrapRequest(*record.BootstrapRequest)
	if err := validateRequest(record.OperationKind, intent, bootstrap); err != nil {
		return Plan{}, err
	}
	previous, previousState, err := generation.ReadGeneration(root, defaultGenerationID)
	if err != nil {
		return Plan{}, fmt.Errorf("read generation 0 records: %w", err)
	}
	inputs, err := runtimeInputs(root, previous, intent, bootstrap)
	if err != nil {
		return Plan{}, err
	}
	record.BootstrapRequest = &bootstrap
	if strings.TrimSpace(record.CandidateGenerationID) == "" {
		record.CandidateGenerationID = bootstrap.CandidateGenerationID
	}
	return Plan{
		Operation:     record,
		Previous:      previous,
		PreviousState: previousState,
		RuntimeInputs: inputs,
	}, nil
}

func validateIntent(intent installer.ClusterIntent) error {
	if strings.TrimSpace(intent.GenerationID) != defaultGenerationID {
		return fmt.Errorf("cluster intent generationID must be %q", defaultGenerationID)
	}
	role := strings.TrimSpace(intent.SystemRole)
	switch role {
	case "control-plane", "worker":
	default:
		return fmt.Errorf("cluster intent systemRole must be control-plane or worker")
	}
	if strings.TrimSpace(intent.Inventory.NodeName) == "" {
		return fmt.Errorf("cluster intent inventory nodeName is required")
	}
	if strings.TrimSpace(intent.Kubernetes.PayloadVersion) == "" {
		return fmt.Errorf("cluster intent Kubernetes payloadVersion is required")
	}
	if intent.BootstrapProfile == nil || strings.TrimSpace(intent.BootstrapProfile.Ref) == "" {
		return fmt.Errorf("cluster intent bootstrapProfile ref is required")
	}
	if strings.TrimSpace(intent.BootstrapProfile.ResolvedID) == "" {
		return fmt.Errorf("cluster intent bootstrapProfile resolvedID is required")
	}
	if strings.TrimSpace(intent.BootstrapProfile.KubeadmConfigRef) == "" || strings.TrimSpace(intent.BootstrapProfile.KubeadmInputDigest) == "" {
		return fmt.Errorf("cluster intent bootstrapProfile kubeadm refs are required")
	}
	if intent.Kubeadm == nil || strings.TrimSpace(intent.Kubeadm.ConfigRef) == "" || strings.TrimSpace(intent.Kubeadm.ConfigPath) == "" || strings.TrimSpace(intent.Kubeadm.InputDigest) == "" {
		return fmt.Errorf("cluster intent kubeadm input is required")
	}
	if intent.Kubeadm.Intent != role {
		return fmt.Errorf("cluster intent kubeadm intent %q does not match systemRole %q", intent.Kubeadm.Intent, role)
	}
	if err := validateKubeadmPath(intent.Kubeadm.ConfigPath); err != nil {
		return err
	}
	if intent.BootstrapProfile.KubeadmConfigRef != "" && intent.BootstrapProfile.KubeadmConfigRef != intent.Kubeadm.ConfigRef {
		return fmt.Errorf("cluster intent bootstrapProfile kubeadmConfigRef %q does not match kubeadm configRef %q", intent.BootstrapProfile.KubeadmConfigRef, intent.Kubeadm.ConfigRef)
	}
	if intent.BootstrapProfile.KubeadmInputDigest != "" && intent.BootstrapProfile.KubeadmInputDigest != intent.Kubeadm.InputDigest {
		return fmt.Errorf("cluster intent bootstrapProfile kubeadmInputDigest does not match kubeadm inputDigest")
	}
	return nil
}

func validateKubeadmPath(path string) error {
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if path == "." || path == "/" || strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") {
		return fmt.Errorf("cluster intent kubeadm configPath must be clean")
	}
	if !strings.HasPrefix(path, "/etc/katl/kubeadm/") {
		return fmt.Errorf("cluster intent kubeadm configPath must be under /etc/katl/kubeadm")
	}
	if filepath.Ext(path) != ".yaml" {
		return fmt.Errorf("cluster intent kubeadm configPath must name a YAML file")
	}
	return nil
}

func validateRequest(kind string, intent installer.ClusterIntent, request operation.BootstrapRequest) error {
	if request.InventoryNodeName != intent.Inventory.NodeName {
		return fmt.Errorf("bootstrapRequest inventoryNodeName %q does not match stored intent %q", request.InventoryNodeName, intent.Inventory.NodeName)
	}
	if request.SystemRole != intent.SystemRole {
		return fmt.Errorf("bootstrapRequest systemRole %q does not match stored intent %q", request.SystemRole, intent.SystemRole)
	}
	if kind == OperationKindInit && request.SystemRole != "control-plane" {
		return fmt.Errorf("%s requires control-plane systemRole", OperationKindInit)
	}
	if kind == OperationKindJoinWorker && request.SystemRole != "worker" {
		return fmt.Errorf("%s requires worker systemRole", OperationKindJoinWorker)
	}
	if request.KubernetesPayloadVersion != intent.Kubernetes.PayloadVersion {
		return fmt.Errorf("bootstrapRequest kubernetesPayloadVersion %q does not match stored intent %q", request.KubernetesPayloadVersion, intent.Kubernetes.PayloadVersion)
	}
	if request.BootstrapProfileRef != intent.BootstrapProfile.Ref {
		return fmt.Errorf("bootstrapRequest bootstrapProfileRef %q does not match stored intent %q", request.BootstrapProfileRef, intent.BootstrapProfile.Ref)
	}
	if strings.TrimSpace(request.KubeadmInputDigest) != "" && request.KubeadmInputDigest != intent.Kubeadm.InputDigest {
		return fmt.Errorf("bootstrapRequest kubeadmInputDigest does not match stored intent")
	}
	candidate := strings.TrimSpace(request.CandidateGenerationID)
	if candidate == "" || candidate == defaultGenerationID {
		return fmt.Errorf("bootstrapRequest candidateGenerationID must name a non-zero candidate generation")
	}
	if kind == OperationKindJoinWorker && strings.TrimSpace(request.JoinMaterialRef) == "" {
		return fmt.Errorf("%s requires joinMaterialRef", OperationKindJoinWorker)
	}
	return operation.ValidateRecord(operation.OperationRecord{
		APIVersion:       operation.APIVersion,
		Kind:             operation.RecordKind,
		SchemaVersion:    operation.SchemaVersion,
		OperationID:      "validate-bootstrap-request",
		OperationKind:    kind,
		Scope:            operationScope,
		RequestDigest:    requestDigest(kind, request),
		RecordRevision:   1,
		LatestJournalSeq: 1,
		BootstrapRequest: &request,
		CreatedAt:        time.Unix(1, 0).UTC(),
		UpdatedAt:        time.Unix(1, 0).UTC(),
	})
}

func runtimeInputs(root string, previous generation.GenerationSpec, intent installer.ClusterIntent, request operation.BootstrapRequest) (RuntimeInputs, error) {
	selected, err := selectBundledSysext(root, previous, intent.Kubernetes)
	if err != nil {
		return RuntimeInputs{}, err
	}
	return RuntimeInputs{
		SelectedKubernetesSysext: selected,
		HostConfig: HostConfig{
			NodeName:             request.InventoryNodeName,
			SystemRole:           request.SystemRole,
			ControlPlaneEndpoint: firstNonEmpty(request.ControlPlaneEndpoint, intent.Inventory.ControlPlaneEndpoint),
			NodeMetadataPath:     "/etc/katl/node.json",
		},
		KubeadmInput: KubeadmInput{
			ConfigRef: intent.Kubeadm.ConfigRef,
			Intent:    intent.Kubeadm.Intent,
			Path:      intent.Kubeadm.ConfigPath,
			Digest:    intent.Kubeadm.InputDigest,
		},
		KubernetesProjection: KubernetesProjection{
			What:  generation.KubernetesSource,
			Where: generation.KubernetesTarget,
		},
	}, nil
}

func selectBundledSysext(root string, previous generation.GenerationSpec, kubernetes installer.ClusterIntentKubernetes) (SelectedKubernetesSysext, error) {
	if strings.TrimSpace(kubernetes.SysextPath) == "" {
		return SelectedKubernetesSysext{}, fmt.Errorf("cluster intent Kubernetes sysextPath is required")
	}
	if strings.TrimSpace(kubernetes.SysextSHA256) == "" {
		return SelectedKubernetesSysext{}, fmt.Errorf("cluster intent Kubernetes sysextSHA256 is required")
	}
	root = filepath.Clean(root)
	intentPath, err := cleanBundledSysextPath(kubernetes.SysextPath)
	if err != nil {
		return SelectedKubernetesSysext{}, err
	}
	path := filepath.Join(root, strings.TrimPrefix(intentPath, string(filepath.Separator)))
	info, err := os.Stat(path)
	if err != nil {
		return SelectedKubernetesSysext{}, fmt.Errorf("inspect bundled Kubernetes sysext: %w", err)
	}
	if info.IsDir() {
		return SelectedKubernetesSysext{}, fmt.Errorf("bundled Kubernetes sysext is a directory")
	}
	if kubernetes.SysextSize > 0 && uint64(info.Size()) != kubernetes.SysextSize {
		return SelectedKubernetesSysext{}, fmt.Errorf("bundled Kubernetes sysext size %d does not match intent %d", info.Size(), kubernetes.SysextSize)
	}
	got, err := fileSHA256(path)
	if err != nil {
		return SelectedKubernetesSysext{}, err
	}
	if got != strings.ToLower(kubernetes.SysextSHA256) {
		return SelectedKubernetesSysext{}, fmt.Errorf("bundled Kubernetes sysext SHA-256 does not match intent")
	}
	return SelectedKubernetesSysext{
		Path:             intentPath,
		SHA256:           strings.ToLower(kubernetes.SysextSHA256),
		SizeBytes:        uint64(info.Size()),
		PayloadVersion:   kubernetes.PayloadVersion,
		ActivationPath:   "/run/extensions/kubernetes.raw",
		Architecture:     previous.Root.Architecture,
		RuntimeInterface: previous.Root.RuntimeInterface,
	}, nil
}

func cleanBundledSysextPath(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" {
		return "", fmt.Errorf("cluster intent Kubernetes sysextPath is required")
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("cluster intent Kubernetes sysextPath must be absolute")
	}
	cleaned := pathClean(value)
	if cleaned != value || strings.Contains(cleaned, "/../") || strings.HasSuffix(cleaned, "/..") {
		return "", fmt.Errorf("cluster intent Kubernetes sysextPath must be clean")
	}
	if !strings.HasPrefix(cleaned, "/var/lib/katl/artifacts/katlos-image/") {
		return "", fmt.Errorf("cluster intent Kubernetes sysextPath must be under /var/lib/katl/artifacts/katlos-image")
	}
	if filepath.Base(cleaned) != "katl-kubernetes.raw" {
		return "", fmt.Errorf("cluster intent Kubernetes sysextPath must name katl-kubernetes.raw")
	}
	return cleaned, nil
}

func pathClean(value string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open bundled Kubernetes sysext: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash bundled Kubernetes sysext: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func requestDigest(kind string, request operation.BootstrapRequest) string {
	normalized := *cloneBootstrapRequest(request)
	payload := struct {
		Kind    string                     `json:"kind"`
		Request operation.BootstrapRequest `json:"request"`
	}{
		Kind:    strings.TrimSpace(kind),
		Request: normalized,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func phasePlan(kind string) []string {
	phase := "kubeadm-init"
	if kind == OperationKindJoinWorker {
		phase = "kubeadm-join-worker"
	}
	return []string{
		"accepted",
		"prepare-bootstrap-runtime",
		"activate-bootstrap-runtime",
		phase,
		"post-kubeadm-health",
		operation.HostBookkeepingCompletionPhase,
	}
}

func cloneBootstrapRequest(request operation.BootstrapRequest) *operation.BootstrapRequest {
	return &operation.BootstrapRequest{
		InventoryNodeName:        strings.TrimSpace(request.InventoryNodeName),
		SystemRole:               strings.TrimSpace(request.SystemRole),
		KubernetesPayloadVersion: strings.TrimSpace(request.KubernetesPayloadVersion),
		BootstrapProfileRef:      strings.TrimSpace(request.BootstrapProfileRef),
		ControlPlaneEndpoint:     strings.TrimSpace(request.ControlPlaneEndpoint),
		StableEndpoint:           strings.TrimSpace(request.StableEndpoint),
		CandidateGenerationID:    strings.TrimSpace(request.CandidateGenerationID),
		KubeadmInputDigest:       strings.TrimSpace(request.KubeadmInputDigest),
		JoinMaterialRef:          strings.TrimSpace(request.JoinMaterialRef),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
