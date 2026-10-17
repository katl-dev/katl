package kubeadmconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
)

const inputMetadataFile = "katl-input.json"

type inputMetadata struct {
	NodeLocalKubelet  bool `json:"nodeLocalKubelet,omitempty"`
	SharedKubeletBase bool `json:"sharedKubeletBase,omitempty"`
}

// ValidateSharedKubeletBase rejects node-local input whose shared base cannot
// be distinguished from a legacy projected configuration.
func (p Plan) ValidateSharedKubeletBase() error {
	if p.NodeLocalKubelet && !p.SharedKubeletBase {
		return fmt.Errorf("selected node-local kubelet input uses a legacy projected configuration; apply the original ClusterConfig source with the current katlctl using 'katlctl cluster apply --config <source>' before bootstrap or shared kubelet refresh")
	}
	return nil
}

// InputFiles includes ownership metadata for persistence, not native rendering.
func (p Plan) InputFiles() ([]File, error) {
	plan, err := PlanFromRenderedFiles(p.Name, append([]File{p.Config}, p.Patches...))
	if err != nil {
		return nil, err
	}
	files := append([]File{plan.Config}, plan.Patches...)
	if !p.NodeLocalKubelet && !p.SharedKubeletBase {
		return files, nil
	}
	data, err := json.Marshal(inputMetadata{NodeLocalKubelet: p.NodeLocalKubelet, SharedKubeletBase: p.SharedKubeletBase})
	if err != nil {
		return nil, err
	}
	return append(files, File{
		RenderPath: filepath.Join("/etc/katl/kubeadm", p.Name, inputMetadataFile),
		Content:    data, Mode: 0o644,
	}), nil
}

// PlanFromInputFiles separates persisted ownership metadata from native inputs.
// Missing metadata keeps legacy inputs unproven as a shared base.
func PlanFromInputFiles(name string, files []File) (Plan, error) {
	metadataPath := filepath.Join("/etc/katl/kubeadm", name, inputMetadataFile)
	var native []File
	var metadata inputMetadata
	found := false
	for _, file := range files {
		if file.RenderPath != metadataPath {
			native = append(native, file)
			continue
		}
		if found {
			return Plan{}, fmt.Errorf("duplicate kubeadm input metadata")
		}
		if err := json.Unmarshal(file.Content, &metadata); err != nil {
			return Plan{}, fmt.Errorf("decode kubeadm input metadata: %w", err)
		}
		found = true
	}
	plan, err := PlanFromRenderedFiles(name, native)
	if err != nil {
		return Plan{}, err
	}
	plan.NodeLocalKubelet = metadata.NodeLocalKubelet
	plan.SharedKubeletBase = metadata.SharedKubeletBase
	return plan, nil
}

// InputDigest binds native files and ownership in one snapshot. Inputs without
// metadata retain the digest used by previously installed cluster intent.
func (p Plan) InputDigest() (string, error) {
	files, err := p.InputFiles()
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	for i, file := range files {
		kind := "patch"
		if i == 0 {
			kind = "config"
		} else if filepath.Base(file.RenderPath) == inputMetadataFile && filepath.Dir(file.RenderPath) == filepath.Dir(p.Config.RenderPath) {
			kind = "metadata"
		}
		fmt.Fprintf(digest, "%s\x00%s\x00%#o\x00", kind, file.RenderPath, file.Mode)
		digest.Write(file.Content)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
