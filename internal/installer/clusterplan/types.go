package clusterplan

import (
	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/confext"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
	"github.com/zariel/katl/internal/installer/platformendpoint"
	"github.com/zariel/katl/internal/installer/sysextcatalog"
)

const (
	APIVersion = "cluster.katl.dev/v1alpha1"
	Kind       = "ClusterPlan"
)

type Config struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type Spec struct {
	ControlPlaneEndpoint string                             `yaml:"controlPlaneEndpoint,omitempty" json:"controlPlaneEndpoint,omitempty"`
	PlatformAPIEndpoint  *platformendpoint.Config           `yaml:"platformAPIEndpoint,omitempty" json:"platformAPIEndpoint,omitempty"`
	Kubernetes           KubernetesSelection                `yaml:"kubernetes" json:"kubernetes"`
	KatlosImage          manifest.KatlosImage               `yaml:"katlosImage" json:"katlosImage"`
	WipeTarget           bool                               `yaml:"wipeTarget" json:"wipeTarget"`
	Defaults             NodeLayer                          `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	SystemRoleDefaults   map[inventory.SystemRole]NodeLayer `yaml:"systemRoleDefaults,omitempty" json:"systemRoleDefaults,omitempty"`
	Nodes                []Node                             `yaml:"nodes" json:"nodes"`
}

type KubernetesSelection struct {
	PayloadVersion string `yaml:"payloadVersion,omitempty" json:"payloadVersion,omitempty"`
	CatalogRef     string `yaml:"catalogRef,omitempty" json:"catalogRef,omitempty"`
	BundleSource   string `yaml:"bundleSource,omitempty" json:"bundleSource,omitempty"`
	BundleRef      string `yaml:"bundleRef,omitempty" json:"bundleRef,omitempty"`
}

type Node struct {
	Name       string               `yaml:"name" json:"name"`
	SystemRole inventory.SystemRole `yaml:"systemRole" json:"systemRole"`
	Overrides  NodeLayer            `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

type NodeLayer struct {
	Hostname   string                  `yaml:"hostname,omitempty" json:"hostname,omitempty"`
	SSH        manifest.SSHIdentity    `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	Networkd   manifest.NetworkdConfig `yaml:"networkd,omitempty" json:"networkd,omitempty"`
	Install    InstallLayer            `yaml:"install,omitempty" json:"install,omitempty"`
	Kubernetes KubernetesLayer         `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`
	Bootstrap  BootstrapLayer          `yaml:"bootstrap,omitempty" json:"bootstrap,omitempty"`
}

type InstallLayer struct {
	TargetDisk *manifest.DiskSelector `yaml:"targetDisk,omitempty" json:"targetDisk,omitempty"`
	ExtraDisks []manifest.ExtraDisk   `yaml:"extraDisks,omitempty" json:"extraDisks,omitempty"`
}

type KubernetesLayer struct {
	KubeadmConfigRef string               `yaml:"kubeadmConfigRef,omitempty" json:"kubeadmConfigRef,omitempty"`
	NodeLabels       map[string]string    `yaml:"nodeLabels,omitempty" json:"nodeLabels,omitempty"`
	NodeTaints       []manifest.NodeTaint `yaml:"nodeTaints,omitempty" json:"nodeTaints,omitempty"`
}

type BootstrapLayer struct {
	Address string           `yaml:"address,omitempty" json:"address,omitempty"`
	Access  inventory.Access `yaml:"access,omitempty" json:"access,omitempty"`
}

type CompileRequest struct {
	Config                     Config
	KubeadmConfigs             map[string]kubeadmconfig.Plan
	KubernetesCatalog          sysextcatalog.Catalog
	KubernetesArtifactBasePath string
	KubernetesActivationPath   string
	AddressOverrides           map[string]string
}

type Plan struct {
	ControlPlaneEndpoint   string                      `json:"controlPlaneEndpoint,omitempty"`
	PlatformAPIEndpoint    *platformendpoint.Plan      `json:"platformAPIEndpoint,omitempty"`
	KubernetesVersion      string                      `json:"kubernetesVersion,omitempty"`
	KubernetesCatalogRef   string                      `json:"kubernetesCatalogRef,omitempty"`
	KubernetesBundleSource string                      `json:"kubernetesBundleSource,omitempty"`
	KubernetesBundleRef    string                      `json:"kubernetesBundleRef,omitempty"`
	KubernetesSysext       *generation.ExtensionRef    `json:"kubernetesSysext,omitempty"`
	KatlosImage            manifest.KatlosImage        `json:"katlosImage"`
	Nodes                  []NodeMaterial              `json:"nodes"`
	BootstrapInventory     inventory.Inventory         `json:"bootstrapInventory"`
	AddressOverrides       []inventory.AddressOverride `json:"addressOverrides,omitempty"`
}

type NodeMaterial struct {
	Name                   string                   `json:"name"`
	SystemRole             inventory.SystemRole     `json:"systemRole"`
	BootstrapAddress       string                   `json:"bootstrapAddress,omitempty"`
	InstallManifest        manifest.Manifest        `json:"installManifest"`
	NativeEtcFiles         []confext.NativeEtcFile  `json:"nativeEtcFiles,omitempty"`
	KubeadmConfig          inventory.KubeadmConfig  `json:"kubeadmConfig,omitempty"`
	NodeLabels             map[string]string        `json:"nodeLabels,omitempty"`
	NodeTaints             []manifest.NodeTaint     `json:"nodeTaints,omitempty"`
	KubernetesVersion      string                   `json:"kubernetesVersion,omitempty"`
	KubernetesCatalogRef   string                   `json:"kubernetesCatalogRef,omitempty"`
	KubernetesBundleSource string                   `json:"kubernetesBundleSource,omitempty"`
	KubernetesBundleRef    string                   `json:"kubernetesBundleRef,omitempty"`
	KubernetesSysext       *generation.ExtensionRef `json:"kubernetesSysext,omitempty"`
}
