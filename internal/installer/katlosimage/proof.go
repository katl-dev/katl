package katlosimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	installmanifest "github.com/katl-dev/katl/internal/installer/manifest"
)

type SingleImageProofRequest struct {
	ImagePath      string
	ImageSHA256    string
	ImageSizeBytes uint64
	Sysupdate      *SysupdateProof
}

type SysupdateProof struct {
	SourcePath       string `json:"sourcePath"`
	RootTransferPath string `json:"rootTransferPath"`
	UKITransferPath  string `json:"ukiTransferPath"`
	RootSourcePath   string `json:"rootSourcePath,omitempty"`
	UKISourcePath    string `json:"ukiSourcePath,omitempty"`
	RootSourceSHA256 string `json:"rootSourceSHA256,omitempty"`
	UKISourceSHA256  string `json:"ukiSourceSHA256,omitempty"`
}

type SingleImageProofReport struct {
	APIVersion     string                 `json:"apiVersion"`
	Kind           string                 `json:"kind"`
	ImagePath      string                 `json:"imagePath"`
	ImageSHA256    string                 `json:"imageSHA256,omitempty"`
	ImageSizeBytes uint64                 `json:"imageSizeBytes,omitempty"`
	EmbeddedIndex  EmbeddedIndexProof     `json:"embeddedIndex"`
	Components     []ComponentProof       `json:"components"`
	Sysupdate      *SysupdateProof        `json:"sysupdate,omitempty"`
	Verification   []VerificationEvidence `json:"verification"`
}

type EmbeddedIndexProof struct {
	Path             string `json:"path"`
	ImageRole        string `json:"imageRole"`
	Version          string `json:"version"`
	BuildID          string `json:"buildID"`
	Architecture     string `json:"architecture"`
	RuntimeInterface string `json:"runtimeInterface"`
}

type ComponentProof struct {
	Name           string `json:"name"`
	Role           string `json:"role"`
	Path           string `json:"path"`
	Format         string `json:"format"`
	SizeBytes      int64  `json:"sizeBytes"`
	SHA256         string `json:"sha256"`
	Version        string `json:"version"`
	PayloadVersion string `json:"payloadVersion,omitempty"`
	Verified       bool   `json:"verified"`
}

