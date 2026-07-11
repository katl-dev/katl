package configbundle

import (
	"archive/tar"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/clusterplan"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type ReadOptions struct {
	ExpectedDigest     string
	NodeName           string
	DefaultKatlosImage manifest.KatlosImage
}

type SelectedNodeMaterial struct {
	BundleManifest        BundleManifest
	Node                  NodeRecord
	NodeMaterial          clusterplan.NodeMaterial
	InstallManifest       manifest.Manifest
	KubeadmConfigs        map[string]kubeadmconfig.Plan
	BundleDigest          string
	SourceDigest          string
	NodeMaterialDigest    string
	InstallMaterialDigest string
	KatlosImageFromMedia  bool
}

type Bundle struct {
	Manifest BundleManifest
	Digest   string
}

func ReadBundleFile(path, expectedDigest string) (Bundle, error) {
	file, err := os.Open(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("open config bundle: %w", err)
	}
	defer file.Close()
	return ReadBundle(file, expectedDigest)
}

func ReadBundle(reader io.Reader, expectedDigest string) (Bundle, error) {
	archive, err := readOCIArchive(reader)
	if err != nil {
		return Bundle{}, err
	}
	bundleDigest, manifestData, err := archive.bundleManifest()
	if err != nil {
		return Bundle{}, err
	}
	if expected := normalizeDigest(expectedDigest); expected != "" && expected != bundleDigest {
		return Bundle{}, fmt.Errorf("config bundle digest mismatch: got %s want %s", bundleDigest, expected)
	}
	var bundle BundleManifest
	if err := json.Unmarshal(manifestData, &bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode config bundle manifest: %w", err)
	}
	if err := validateBundleManifest(bundle); err != nil {
		return Bundle{}, err
	}
	return Bundle{Manifest: bundle, Digest: bundleDigest}, nil
}

func ReadSelectedNodeFile(path string, options ReadOptions) (SelectedNodeMaterial, error) {
	file, err := os.Open(path)
	if err != nil {
		return SelectedNodeMaterial{}, fmt.Errorf("open config bundle: %w", err)
	}
	defer file.Close()
	return ReadSelectedNode(file, options)
}

func ReadSelectedNode(reader io.Reader, options ReadOptions) (SelectedNodeMaterial, error) {
	archive, err := readOCIArchive(reader)
	if err != nil {
		return SelectedNodeMaterial{}, err
	}
	bundleDigest, manifestData, err := archive.bundleManifest()
	if err != nil {
		return SelectedNodeMaterial{}, err
	}
	if expected := normalizeDigest(options.ExpectedDigest); expected != "" && expected != bundleDigest {
		return SelectedNodeMaterial{}, fmt.Errorf("config bundle digest mismatch: got %s want %s", bundleDigest, expected)
	}
	var bundle BundleManifest
	if err := json.Unmarshal(manifestData, &bundle); err != nil {
		return SelectedNodeMaterial{}, fmt.Errorf("decode config bundle manifest: %w", err)
	}
	if err := validateBundleManifest(bundle); err != nil {
		return SelectedNodeMaterial{}, err
	}
	node, err := selectNode(bundle.Nodes, options.NodeName)
	if err != nil {
		return SelectedNodeMaterial{}, err
	}

	nodeMaterialData, err := archive.descriptorData(node.NodeMaterial)
	if err != nil {
		return SelectedNodeMaterial{}, fmt.Errorf("read selected node material: %w", err)
	}
	var nodeMaterial clusterplan.NodeMaterial
	if err := json.Unmarshal(nodeMaterialData, &nodeMaterial); err != nil {
		return SelectedNodeMaterial{}, fmt.Errorf("decode selected node material %q: %w", node.Name, err)
	}

	installData, err := archive.descriptorData(node.InstallMaterial)
	if err != nil {
		return SelectedNodeMaterial{}, fmt.Errorf("read selected install material: %w", err)
	}
	installManifest, defaulted, err := manifest.DecodeWithDefaultImage(bytes.NewReader(installData), options.DefaultKatlosImage)
	if err != nil {
		return SelectedNodeMaterial{}, fmt.Errorf("decode selected install material %q: %w", node.Name, err)
	}
	if err := validateSelectedCompatibility(bundle, installManifest); err != nil {
		return SelectedNodeMaterial{}, err
	}

	kubeadmConfigs, err := selectedKubeadmConfigs(archive, node)
	if err != nil {
		return SelectedNodeMaterial{}, err
	}

	return SelectedNodeMaterial{
		BundleManifest:        bundle,
		Node:                  node,
		NodeMaterial:          nodeMaterial,
		InstallManifest:       installManifest,
		KubeadmConfigs:        kubeadmConfigs,
		BundleDigest:          bundleDigest,
		SourceDigest:          bundle.Source.SourceDigest,
		NodeMaterialDigest:    node.NodeMaterial.Digest,
		InstallMaterialDigest: node.InstallMaterial.Digest,
		KatlosImageFromMedia:  defaulted || (!manifest.KatlosImageEmpty(options.DefaultKatlosImage) && installManifest.KatlosImage == options.DefaultKatlosImage),
	}, nil
}

