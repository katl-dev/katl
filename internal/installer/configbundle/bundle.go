package configbundle

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	installer "github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/clusterplan"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/kubernetescompat"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "config.katl.dev/v1alpha1"
	Kind       = "ClusterConfig"

	BundleManifestMediaType = "application/vnd.katl.config.bundle.v1+json"
	BundleArtifactType      = "application/vnd.katl.config.bundle.v1"

	DefaultKubernetesVersion = "v1.36.1"
)

type BuildRequest struct {
	SourcePath     string
	KatlctlVersion string
	KatlctlCommit  string
	CreatedBy      string
	Planning       PlanningInputs
}

// PlanningInputs are operation-scoped mechanisms supplied by Katl, not
// operator-authored cluster intent.
type PlanningInputs struct {
	KatlosImage      manifest.KatlosImage
	KubernetesBundle string
	BootstrapAccess  map[string]inventory.Access
}

type Result struct {
	Digest      string
	Manifest    BundleManifest
	ArchiveSize int64
}

type SourceConfig struct {
	APIVersion string     `yaml:"apiVersion" json:"apiVersion"`
	Kind       string     `yaml:"kind" json:"kind"`
	Metadata   Metadata   `yaml:"metadata" json:"metadata"`
	Spec       SourceSpec `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type SourceSpec struct {
	ControlPlaneEndpoint string                  `yaml:"controlPlaneEndpoint,omitempty" json:"controlPlaneEndpoint,omitempty"`
	Kubernetes           SourceKubernetesCluster `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`
	Defaults             SourceNodeLayer         `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Nodes                []SourceNode            `yaml:"nodes" json:"nodes"`
}

type SourceNode struct {
	Name       string                  `yaml:"name" json:"name"`
	SystemRole inventory.SystemRole    `yaml:"systemRole" json:"systemRole"`
	Identity   SourceIdentity          `yaml:"identity,omitempty" json:"identity,omitempty"`
	Networkd   manifest.NetworkdConfig `yaml:"networkd,omitempty" json:"networkd,omitempty"`
	Install    SourceInstallLayer      `yaml:"install,omitempty" json:"install,omitempty"`
	Kubernetes SourceKubernetesLayer   `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`
	Bootstrap  SourceBootstrapLayer    `yaml:"bootstrap,omitempty" json:"bootstrap,omitempty"`
}

type SourceNodeLayer struct {
	Identity   SourceIdentity          `yaml:"identity,omitempty" json:"identity,omitempty"`
	Networkd   manifest.NetworkdConfig `yaml:"networkd,omitempty" json:"networkd,omitempty"`
	Install    SourceInstallLayer      `yaml:"install,omitempty" json:"install,omitempty"`
	Kubernetes SourceKubernetesLayer   `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`
}

type SourceBootstrapLayer struct {
	Address string `yaml:"address,omitempty" json:"address,omitempty"`
}

type SourceIdentity struct {
	SSH manifest.SSHIdentity `yaml:"ssh,omitempty" json:"ssh,omitempty"`
}

type SourceInstallLayer struct {
	TargetDisk         *manifest.DiskSelector `yaml:"targetDisk,omitempty" json:"targetDisk,omitempty"`
	TargetDiskDefaults *manifest.DiskSelector `yaml:"targetDiskDefaults,omitempty" json:"targetDiskDefaults,omitempty"`
	ExtraDisks         []manifest.ExtraDisk   `yaml:"extraDisks,omitempty" json:"extraDisks,omitempty"`
}

type SourceKubernetesCluster struct {
	Version string              `yaml:"version,omitempty" json:"version,omitempty"`
	Kubeadm *SourceKubeadmInput `yaml:"kubeadm,omitempty" json:"kubeadm,omitempty"`
}

type SourceKubeadmInput struct {
	ConfigFile string `yaml:"configFile,omitempty" json:"configFile,omitempty"`
	PatchesDir string `yaml:"patchesDir,omitempty" json:"patchesDir,omitempty"`
}

