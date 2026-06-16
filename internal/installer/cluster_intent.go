package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer/configdomain"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
)

const (
	ClusterIntentAPIVersion = "katl.dev/v1alpha1"
	ClusterIntentKind       = "ClusterIntent"
)

type ClusterIntent struct {
	APIVersion         string                  `json:"apiVersion"`
	Kind               string                  `json:"kind"`
	GenerationID       string                  `json:"generationID"`
	SystemRole         string                  `json:"systemRole"`
	Identity           ClusterIntentIdentity   `json:"identity"`
	Inventory          ClusterIntentInventory  `json:"inventory"`
	BootstrapProfile   *ClusterIntentProfile   `json:"bootstrapProfile,omitempty"`
	Kubernetes         ClusterIntentKubernetes `json:"kubernetes"`
	Kubeadm            *ClusterIntentKubeadm   `json:"kubeadm,omitempty"`
	KatlosImage        manifest.KatlosImage    `json:"katlosImage"`
	Source             ClusterIntentSource     `json:"source"`
	RequestDigest      string                  `json:"requestDigest,omitempty"`
	InstalledAt        time.Time               `json:"installedAt"`
	TargetDiskStableID string                  `json:"targetDiskStableID,omitempty"`
}

type ClusterIntentIdentity struct {
	Hostname string `json:"hostname"`
}

type ClusterIntentInventory struct {
	ClusterName          string                   `json:"clusterName,omitempty"`
	NodeName             string                   `json:"nodeName"`
	NodeAddress          string                   `json:"nodeAddress,omitempty"`
	ControlPlaneEndpoint string                   `json:"controlPlaneEndpoint,omitempty"`
	Access               manifest.BootstrapAccess `json:"access,omitempty"`
	Labels               map[string]string        `json:"labels,omitempty"`
	Taints               []manifest.NodeTaint     `json:"taints,omitempty"`
}

type ClusterIntentKubernetes struct {
	PayloadVersion string `json:"payloadVersion,omitempty"`
	CatalogRef     string `json:"catalogRef,omitempty"`
	SysextPath     string `json:"sysextPath,omitempty"`
	SysextSHA256   string `json:"sysextSHA256,omitempty"`
	SysextSize     uint64 `json:"sysextSizeBytes,omitempty"`
}

type ClusterIntentProfile struct {
	Ref                string `json:"ref"`
	ResolvedID         string `json:"resolvedID,omitempty"`
	KubeadmConfigRef   string `json:"kubeadmConfigRef,omitempty"`
	KubeadmInputDigest string `json:"kubeadmInputDigest,omitempty"`
}

type ClusterIntentKubeadm struct {
	ConfigRef   string `json:"configRef,omitempty"`
	ConfigPath  string `json:"configPath,omitempty"`
	Intent      string `json:"intent,omitempty"`
	InputDigest string `json:"inputDigest,omitempty"`
}

type ClusterIntentSource struct {
	RequestDigest     string `json:"requestDigest,omitempty"`
	KatlosImageSHA256 string `json:"katlosImageSHA256,omitempty"`
}

type ClusterIntentRequest struct {
	TargetRoot         string
	Manifest           manifest.Manifest
	KubeadmConfigs     map[string]kubeadmconfig.Plan
	KubernetesVersion  string
	KubernetesSysext   *ClusterIntentKubernetesSysext
	GenerationID       string
	RequestDigest      string
	InstalledAt        time.Time
	TargetDiskStableID string
}

type ClusterIntentKubernetesSysext struct {
	Path      string
	SHA256    string
	SizeBytes uint64
}

func WriteClusterIntent(request ClusterIntentRequest) (string, error) {
	if strings.TrimSpace(request.TargetRoot) == "" {
		return "", fmt.Errorf("target root is required")
	}
	intent, err := BuildClusterIntent(request)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal cluster intent: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(filepath.Clean(request.TargetRoot), "var/lib/katl/cluster/intent.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("create cluster intent directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write cluster intent: %w", err)
	}
	return path, nil
}

func ReadClusterIntent(root string) (ClusterIntent, string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return ClusterIntent{}, "", fmt.Errorf("runtime root is required")
	}
	path := filepath.Join(filepath.Clean(root), "var/lib/katl/cluster/intent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ClusterIntent{}, "", fmt.Errorf("read cluster intent: %w", err)
	}
	var intent ClusterIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return ClusterIntent{}, "", fmt.Errorf("decode cluster intent: %w", err)
	}
	if intent.APIVersion != ClusterIntentAPIVersion {
		return ClusterIntent{}, "", fmt.Errorf("cluster intent apiVersion must be %q", ClusterIntentAPIVersion)
	}
	if intent.Kind != ClusterIntentKind {
		return ClusterIntent{}, "", fmt.Errorf("cluster intent kind must be %q", ClusterIntentKind)
	}
	return intent, digestBytes(data), nil
}