type ociArchive struct {
	index ociIndex
	blobs map[string][]byte
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociDescriptor struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType,omitempty"`
	Digest       string `json:"digest"`
	Size         int    `json:"size"`
}

func readOCIArchive(reader io.Reader) (ociArchive, error) {
	files := map[string][]byte{}
	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return ociArchive{}, fmt.Errorf("read config bundle archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if strings.HasPrefix(name, "../") || name == ".." || filepath.IsAbs(name) {
			return ociArchive{}, fmt.Errorf("config bundle archive contains unsafe path %q", header.Name)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			return ociArchive{}, fmt.Errorf("read config bundle archive member %s: %w", header.Name, err)
		}
		files[name] = buf.Bytes()
	}
	if _, ok := files["oci-layout"]; !ok {
		return ociArchive{}, fmt.Errorf("config bundle missing oci-layout")
	}
	indexData, ok := files["index.json"]
	if !ok {
		return ociArchive{}, fmt.Errorf("config bundle missing index.json")
	}
	var index ociIndex
	if err := json.Unmarshal(indexData, &index); err != nil {
		return ociArchive{}, fmt.Errorf("decode config bundle index: %w", err)
	}
	if index.SchemaVersion != 2 {
		return ociArchive{}, fmt.Errorf("config bundle index schemaVersion must be 2")
	}
	blobs := map[string][]byte{}
	for name, data := range files {
		if !strings.HasPrefix(name, "blobs/sha256/") {
			continue
		}
		hexDigest := strings.TrimPrefix(name, "blobs/sha256/")
		if len(hexDigest) != 64 {
			return ociArchive{}, fmt.Errorf("config bundle blob path %q has invalid sha256", name)
		}
		if _, err := hex.DecodeString(hexDigest); err != nil {
			return ociArchive{}, fmt.Errorf("config bundle blob path %q has invalid sha256: %w", name, err)
		}
		digest := "sha256:" + hexDigest
		if got := digestBytes(data); got != digest {
			return ociArchive{}, fmt.Errorf("config bundle blob %s digest mismatch: got %s", name, got)
		}
		blobs[digest] = data
	}
	return ociArchive{index: index, blobs: blobs}, nil
}

func (a ociArchive) bundleManifest() (string, []byte, error) {
	var matches []ociDescriptor
	for _, desc := range a.index.Manifests {
		if desc.MediaType == BundleManifestMediaType && desc.ArtifactType == BundleArtifactType {
			matches = append(matches, desc)
		}
	}
	if len(matches) != 1 {
		return "", nil, fmt.Errorf("config bundle index must contain exactly one Katl config bundle manifest, found %d", len(matches))
	}
	desc := matches[0]
	data, ok := a.blobs[desc.Digest]
	if !ok {
		return "", nil, fmt.Errorf("config bundle manifest blob %s is missing", desc.Digest)
	}
	if desc.Size != len(data) {
		return "", nil, fmt.Errorf("config bundle manifest size %d does not match blob size %d", desc.Size, len(data))
	}
	return desc.Digest, data, nil
}

func (a ociArchive) descriptorData(desc Descriptor) ([]byte, error) {
	if strings.TrimSpace(desc.Digest) == "" {
		return nil, fmt.Errorf("descriptor %s missing digest", desc.FileName)
	}
	data, ok := a.blobs[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("descriptor %s blob %s is missing", desc.FileName, desc.Digest)
	}
	if desc.SizeBytes != len(data) {
		return nil, fmt.Errorf("descriptor %s size %d does not match blob size %d", desc.FileName, desc.SizeBytes, len(data))
	}
	if got := digestBytes(data); got != desc.Digest {
		return nil, fmt.Errorf("descriptor %s digest mismatch: got %s want %s", desc.FileName, got, desc.Digest)
	}
	return data, nil
}