type SourceKubernetesLayer struct {
	Labels map[string]string    `yaml:"labels,omitempty" json:"labels,omitempty"`
	Taints []manifest.NodeTaint `yaml:"taints,omitempty" json:"taints,omitempty"`
}

type BundleManifest struct {
	APIVersion          string        `json:"apiVersion"`
	Kind                string        `json:"kind"`
	ArtifactKind        string        `json:"artifactKind"`
	ArtifactVersion     string        `json:"artifactVersion"`
	BundleSchemaVersion int           `json:"bundleSchemaVersion"`
	ClusterName         string        `json:"clusterName"`
	CreatedAt           string        `json:"createdAt"`
	Compatibility       Compatibility `json:"compatibility"`
	Source              SourceRecord  `json:"source"`
	Cluster             ClusterRecord `json:"cluster"`
	Nodes               []NodeRecord  `json:"nodes"`
	Descriptors         []Descriptor  `json:"descriptors"`
	Provenance          Provenance    `json:"provenance"`
}

type Compatibility struct {
	SupportedArchitectures           []string `json:"supportedArchitectures,omitempty"`
	SupportedKatlOSRuntimeInterfaces []string `json:"supportedKatlOSRuntimeInterfaces,omitempty"`
	RequiredInstallerFeatures        []string `json:"requiredInstallerFeatures,omitempty"`
	RequiredKatlcFeatures            []string `json:"requiredKatlcFeatures,omitempty"`
	InstallMaterialSchemaVersion     string   `json:"installMaterialSchemaVersion"`
	ClusterPlanSchemaVersion         string   `json:"clusterPlanSchemaVersion"`
	KubeadmAPIVersions               []string `json:"kubeadmAPIVersions,omitempty"`
}

type SourceRecord struct {
	NormalizedConfig Descriptor `json:"normalizedConfig"`
	SourceDigest     string     `json:"sourceDigest"`
}

type ClusterRecord struct {
	ResolvedPlan       Descriptor                `json:"resolvedPlan"`
	BootstrapInventory inventory.Inventory       `json:"bootstrapInventory"`
	KubernetesPayloads []KubernetesPayloadRecord `json:"kubernetesPayloads"`
}

type KubernetesPayloadRecord struct {
	RequestedVersion           string   `json:"requestedVersion,omitempty"`
	ResolvedPayloadVersion     string   `json:"resolvedPayloadVersion,omitempty"`
	Source                     string   `json:"source,omitempty"`
	Ref                        string   `json:"ref,omitempty"`
	BundleManifestDigest       string   `json:"bundleManifestDigest,omitempty"`
	OCIManifestDigest          string   `json:"ociManifestDigest,omitempty"`
	ArtifactVersion            string   `json:"artifactVersion,omitempty"`
	Architecture               string   `json:"architecture,omitempty"`
	SupportedRuntimeInterfaces []string `json:"supportedRuntimeInterfaces,omitempty"`
	ResolverVersion            string   `json:"resolverVersion"`
}

type NodeRecord struct {
	Name            string       `json:"name"`
	SystemRole      string       `json:"systemRole"`
	Architecture    string       `json:"architecture,omitempty"`
	NodeMaterial    Descriptor   `json:"nodeMaterial"`
	InstallMaterial Descriptor   `json:"installMaterial"`
	NativeConfig    Descriptor   `json:"nativeConfig"`
	KubeadmInputs   []Descriptor `json:"kubeadmInputs,omitempty"`
	ResolvedDigests []Descriptor `json:"resolvedDigests,omitempty"`
}

