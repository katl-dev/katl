package bootstrapplan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

const (
	OperationKindInit             = "bootstrap-init"
	OperationKindJoinWorker       = "bootstrap-join-worker"
	OperationKindJoinControlPlane = "bootstrap-join-control-plane"

	defaultGenerationID = "0"
	operationScope      = "kubeadm-state"
)

type Request struct {
	Root                        string
	StoreRoot                   string
	OperationID                 string
	Kind                        string
	Actor                       string
	ClientID                    string
	ExpectedCurrentGenerationID string
	Now                         time.Time
	Bootstrap                   operation.BootstrapRequest
	BundleClient                *http.Client
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
	Path                 string `json:"path"`
	SHA256               string `json:"sha256"`
	SizeBytes            uint64 `json:"sizeBytes"`
	PayloadVersion       string `json:"payloadVersion"`
	ActivationPath       string `json:"activationPath"`
	Architecture         string `json:"architecture"`
	RuntimeInterface     string `json:"runtimeInterface"`
	ArtifactVersion      string `json:"artifactVersion,omitempty"`
	BundleSource         string `json:"bundleSource,omitempty"`
	BundleRef            string `json:"bundleRef,omitempty"`
	BundleManifestDigest string `json:"bundleManifestDigest,omitempty"`
	SysextPayloadDigest  string `json:"sysextPayloadDigest,omitempty"`
}