type VerificationEvidence struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (p Payload) SingleImageProof(request SingleImageProofRequest) (SingleImageProofReport, error) {
	imagePath := strings.TrimSpace(request.ImagePath)
	if imagePath == "" {
		imagePath = p.Root
	}
	if strings.TrimSpace(imagePath) == "" {
		return SingleImageProofReport{}, fmt.Errorf("single-image proof image path is required")
	}
	info, err := os.Stat(imagePath)
	if err != nil {
		return SingleImageProofReport{}, fmt.Errorf("stat single-image proof image path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SingleImageProofReport{}, fmt.Errorf("single-image proof image path %q is not a regular file", imagePath)
	}
	imageSHA256, err := fileSHA256(imagePath)
	if err != nil {
		return SingleImageProofReport{}, err
	}
	imageSizeBytes := uint64(info.Size())
	report := SingleImageProofReport{
		APIVersion:     APIVersion,
		Kind:           "KatlOSSingleImageProof",
		ImagePath:      imagePath,
		ImageSHA256:    imageSHA256,
		ImageSizeBytes: imageSizeBytes,
		EmbeddedIndex: EmbeddedIndexProof{
			Path:             "katlos/image.json",
			ImageRole:        p.Index.ImageRole,
			Version:          p.Index.Version,
			BuildID:          p.Index.BuildID,
			Architecture:     p.Index.Architecture,
			RuntimeInterface: p.Index.RuntimeInterface,
		},
		Components: make([]ComponentProof, 0, len(p.Index.Components)),
		Verification: []VerificationEvidence{
			{Field: "image", Message: "one KatlOS image selected as user-facing payload"},
			{Field: "embeddedIndex", Message: "component metadata loaded from katlos/image.json"},
		},
	}
	for _, component := range p.Index.Components {
		if err := verifyComponentFile(p.ComponentPath(component), component); err != nil {
			return SingleImageProofReport{}, fmt.Errorf("verify single-image component %s/%s from %s: %w", component.Role, component.Name, p.Index.ImageRole, err)
		}
		report.Components = append(report.Components, ComponentProof{
			Name:           component.Name,
			Role:           component.Role,
			Path:           component.Path,
			Format:         component.Format,
			SizeBytes:      component.SizeBytes,
			SHA256:         component.SHA256,
			Version:        component.Version,
			PayloadVersion: component.PayloadVersion,
			Verified:       true,
		})
	}
	for _, role := range []string{ComponentRuntimeRoot, ComponentRuntimeUKI} {
		if !report.hasRole(role) {
			return SingleImageProofReport{}, fmt.Errorf("single-image proof missing component role %q in image index %s", role, report.EmbeddedIndex.Path)
		}
	}
	if request.Sysupdate != nil {
		sysupdate := *request.Sysupdate
		if strings.TrimSpace(sysupdate.SourcePath) == "" {
			return SingleImageProofReport{}, fmt.Errorf("sysupdate source path is required for single-image upgrade proof")
		}
		if strings.TrimSpace(sysupdate.RootTransferPath) == "" {
			return SingleImageProofReport{}, fmt.Errorf("sysupdate root transfer path is required for single-image upgrade proof")
		}
		if strings.TrimSpace(sysupdate.UKITransferPath) == "" {
			return SingleImageProofReport{}, fmt.Errorf("sysupdate UKI transfer path is required for single-image upgrade proof")
		}
		if err := verifySysupdateSource(sysupdate.RootSourcePath, p.Runtime, "runtime-root"); err != nil {
			return SingleImageProofReport{}, err
		}
		if err := verifySysupdateSource(sysupdate.UKISourcePath, p.Boot, "runtime-uki"); err != nil {
			return SingleImageProofReport{}, err
		}
		rootSHA, err := fileSHA256(sysupdate.RootSourcePath)
		if err != nil {
			return SingleImageProofReport{}, err
		}
		ukiSHA, err := fileSHA256(sysupdate.UKISourcePath)
		if err != nil {
			return SingleImageProofReport{}, err
		}
		sysupdate.RootSourceSHA256 = rootSHA
		sysupdate.UKISourceSHA256 = ukiSHA
		if err := verifyTransfer(sysupdate.RootTransferPath, sysupdate.SourcePath, "katl_@v.root.squashfs"); err != nil {
			return SingleImageProofReport{}, fmt.Errorf("verify sysupdate runtime-root transfer: %w", err)
		}
		if err := verifyTransfer(sysupdate.UKITransferPath, sysupdate.SourcePath, "katl_@v.efi"); err != nil {
			return SingleImageProofReport{}, fmt.Errorf("verify sysupdate runtime-uki transfer: %w", err)
		}
		report.Sysupdate = &sysupdate
		report.Verification = append(
			report.Verification,
			VerificationEvidence{Field: "sysupdate.root", Message: "runtime-root component bytes match local/offline transfer source"},
			VerificationEvidence{Field: "sysupdate.uki", Message: "runtime-uki component bytes match local/offline transfer source"},
		)
	}
	return report, nil
}

func (r SingleImageProofReport) hasRole(role string) bool {
	for _, component := range r.Components {
		if component.Role == role && component.Verified {
			return true
		}
	}
	return false
}

func WriteSingleImageProof(path string, report SingleImageProofReport) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("single-image proof path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create single-image proof directory: %w", err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal single-image proof: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func verifySysupdateSource(path string, component Component, role string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("sysupdate %s source path is required for single-image upgrade proof", role)
	}
	sha, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if sha != component.SHA256 {
		return fmt.Errorf("sysupdate %s source digest %s does not match image component %s", role, sha, component.SHA256)
	}
	return nil
}

func verifyTransfer(path, sourcePath, pattern string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	if !strings.Contains(text, "Path="+sourcePath) {
		return fmt.Errorf("%s does not reference source path %q", path, sourcePath)
	}
	if !strings.Contains(text, "MatchPattern="+pattern) {
		return fmt.Errorf("%s does not reference match pattern %q", path, pattern)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func VerifyInstallManifestSingleImage(data []byte) (installmanifest.KatlosImage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return installmanifest.KatlosImage{}, fmt.Errorf("decode install manifest for single-image proof: %w", err)
	}
	for _, field := range []string{"artifacts", "runtimeRoot", "uki", "sysexts", "kubernetesSysexts"} {
		if _, ok := raw[field]; ok {
			return installmanifest.KatlosImage{}, fmt.Errorf("install manifest uses loose component field %q instead of katlosImage", field)
		}
	}
	manifest, err := installmanifest.Decode(bytes.NewReader(data))
	if err != nil {
		return installmanifest.KatlosImage{}, err
	}
	return manifest.KatlosImage, nil
}