type Descriptor struct {
	Role        string            `json:"role"`
	Node        string            `json:"node,omitempty"`
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	SizeBytes   int               `json:"sizeBytes"`
	FileName    string            `json:"fileName"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Provenance struct {
	KatlctlVersion        string `json:"katlctlVersion,omitempty"`
	KatlctlCommit         string `json:"katlctlCommit,omitempty"`
	CompilerSchemaVersion string `json:"compilerSchemaVersion"`
	SourceDigest          string `json:"sourceDigest"`
	RenderedDigest        string `json:"renderedDigest"`
	CreatedBy             string `json:"createdBy,omitempty"`
}

type member struct {
	descriptor Descriptor
	data       []byte
}

func BuildArchive(request BuildRequest) ([]byte, Result, error) {
	sourcePath := strings.TrimSpace(request.SourcePath)
	if sourcePath == "" {
		return nil, Result{}, fmt.Errorf("source path is required")
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, Result{}, fmt.Errorf("resolve source path: %w", err)
	}
	sourceData, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, Result{}, fmt.Errorf("read source config: %w", err)
	}
	source, err := DecodeSource(bytes.NewReader(sourceData))
	if err != nil {
		return nil, Result{}, err
	}
	source = defaultSource(source)
	normalized, err := marshalCanonical(source)
	if err != nil {
		return nil, Result{}, err
	}
	kubeadmConfigs, kubeadmSourceInputs, err := resolveKubeadmConfigs(filepath.Dir(sourcePath), source.Spec.Kubernetes.Kubeadm, selectedKubernetesVersion(source))
	if err != nil {
		return nil, Result{}, err
	}
	sourceDigest := digestSourceInputs(normalized, kubeadmSourceInputs)
	planning := request.Planning
	if strings.TrimSpace(planning.KubernetesBundle) == "" {
		selection, err := kubernetescompat.Resolve(kubernetescompat.Request{
			KubernetesVersion: selectedKubernetesVersion(source),
			Architecture:      planning.KatlosImage.Architecture,
			RuntimeInterface:  planning.KatlosImage.RuntimeInterface,
		})
		if err != nil {
			return nil, Result{}, fmt.Errorf("resolve spec.kubernetes.version: %w", err)
		}
		planning.KubernetesBundle = selection.Bundle
	}
	config, err := LowerSource(source, planning)
	if err != nil {
		return nil, Result{}, err
	}
	plan, err := clusterplan.Compile(clusterplan.CompileRequest{
		Config:         config,
		KubeadmConfigs: kubeadmConfigs,
	})
	if err != nil {
		return nil, Result{}, err
	}
	members, manifest, err := buildMembers(source, normalized, sourceDigest, plan, kubeadmConfigs, request)
	if err != nil {
		return nil, Result{}, err
	}
	renderedDigest, err := renderedDigest(members)
	if err != nil {
		return nil, Result{}, err
	}
	manifest.ArtifactVersion = renderedDigest
	manifest.Provenance.RenderedDigest = renderedDigest
	manifestBytes, err := marshalCanonical(manifest)
	if err != nil {
		return nil, Result{}, err
	}
	manifestDigest := digestBytes(manifestBytes)
	members = append(members, member{
		descriptor: Descriptor{
			Role:      "bundle-manifest",
			MediaType: BundleManifestMediaType,
			Digest:    manifestDigest,
			SizeBytes: len(manifestBytes),
			FileName:  "bundle/manifest.json",
		},
		data: manifestBytes,
	})
	archive, err := writeOCIArchive(manifestDigest, members)
	if err != nil {
		return nil, Result{}, err
	}
	return archive, Result{Digest: manifestDigest, Manifest: manifest, ArchiveSize: int64(len(archive))}, nil
}

func WriteArchive(path string, request BuildRequest) (Result, error) {
	archive, result, err := BuildArchive(request)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(path) == "" {
		return Result{}, fmt.Errorf("output path is required")
	}
	if err := os.WriteFile(path, archive, 0o644); err != nil {
		return Result{}, fmt.Errorf("write config bundle: %w", err)
	}
	return result, nil
}

func DecodeSource(reader io.Reader) (SourceConfig, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return SourceConfig{}, fmt.Errorf("read cluster config: %w", err)
	}
	var document yaml.Node
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := nodeDecoder.Decode(&document); err != nil {
		return SourceConfig{}, fmt.Errorf("decode cluster config: %w", err)
	}
	var trailing yaml.Node
	if err := nodeDecoder.Decode(&trailing); err != io.EOF {
		return SourceConfig{}, fmt.Errorf("decode cluster config: multiple YAML documents")
	}
	if err := validateSourceFields(&document); err != nil {
		return SourceConfig{}, fmt.Errorf("decode cluster config: %w", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config SourceConfig
	if err := decoder.Decode(&config); err != nil {
		return SourceConfig{}, fmt.Errorf("decode cluster config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return SourceConfig{}, fmt.Errorf("decode cluster config: multiple YAML documents")
	}
	if config.APIVersion != APIVersion {
		return SourceConfig{}, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if config.Kind != Kind {
		return SourceConfig{}, fmt.Errorf("kind must be %s", Kind)
	}
	return config, nil
}

func LowerSource(source SourceConfig, planning PlanningInputs) (clusterplan.Config, error) {
	source = defaultSource(source)
	selection, err := lowerKubernetesSelection(source, planning.KubernetesBundle)
	if err != nil {
		return clusterplan.Config{}, err
	}
	nodes := make([]clusterplan.Node, 0, len(source.Spec.Nodes))
	for _, node := range source.Spec.Nodes {
		layer := lowerNodeLayer(sourceNodeLayer(node))
		layer.Bootstrap.Address = strings.TrimSpace(node.Bootstrap.Address)
		if access, ok := planning.BootstrapAccess[node.Name]; ok {
			layer.Bootstrap.Access = access
		}
		layer.Kubernetes.KubeadmConfigRef = defaultKubeadmConfigRef(node.SystemRole)
		nodes = append(nodes, clusterplan.Node{
			Name:       node.Name,
			SystemRole: node.SystemRole,
			Overrides:  layer,
		})
	}
	return clusterplan.Config{
		APIVersion: clusterplan.APIVersion,
		Kind:       clusterplan.Kind,
		Metadata:   clusterplan.Metadata{Name: source.Metadata.Name},
		Spec: clusterplan.Spec{
			ControlPlaneEndpoint: source.Spec.ControlPlaneEndpoint,
			Kubernetes:           selection,
			KatlosImage:          planning.KatlosImage,
			WipeTarget:           true,
			Defaults:             lowerNodeLayer(source.Spec.Defaults),
			Nodes:                nodes,
		},
	}, nil
}

func lowerKubernetesSelection(source SourceConfig, bundle string) (clusterplan.KubernetesSelection, error) {
	version := strings.TrimSpace(source.Spec.Kubernetes.Version)
	out := clusterplan.KubernetesSelection{PayloadVersion: version}
	if bundle = strings.TrimSpace(bundle); bundle != "" {
		image, err := kubernetesbundle.ParseImageReference(bundle)
		if err != nil {
			return clusterplan.KubernetesSelection{}, fmt.Errorf("operation Kubernetes bundle: %w", err)
		}
		if version != "" && version != image.PayloadVersion {
			return clusterplan.KubernetesSelection{}, fmt.Errorf("spec.kubernetes.version %q is not available from the selected Kubernetes bundle %q", version, bundle)
		}
		out.PayloadVersion = ""
		out.BundleSource = image.Source
		out.BundleRef = image.Value
	}
	return out, nil
}

func lowerNodeLayer(layer SourceNodeLayer) clusterplan.NodeLayer {
	return clusterplan.NodeLayer{
		SSH:      layer.Identity.SSH,
		Networkd: layer.Networkd,
		Install: clusterplan.InstallLayer{
			TargetDisk:         layer.Install.TargetDisk,
			TargetDiskDefaults: layer.Install.TargetDiskDefaults,
			ExtraDisks:         append([]manifest.ExtraDisk(nil), layer.Install.ExtraDisks...),
		},
		Kubernetes: clusterplan.KubernetesLayer{
			NodeLabels: copyLabels(layer.Kubernetes.Labels),
			NodeTaints: append([]manifest.NodeTaint(nil), layer.Kubernetes.Taints...),
		},
	}
}

func sourceNodeLayer(node SourceNode) SourceNodeLayer {
	return SourceNodeLayer{
		Identity:   node.Identity,
		Networkd:   node.Networkd,
		Install:    node.Install,
		Kubernetes: node.Kubernetes,
	}
}

func defaultKubeadmConfigRef(role inventory.SystemRole) string {
	switch role {
	case inventory.RoleControlPlane:
		return "control-plane"
	case inventory.RoleWorker:
		return "worker"
	default:
		return ""
	}
}

func selectedKubernetesVersion(source SourceConfig) string {
	return strings.TrimSpace(source.Spec.Kubernetes.Version)
}

func defaultSource(source SourceConfig) SourceConfig {
	spec := source.Spec
	spec.Nodes = append([]SourceNode(nil), spec.Nodes...)
	if strings.TrimSpace(spec.ControlPlaneEndpoint) == "" {
		for _, node := range spec.Nodes {
			if node.SystemRole == inventory.RoleControlPlane {
				if address := strings.TrimSpace(node.Bootstrap.Address); address != "" {
					spec.ControlPlaneEndpoint = net.JoinHostPort(address, "6443")
					break
				}
			}
		}
	}
	version := strings.TrimSpace(spec.Kubernetes.Version)
	if version == "" {
		spec.Kubernetes.Version = DefaultKubernetesVersion
	}
	source.Spec = spec
	return source
}

func defaultKubeadmInitConfig(version string) string {
	config := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  criSocket: unix:///run/containerd/containerd.sock\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"
	if version != "" {
		config += "kubernetesVersion: " + version + "\n"
	}
	return config
}

func defaultKubeadmJoinConfig() string {
	return "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\nnodeRegistration:\n  criSocket: unix:///run/containerd/containerd.sock\n"
}

func defaultKubeadmConfigs(kubernetesVersion string) (map[string]kubeadmconfig.Plan, error) {
	inputs := map[string]string{
		"control-plane": defaultKubeadmInitConfig(kubernetesVersion),
		"worker":        defaultKubeadmJoinConfig(),
	}
	configs := make(map[string]kubeadmconfig.Plan, len(inputs))
	for _, name := range []string{"control-plane", "worker"} {
		plan, err := kubeadmconfig.PlanFromRenderedFiles(name, []kubeadmconfig.File{{
			RenderPath: "/etc/katl/kubeadm/" + name + "/config.yaml",
			Content:    []byte(inputs[name]),
			Mode:       0o644,
		}})
		if err != nil {
			return nil, fmt.Errorf("select internal kubeadm profile %s: %w", name, err)
		}
		configs[name] = plan
	}
	return configs, nil
}

func buildMembers(source SourceConfig, normalized []byte, sourceDigest string, plan clusterplan.Plan, kubeadmConfigs map[string]kubeadmconfig.Plan, request BuildRequest) ([]member, BundleManifest, error) {
	var members []member
	descriptors := []Descriptor{}
	add := func(role, node, mediaType, fileName string, value any, annotations map[string]string) (Descriptor, error) {
		data, err := marshalCanonical(value)
		if err != nil {
			return Descriptor{}, err
		}
		return addBytes(&members, &descriptors, role, node, mediaType, fileName, data, annotations), nil
	}
	sourceDesc := addBytes(&members, &descriptors, "source-normalized", "", "application/vnd.katl.cluster-config.v1+yaml", "source/cluster.normalized.yaml", normalized, nil)
	_, err := add("source-provenance", "", "application/vnd.katl.source-provenance.v1+json", "source/provenance.json", map[string]string{"sourceDigest": sourceDigest}, nil)
	if err != nil {
		return nil, BundleManifest{}, err
	}
	planDesc, err := add("cluster-plan", "", "application/vnd.katl.cluster-plan.v1+json", "cluster/plan.json", plan, nil)
	if err != nil {
		return nil, BundleManifest{}, err
	}
	payloads := kubernetesPayloads(plan)
	_, err = add("kubernetes-payloads", "", "application/vnd.katl.kubernetes.payload.resolution.v1+json", "cluster/kubernetes-payloads.json", payloads, nil)
	if err != nil {
		return nil, BundleManifest{}, err
	}
	_, err = add("bundle-provenance", "", "application/vnd.katl.bundle-provenance.v1+json", "bundle/provenance.json", Provenance{
		KatlctlVersion:        request.KatlctlVersion,
		KatlctlCommit:         request.KatlctlCommit,
		CompilerSchemaVersion: "config-bundle-v1",
		SourceDigest:          sourceDigest,
		CreatedBy:             firstNonEmpty(request.CreatedBy, "katlctl config bundle"),
	}, nil)
	if err != nil {
		return nil, BundleManifest{}, err
	}
	nodeRecords := make([]NodeRecord, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		materialDesc, err := add("node-material", node.Name, "application/vnd.katl.node-material.v1+json", "nodes/"+node.Name+"/material.json", node, nil)
		if err != nil {
			return nil, BundleManifest{}, err
		}
		installDesc, err := add("node-install-material", node.Name, "application/vnd.katl.node-install-material.v1+json", "nodes/"+node.Name+"/install/material.json", node.InstallManifest, nil)
		if err != nil {
			return nil, BundleManifest{}, err
		}
		intent, err := installer.BuildClusterIntent(installer.ClusterIntentRequest{
			Manifest:          node.InstallManifest,
			KubeadmConfigs:    kubeadmConfigs,
			KubernetesVersion: plan.KubernetesVersion,
			GenerationID:      "0",
			RequestDigest:     sourceDigest,
			InstalledAt:       time.Unix(0, 0).UTC(),
		})
		if err != nil {
			return nil, BundleManifest{}, err
		}
		intentDesc, err := add("node-cluster-intent", node.Name, "application/vnd.katl.node-cluster-intent.v1+json", "nodes/"+node.Name+"/cluster/intent.json", intent, nil)
		if err != nil {
			return nil, BundleManifest{}, err
		}
		nativeDesc, err := add("node-native-config", node.Name, "application/vnd.katl.node-native-config.v1+json", "nodes/"+node.Name+"/config/native.json", node.NativeEtcFiles, nil)
		if err != nil {
			return nil, BundleManifest{}, err
		}
		kubeadmDescs, err := addNodeKubeadmInputs(&members, &descriptors, node, kubeadmConfigs)
		if err != nil {
			return nil, BundleManifest{}, err
		}
		digests := []Descriptor{materialDesc, installDesc, intentDesc, nativeDesc}
		digests = append(digests, kubeadmDescs...)
		digestDesc, err := add("node-digests", node.Name, "application/vnd.katl.node-digests.v1+json", "nodes/"+node.Name+"/digests.json", digests, nil)
		if err != nil {
			return nil, BundleManifest{}, err
		}
		nodeRecords = append(nodeRecords, NodeRecord{
			Name:            node.Name,
			SystemRole:      string(node.SystemRole),
			Architecture:    plan.KatlosImage.Architecture,
			NodeMaterial:    materialDesc,
			InstallMaterial: installDesc,
			NativeConfig:    nativeDesc,
			KubeadmInputs:   kubeadmDescs,
			ResolvedDigests: append(digests, digestDesc),
		})
	}
	manifest := BundleManifest{
		APIVersion:          APIVersion,
		Kind:                "KatlConfigBundle",
		ArtifactKind:        "katl.config.bundle.v1",
		BundleSchemaVersion: 1,
		ClusterName:         source.Metadata.Name,
		CreatedAt:           time.Unix(0, 0).UTC().Format(time.RFC3339),
		Compatibility: Compatibility{
			SupportedArchitectures:           nonEmptyStrings(plan.KatlosImage.Architecture),
			SupportedKatlOSRuntimeInterfaces: nonEmptyStrings(plan.KatlosImage.RuntimeInterface),
			RequiredInstallerFeatures:        []string{"config-bundle-v1"},
			InstallMaterialSchemaVersion:     manifest.APIVersion,
			ClusterPlanSchemaVersion:         clusterplan.APIVersion,
			KubeadmAPIVersions:               kubeadmAPIVersions(kubeadmConfigs),
		},
		Source: SourceRecord{
			NormalizedConfig: sourceDesc,
			SourceDigest:     sourceDigest,
		},
		Cluster: ClusterRecord{
			ResolvedPlan:       planDesc,
			BootstrapInventory: plan.BootstrapInventory,
			KubernetesPayloads: payloads,
		},
		Nodes:       nodeRecords,
		Descriptors: descriptors,
		Provenance: Provenance{
			KatlctlVersion:        request.KatlctlVersion,
			KatlctlCommit:         request.KatlctlCommit,
			CompilerSchemaVersion: "config-bundle-v1",
			SourceDigest:          sourceDigest,
			CreatedBy:             firstNonEmpty(request.CreatedBy, "katlctl config bundle"),
		},
	}
	if len(payloads) == 1 {
		if len(manifest.Compatibility.SupportedArchitectures) == 0 {
			manifest.Compatibility.SupportedArchitectures = nonEmptyStrings(payloads[0].Architecture)
		}
		if len(manifest.Compatibility.SupportedKatlOSRuntimeInterfaces) == 0 {
			manifest.Compatibility.SupportedKatlOSRuntimeInterfaces = append([]string(nil), payloads[0].SupportedRuntimeInterfaces...)
		}
	}
	return members, manifest, nil
}

func addNodeKubeadmInputs(members *[]member, descriptors *[]Descriptor, node clusterplan.NodeMaterial, configs map[string]kubeadmconfig.Plan) ([]Descriptor, error) {
	ref := strings.TrimSpace(node.KubeadmConfig.Ref)
	if ref == "" {
		return nil, nil
	}
	plan, ok := configs[ref]
	if !ok {
		return nil, fmt.Errorf("node %s kubeadm config %q was not resolved", node.Name, ref)
	}
	var out []Descriptor
	files := append([]kubeadmconfig.File{plan.Config}, plan.Patches...)
	for _, file := range files {
		rel := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(file.RenderPath), "/etc/katl/kubeadm/"+ref+"/"), "/")
		desc := addBytes(members, descriptors, "kubeadm-input", node.Name, "application/vnd.katl.kubeadm.input.v1+yaml", "nodes/"+node.Name+"/kubernetes/kubeadm/"+rel, file.Content, map[string]string{
			"dev.katl.kubeadm.resolved-id":  ref,
			"dev.katl.kubeadm.intent":       string(node.KubeadmConfig.Intent),
			"dev.katl.kubeadm.api-versions": strings.Join(kubeadmAPIVersions(map[string]kubeadmconfig.Plan{ref: plan}), ","),
		})
		out = append(out, desc)
	}
	return out, nil
}

func addBytes(members *[]member, descriptors *[]Descriptor, role, node, mediaType, fileName string, data []byte, annotations map[string]string) Descriptor {
	desc := Descriptor{
		Role:        role,
		Node:        node,
		MediaType:   mediaType,
		Digest:      digestBytes(data),
		SizeBytes:   len(data),
		FileName:    fileName,
		Annotations: annotations,
	}
	*members = append(*members, member{descriptor: desc, data: append([]byte(nil), data...)})
	*descriptors = append(*descriptors, desc)
	return desc
}

func kubernetesPayloads(plan clusterplan.Plan) []KubernetesPayloadRecord {
	record := KubernetesPayloadRecord{
		RequestedVersion:           plan.KubernetesVersion,
		ResolvedPayloadVersion:     plan.KubernetesVersion,
		Source:                     plan.KubernetesBundleSource,
		Ref:                        plan.KubernetesBundleRef,
		Architecture:               plan.KatlosImage.Architecture,
		SupportedRuntimeInterfaces: nonEmptyStrings(plan.KatlosImage.RuntimeInterface),
		ResolverVersion:            "config-bundle-v1",
	}
	if selected, err := kubernetescompat.Resolve(kubernetescompat.Request{KubernetesVersion: plan.KubernetesVersion}); err == nil && selected.Bundle == plan.KubernetesBundleRef {
		record.ResolverVersion = "release-compatibility-v1"
		record.SupportedRuntimeInterfaces = append([]string(nil), selected.RuntimeInterfaces...)
		if record.Architecture == "" && len(selected.Architectures) == 1 {
			record.Architecture = selected.Architectures[0]
		}
	}
	if image, err := kubernetesbundle.ParseImageReference(plan.KubernetesBundleRef); err == nil {
		record.ArtifactVersion = image.ArtifactVersion
		record.OCIManifestDigest = image.ManifestDigest
	} else if _, digest, ok := strings.Cut(plan.KubernetesBundleRef, "@"); ok {
		record.BundleManifestDigest = digest
	}
	return []KubernetesPayloadRecord{record}
}

func renderedDigest(members []member) (string, error) {
	type renderedMember struct {
		Descriptor Descriptor `json:"descriptor"`
		Digest     string     `json:"digest"`
	}
	rendered := make([]renderedMember, 0, len(members))
	for _, member := range members {
		rendered = append(rendered, renderedMember{Descriptor: member.descriptor, Digest: digestBytes(member.data)})
	}
	sort.Slice(rendered, func(i, j int) bool {
		return rendered[i].Descriptor.FileName < rendered[j].Descriptor.FileName
	})
	data, err := marshalCanonical(rendered)
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func writeOCIArchive(manifestDigest string, members []member) ([]byte, error) {
	blobs := map[string][]byte{}
	for _, member := range members {
		blobs[member.descriptor.Digest] = member.data
	}
	index := map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{{
			"mediaType":    BundleManifestMediaType,
			"artifactType": BundleArtifactType,
			"digest":       manifestDigest,
			"size":         len(blobs[manifestDigest]),
		}},
	}
	indexBytes, err := marshalCanonical(index)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarFile(tw, "oci-layout", []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"), 0o644); err != nil {
		return nil, err
	}
	if err := writeTarFile(tw, "index.json", indexBytes, 0o644); err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(blobs))
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		hexValue := strings.TrimPrefix(digest, "sha256:")
		if err := writeTarFile(tw, "blobs/sha256/"+hexValue, blobs[digest], 0o644); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close bundle archive: %w", err)
	}
	return buf.Bytes(), nil
}

func writeTarFile(tw *tar.Writer, name string, data []byte, mode fs.FileMode) error {
	header := &tar.Header{
		Name:    name,
		Mode:    int64(mode.Perm()),
		Size:    int64(len(data)),
		ModTime: time.Unix(0, 0).UTC(),
		Uid:     0,
		Gid:     0,
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar file %s: %w", name, err)
	}
	return nil
}

func marshalCanonical(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestSourceInputs(config []byte, kubeadmInputs []kubeadmSourceInput) string {
	if len(kubeadmInputs) == 0 {
		return digestBytes(config)
	}
	inputs := append([]kubeadmSourceInput(nil), kubeadmInputs...)
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "cluster-config:%d\n", len(config))
	_, _ = hash.Write(config)
	for _, input := range inputs {
		_, _ = fmt.Fprintf(hash, "kubeadm-input:%d:%s:%d\n", len(input.Name), input.Name, len(input.Content))
		_, _ = hash.Write(input.Content)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func kubeadmAPIVersions(configs map[string]kubeadmconfig.Plan) []string {
	seen := map[string]struct{}{}
	for _, config := range configs {
		for _, doc := range config.Documents {
			if strings.TrimSpace(doc.APIVersion) != "" {
				seen[doc.APIVersion] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func nonEmptyStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