func validateBundleManifest(bundle BundleManifest) error {
	if bundle.APIVersion != APIVersion {
		return fmt.Errorf("config bundle apiVersion must be %s", APIVersion)
	}
	if bundle.Kind != "KatlConfigBundle" {
		return fmt.Errorf("config bundle kind must be KatlConfigBundle")
	}
	if bundle.ArtifactKind != "katl.config.bundle.v1" {
		return fmt.Errorf("config bundle artifactKind must be katl.config.bundle.v1")
	}
	if bundle.BundleSchemaVersion != 1 {
		return fmt.Errorf("unsupported config bundle schema version %d", bundle.BundleSchemaVersion)
	}
	if bundle.Compatibility.InstallMaterialSchemaVersion != manifest.APIVersion {
		return fmt.Errorf("config bundle install material schema %q is not supported", bundle.Compatibility.InstallMaterialSchemaVersion)
	}
	if bundle.Compatibility.ClusterPlanSchemaVersion != clusterplan.APIVersion {
		return fmt.Errorf("config bundle cluster plan schema %q is not supported", bundle.Compatibility.ClusterPlanSchemaVersion)
	}
	for _, feature := range bundle.Compatibility.RequiredInstallerFeatures {
		if feature != "config-bundle-v1" {
			return fmt.Errorf("config bundle requires unsupported installer feature %q", feature)
		}
	}
	if strings.TrimSpace(bundle.Source.SourceDigest) == "" {
		return fmt.Errorf("config bundle source digest is required")
	}
	if bundle.Provenance.SourceDigest != "" && bundle.Provenance.SourceDigest != bundle.Source.SourceDigest {
		return fmt.Errorf("config bundle provenance source digest %s does not match source digest %s", bundle.Provenance.SourceDigest, bundle.Source.SourceDigest)
	}
	return nil
}

func validateSelectedCompatibility(bundle BundleManifest, installManifest manifest.Manifest) error {
	image := installManifest.KatlosImage
	if len(bundle.Compatibility.SupportedArchitectures) > 0 && !containsString(bundle.Compatibility.SupportedArchitectures, image.Architecture) {
		return fmt.Errorf("config bundle does not support selected install architecture %q", image.Architecture)
	}
	if len(bundle.Compatibility.SupportedKatlOSRuntimeInterfaces) > 0 && !containsString(bundle.Compatibility.SupportedKatlOSRuntimeInterfaces, image.RuntimeInterface) {
		return fmt.Errorf("config bundle does not support selected install runtime interface %q", image.RuntimeInterface)
	}
	return nil
}

func containsString(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func selectNode(nodes []NodeRecord, nodeName string) (NodeRecord, error) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return NodeRecord{}, fmt.Errorf("selected node is required for config bundle")
	}
	var matches []NodeRecord
	for _, node := range nodes {
		if node.Name == nodeName {
			matches = append(matches, node)
		}
	}
	if len(matches) == 0 {
		return NodeRecord{}, fmt.Errorf("config bundle does not contain selected node %q", nodeName)
	}
	if len(matches) > 1 {
		return NodeRecord{}, fmt.Errorf("config bundle contains duplicate selected node %q", nodeName)
	}
	return matches[0], nil
}

func selectedKubeadmConfigs(archive ociArchive, node NodeRecord) (map[string]kubeadmconfig.Plan, error) {
	if len(node.KubeadmInputs) == 0 {
		return nil, nil
	}
	filesByRef := map[string][]kubeadmconfig.File{}
	prefix := "nodes/" + node.Name + "/kubernetes/kubeadm/"
	for _, desc := range node.KubeadmInputs {
		if desc.Role != "kubeadm-input" {
			return nil, fmt.Errorf("node %s kubeadm input descriptor has role %q", node.Name, desc.Role)
		}
		if desc.Node != node.Name {
			return nil, fmt.Errorf("node %s kubeadm input descriptor belongs to node %q", node.Name, desc.Node)
		}
		ref := strings.TrimSpace(desc.Annotations["dev.katl.kubeadm.resolved-id"])
		if ref == "" {
			return nil, fmt.Errorf("node %s kubeadm input %s missing resolved id", node.Name, desc.FileName)
		}
		if !strings.HasPrefix(desc.FileName, prefix) {
			return nil, fmt.Errorf("node %s kubeadm input %s is outside expected prefix", node.Name, desc.FileName)
		}
		rel := strings.TrimPrefix(desc.FileName, prefix)
		if rel == "" || rel == "." || strings.HasPrefix(filepath.ToSlash(filepath.Clean(rel)), "../") {
			return nil, fmt.Errorf("node %s kubeadm input %s has unsafe path", node.Name, desc.FileName)
		}
		data, err := archive.descriptorData(desc)
		if err != nil {
			return nil, err
		}
		filesByRef[ref] = append(filesByRef[ref], kubeadmconfig.File{
			RenderPath: "/etc/katl/kubeadm/" + ref + "/" + rel,
			Content:    data,
			Mode:       0o644,
		})
	}
	configs := make(map[string]kubeadmconfig.Plan, len(filesByRef))
	refs := make([]string, 0, len(filesByRef))
	for ref := range filesByRef {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		plan, err := kubeadmconfig.PlanFromRenderedFiles(ref, filesByRef[ref])
		if err != nil {
			return nil, fmt.Errorf("node %s kubeadm config %q: %w", node.Name, ref, err)
		}
		configs[ref] = plan
	}
	return configs, nil
}

func normalizeDigest(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "sha256:") {
		return value
	}
	if len(value) == 64 {
		if _, err := hex.DecodeString(value); err == nil {
			return "sha256:" + value
		}
	}
	return value
}
