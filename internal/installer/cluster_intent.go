package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Kubernetes         ClusterIntentKubernetes `json:"kubernetes"`
	Kubeadm            *ClusterIntentKubeadm   `json:"kubeadm,omitempty"`
	KatlosImage        manifest.KatlosImage    `json:"katlosImage"`
	RequestDigest      string                  `json:"requestDigest,omitempty"`
	InstalledAt        time.Time               `json:"installedAt"`
	TargetDiskStableID string                  `json:"targetDiskStableID,omitempty"`
}

type ClusterIntentIdentity struct {
	Hostname string `json:"hostname"`
}

type ClusterIntentKubernetes struct {
	PayloadVersion string `json:"payloadVersion,omitempty"`
	SysextPath     string `json:"sysextPath,omitempty"`
	SysextSHA256   string `json:"sysextSHA256,omitempty"`
	SysextSize     uint64 `json:"sysextSizeBytes,omitempty"`
}

type ClusterIntentKubeadm struct {
	ConfigRef string `json:"configRef,omitempty"`
	Intent    string `json:"intent,omitempty"`
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
		Kubernetes: ClusterIntentKubernetes{
			PayloadVersion: strings.TrimSpace(request.KubernetesVersion),
		},
		KatlosImage:        request.Manifest.KatlosImage,
		RequestDigest:      strings.TrimSpace(request.RequestDigest),
		InstalledAt:        installedAt.UTC(),
		TargetDiskStableID: strings.TrimSpace(request.TargetDiskStableID),
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
	intent.Kubeadm = &ClusterIntentKubeadm{
		ConfigRef: ref,
		Intent:    intentValue,
	}
	return intent, nil
}