type HostConfig struct {
	NodeName                string `json:"nodeName"`
	SystemRole              string `json:"systemRole"`
	ControlPlaneEndpoint    string `json:"controlPlaneEndpoint,omitempty"`
	ControlPlaneEndpointVIP string `json:"controlPlaneEndpointVIP,omitempty"`
	NodeMetadataPath        string `json:"nodeMetadataPath"`
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
	if kind != OperationKindInit && kind != OperationKindJoinWorker && kind != OperationKindJoinControlPlane {
		return Plan{}, fmt.Errorf("unsupported bootstrap operation kind %q", kind)
	}
	currentGenerationID := valueOrDefault(request.ExpectedCurrentGenerationID, defaultGenerationID)
	if err := installstatus.ValidateCleanPreBootstrapGeneration(root, currentGenerationID); err != nil {
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
	previous, previousState, err := generation.ReadGeneration(root, currentGenerationID)
	if err != nil {
		return Plan{}, fmt.Errorf("read current generation %s records: %w", currentGenerationID, err)
	}
	inputs, err := runtimeInputs(root, previous, intent, bootstrap, request.BundleClient)
	if err != nil {
		return Plan{}, err
	}
	bootstrap.KubernetesBundleManifestDigest = inputs.SelectedKubernetesSysext.BundleManifestDigest
	bootstrap.KubernetesSysextPayloadDigest = inputs.SelectedKubernetesSysext.SysextPayloadDigest

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
		ExpectedCurrentGenerationID: currentGenerationID,
		ExpectedClusterIntentDigest: intentDigest,
		RequestDigest:               requestDigest(kind, bootstrap),
		PhasePlan:                   phasePlan(kind),
		PreviousGenerationID:        currentGenerationID,
		CandidateGenerationID:       bootstrap.CandidateGenerationID,
		BootstrapRequest:            &bootstrap,
		ActivationMode:              operation.ActivationModeLive,
		ActivationState:             operation.ActivationStatePending,
		GenerationCommitState:       operation.GenerationCommitCandidate,
		PostKubeadmHealthState:      operation.PostKubeadmHealthNotRun,
		Phase:                       "accepted",
		ResourceLocks:               []string{"generation:" + currentGenerationID, "kubeadm-state"},
		HostRollback:                currentGenerationID,
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
	return fromOperation(root, record, nil)
}

func FromOperationWithBundleClient(root string, record operation.OperationRecord, client *http.Client) (Plan, error) {
	return fromOperation(root, record, client)
}

func valueOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func fromOperation(root string, record operation.OperationRecord, client *http.Client) (Plan, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Plan{}, fmt.Errorf("runtime root is required")
	}
	currentGenerationID := valueOrDefault(record.ExpectedCurrentGenerationID, record.PreviousGenerationID)
	currentGenerationID = valueOrDefault(currentGenerationID, defaultGenerationID)
	if err := installstatus.ValidateCleanPreBootstrapGenerationForOperation(root, currentGenerationID, record.OperationID); err != nil {
		return Plan{}, err
	}
	if record.OperationKind != OperationKindInit && record.OperationKind != OperationKindJoinWorker && record.OperationKind != OperationKindJoinControlPlane {
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
	previous, previousState, err := generation.ReadGeneration(root, currentGenerationID)
	if err != nil {
		return Plan{}, fmt.Errorf("read current generation %s records: %w", currentGenerationID, err)
	}
	inputs, err := runtimeInputs(root, previous, intent, bootstrap, client)
	if err != nil {
		return Plan{}, err
	}
	record.BootstrapRequest = &bootstrap
	record.ExpectedCurrentGenerationID = currentGenerationID
	record.PreviousGenerationID = currentGenerationID
	record.HostRollback = currentGenerationID
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
	if kind == OperationKindJoinControlPlane && request.SystemRole != "control-plane" {
		return fmt.Errorf("%s requires control-plane systemRole", OperationKindJoinControlPlane)
	}
	if strings.TrimSpace(intent.Kubernetes.PayloadVersion) != "" && request.KubernetesPayloadVersion != intent.Kubernetes.PayloadVersion {
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
	if isJoinOperation(kind) && strings.TrimSpace(request.JoinMaterialRef) == "" {
		return fmt.Errorf("%s requires joinMaterialRef", kind)
	}
	if isJoinOperation(kind) && strings.TrimSpace(request.JoinMaterialDigest) == "" {
		return fmt.Errorf("%s requires joinMaterialDigest", kind)
	}
	if isJoinOperation(kind) && strings.TrimSpace(request.TemporaryJoinConfigPath) == "" {
		return fmt.Errorf("%s requires temporaryJoinConfigPath", kind)
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

func runtimeInputs(root string, previous generation.GenerationSpec, intent installer.ClusterIntent, request operation.BootstrapRequest, client *http.Client) (RuntimeInputs, error) {
	selected, err := selectKubernetesSysext(root, previous, intent.Kubernetes, request, client)
	if err != nil {
		return RuntimeInputs{}, err
	}
	return RuntimeInputs{
		SelectedKubernetesSysext: selected,
		HostConfig: HostConfig{
			NodeName:                request.InventoryNodeName,
			SystemRole:              request.SystemRole,
			ControlPlaneEndpoint:    firstNonEmpty(request.ControlPlaneEndpoint, intent.Inventory.ControlPlaneEndpoint),
			ControlPlaneEndpointVIP: intent.Inventory.ControlPlaneEndpointVIP,
			NodeMetadataPath:        "/etc/katl/node.json",
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

func selectKubernetesSysext(root string, previous generation.GenerationSpec, _ installer.ClusterIntentKubernetes, request operation.BootstrapRequest, client *http.Client) (SelectedKubernetesSysext, error) {
	if strings.TrimSpace(request.KubernetesBundleSource) == "" && strings.TrimSpace(request.KubernetesBundleRef) == "" {
		return SelectedKubernetesSysext{}, fmt.Errorf("bootstrapRequest kubernetesBundleSource and kubernetesBundleRef are required")
	}
	return selectFetchedBundleSysext(root, previous, request, client)
}

func selectFetchedBundleSysext(root string, previous generation.GenerationSpec, request operation.BootstrapRequest, client *http.Client) (SelectedKubernetesSysext, error) {
	payloadVersion, err := kubernetesbundle.PayloadVersionFromRef(request.KubernetesBundleRef)
	if err != nil {
		return SelectedKubernetesSysext{}, fmt.Errorf("bootstrapRequest kubernetesBundleRef: %w", err)
	}
	if payloadVersion != request.KubernetesPayloadVersion {
		return SelectedKubernetesSysext{}, fmt.Errorf("bootstrapRequest kubernetesBundleRef payload version %q does not match kubernetesPayloadVersion %q", payloadVersion, request.KubernetesPayloadVersion)
	}
	cacheDir := filepath.Join(filepath.Clean(root), "var/lib/katl/artifacts/kubernetes-bundles")
	staged, err := kubernetesbundle.FetchAndStage(context.Background(), kubernetesbundle.Request{
		Source:           request.KubernetesBundleSource,
		Ref:              request.KubernetesBundleRef,
		CacheDir:         cacheDir,
		RuntimeInterface: previous.Root.RuntimeInterface,
		Architecture:     previous.Root.Architecture,
		Client:           client,
		ActivationPath:   "/run/extensions/katl-kubernetes.raw",
	})
	if err != nil {
		return SelectedKubernetesSysext{}, err
	}
	if expected := strings.TrimSpace(request.KubernetesBundleManifestDigest); expected != "" && staged.BundleManifestDigest != expected {
		return SelectedKubernetesSysext{}, fmt.Errorf("staged Kubernetes bundle manifest digest %s does not match operation record %s", staged.BundleManifestDigest, expected)
	}
	if expected := strings.TrimSpace(request.KubernetesSysextPayloadDigest); expected != "" && staged.SysextPayloadDigest != expected {
		return SelectedKubernetesSysext{}, fmt.Errorf("staged Kubernetes sysext payload digest %s does not match operation record %s", staged.SysextPayloadDigest, expected)
	}
	runtimePath, err := runtimeRootPath(root, staged.SysextPath)
	if err != nil {
		return SelectedKubernetesSysext{}, err
	}
	info, err := os.Stat(staged.SysextPath)
	if err != nil {
		return SelectedKubernetesSysext{}, fmt.Errorf("inspect staged Kubernetes sysext: %w", err)
	}
	return SelectedKubernetesSysext{
		Path:                 runtimePath,
		SHA256:               staged.ExtensionRef.SHA256,
		SizeBytes:            uint64(info.Size()),
		PayloadVersion:       staged.PayloadVersion,
		ActivationPath:       staged.ExtensionRef.ActivationPath,
		Architecture:         staged.Architecture,
		RuntimeInterface:     previous.Root.RuntimeInterface,
		ArtifactVersion:      staged.ArtifactVersion,
		BundleSource:         strings.TrimSpace(request.KubernetesBundleSource),
		BundleRef:            strings.TrimSpace(request.KubernetesBundleRef),
		BundleManifestDigest: staged.BundleManifestDigest,
		SysextPayloadDigest:  staged.SysextPayloadDigest,
	}, nil
}

func runtimeRootPath(root string, path string) (string, error) {
	root = filepath.Clean(root)
	path = filepath.Clean(strings.TrimSpace(path))
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("staged Kubernetes sysext path %q is outside runtime root", path)
	}
	return "/" + filepath.ToSlash(rel), nil
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
	} else if kind == OperationKindJoinControlPlane {
		phase = "kubeadm-join-control-plane"
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

func isJoinOperation(kind string) bool {
	return kind == OperationKindJoinWorker || kind == OperationKindJoinControlPlane
}

func cloneBootstrapRequest(request operation.BootstrapRequest) *operation.BootstrapRequest {
	return &operation.BootstrapRequest{
		InventoryNodeName:              strings.TrimSpace(request.InventoryNodeName),
		SystemRole:                     strings.TrimSpace(request.SystemRole),
		KubernetesPayloadVersion:       strings.TrimSpace(request.KubernetesPayloadVersion),
		KubernetesBundleSource:         strings.TrimSpace(request.KubernetesBundleSource),
		KubernetesBundleRef:            strings.TrimSpace(request.KubernetesBundleRef),
		KubernetesBundleManifestDigest: strings.TrimSpace(request.KubernetesBundleManifestDigest),
		KubernetesSysextPayloadDigest:  strings.TrimSpace(request.KubernetesSysextPayloadDigest),
		BootstrapProfileRef:            strings.TrimSpace(request.BootstrapProfileRef),
		ControlPlaneEndpoint:           strings.TrimSpace(request.ControlPlaneEndpoint),
		StableEndpoint:                 strings.TrimSpace(request.StableEndpoint),
		CandidateGenerationID:          strings.TrimSpace(request.CandidateGenerationID),
		KubeadmInputDigest:             strings.TrimSpace(request.KubeadmInputDigest),
		JoinMaterialRef:                strings.TrimSpace(request.JoinMaterialRef),
		JoinMaterialDigest:             strings.TrimSpace(request.JoinMaterialDigest),
		JoinMaterialExpiresAt:          strings.TrimSpace(request.JoinMaterialExpiresAt),
		TemporaryJoinConfigPath:        strings.TrimSpace(request.TemporaryJoinConfigPath),
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