func BuildClusterIntent(request ClusterIntentRequest) (ClusterIntent, error) {
	if strings.TrimSpace(request.GenerationID) == "" {
		return ClusterIntent{}, fmt.Errorf("generation id is required")
	}
	installedAt := request.InstalledAt
	if installedAt.IsZero() {
		installedAt = time.Now().UTC()
	}
	intent := ClusterIntent{
		APIVersion:   ClusterIntentAPIVersion,
		Kind:         ClusterIntentKind,
		GenerationID: strings.TrimSpace(request.GenerationID),
		SystemRole:   request.Manifest.Node.SystemRole,
		Identity: ClusterIntentIdentity{
			Hostname: request.Manifest.Node.Identity.Hostname,
		},
		Inventory: ClusterIntentInventory{
			NodeName: inventoryNodeName(request.Manifest),
		},
		Kubernetes: ClusterIntentKubernetes{
			PayloadVersion: strings.TrimSpace(request.KubernetesVersion),
		},
		KatlosImage:        request.Manifest.KatlosImage,
		Source:             ClusterIntentSource{RequestDigest: strings.TrimSpace(request.RequestDigest), KatlosImageSHA256: strings.TrimSpace(request.Manifest.KatlosImage.SHA256)},
		RequestDigest:      strings.TrimSpace(request.RequestDigest),
		InstalledAt:        installedAt.UTC(),
		TargetDiskStableID: strings.TrimSpace(request.TargetDiskStableID),
	}
	bootstrap := request.Manifest.Node.Bootstrap
	if bootstrap != nil {
		intent.Inventory.ClusterName = strings.TrimSpace(bootstrap.ClusterName)
		intent.Inventory.NodeAddress = strings.TrimSpace(bootstrap.NodeAddress)
		intent.Inventory.ControlPlaneEndpoint = strings.TrimSpace(bootstrap.ControlPlaneEndpoint)
		intent.Inventory.Access = trimBootstrapAccess(bootstrap.Access)
		intent.Inventory.Labels = copyBootstrapLabels(bootstrap.Labels)
		intent.Inventory.Taints = append([]manifest.NodeTaint(nil), bootstrap.Taints...)
		intent.Kubernetes.CatalogRef = strings.TrimSpace(bootstrap.KubernetesCatalogRef)
	}
	if request.KubernetesSysext != nil {
		intent.Kubernetes.SysextPath = strings.TrimSpace(request.KubernetesSysext.Path)
		intent.Kubernetes.SysextSHA256 = strings.TrimSpace(request.KubernetesSysext.SHA256)
		intent.Kubernetes.SysextSize = request.KubernetesSysext.SizeBytes
	}
	ref := strings.TrimSpace(request.Manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	if ref == "" {
		return intent, nil
	}
	config, ok := request.KubeadmConfigs[ref]
	if !ok {
		return ClusterIntent{}, fmt.Errorf("node.kubernetes.kubeadm.configRef %q was not resolved", ref)
	}
	intentValue, err := configdomain.KubeadmIntent(config)
	if err != nil {
		return ClusterIntent{}, err
	}
	inputDigest := digestKubeadmPlan(config)
	profileRef := ref
	resolvedID := "kubeadm:" + ref
	if bootstrap != nil {
		if candidate := strings.TrimSpace(bootstrap.BootstrapProfileRef); candidate != "" {
			profileRef = candidate
		}
		if candidate := strings.TrimSpace(bootstrap.ProfileResolvedID); candidate != "" {
			resolvedID = candidate
		}
	}
	intent.BootstrapProfile = &ClusterIntentProfile{
		Ref:                profileRef,
		ResolvedID:         resolvedID,
		KubeadmConfigRef:   ref,
		KubeadmInputDigest: inputDigest,
	}
	intent.Kubeadm = &ClusterIntentKubeadm{
		ConfigRef:   ref,
		ConfigPath:  config.Config.RenderPath,
		Intent:      intentValue,
		InputDigest: inputDigest,
	}
	return intent, nil
}

func digestKubeadmPlan(plan kubeadmconfig.Plan) string {
	hash := sha256.New()
	writeDigestFile(hash, "config", plan.Config)
	patches := append([]kubeadmconfig.File(nil), plan.Patches...)
	sort.Slice(patches, func(i, j int) bool { return patches[i].RenderPath < patches[j].RenderPath })
	for _, patch := range patches {
		writeDigestFile(hash, "patch", patch)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeDigestFile(hash interface{ Write([]byte) (int, error) }, kind string, file kubeadmconfig.File) {
	fmt.Fprintf(hash, "%s\x00%s\x00%#o\x00", kind, file.RenderPath, file.Mode)
	hash.Write(file.Content)
	hash.Write([]byte{0})
}

func inventoryNodeName(m manifest.Manifest) string {
	if m.Node.Bootstrap != nil {
		name := strings.TrimSpace(m.Node.Bootstrap.InventoryNodeName)
		if name != "" {
			return name
		}
	}
	return strings.TrimSpace(m.Node.Identity.Hostname)
}

func trimBootstrapAccess(access manifest.BootstrapAccess) manifest.BootstrapAccess {
	return manifest.BootstrapAccess{
		Method:        strings.TrimSpace(access.Method),
		User:          strings.TrimSpace(access.User),
		CredentialRef: strings.TrimSpace(access.CredentialRef),
	}
}

func copyBootstrapLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}
