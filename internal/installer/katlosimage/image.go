package katlosimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer/artifact"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/manifest"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "KatlOSImage"

	RoleInstall          = "install"
	FormatSquashFS       = "squashfs"
	ComponentRuntimeRoot = "runtime-root"
	ComponentRuntimeUKI  = "runtime-uki"
	ComponentKubernetes  = "kubernetes-sysext"
)

type Payload struct {
	Root       string
	Index      Index
	Runtime    Component
	Boot       Component
	Kubernetes Component
}

type DirectoryResolver struct {
	Root string
}

func (r DirectoryResolver) ResolveKatlosImage(ctx context.Context, expected manifest.KatlosImage) (Payload, error) {
	return ResolveDirectory(ctx, r.Root, expected)
}

func (p Payload) RuntimeArtifact() artifact.ArtifactVerification {
	return componentArtifact(p.Runtime, artifact.ArtifactRuntimeRoot)
}

func (p Payload) ComponentPath(component Component) string {
	return filepath.Join(p.Root, filepath.FromSlash(component.Path))
}

func componentArtifact(component Component, kind artifact.ArtifactKind) artifact.ArtifactVerification {
	return artifact.ArtifactVerification{
		Name:      component.Name,
		Kind:      kind,
		SHA256:    component.SHA256,
		SizeBytes: component.SizeBytes,
	}
}

type Index struct {
	APIVersion       string      `json:"apiVersion"`
	Kind             string      `json:"kind"`
	ImageRole        string      `json:"imageRole"`
	Format           string      `json:"format"`
	Version          string      `json:"version"`
	BuildID          string      `json:"buildID"`
	Architecture     string      `json:"architecture"`
	RuntimeInterface string      `json:"runtimeInterface"`
	CreatedAt        string      `json:"createdAt"`
	Components       []Component `json:"components"`
}

type Component struct {
	Name            string            `json:"name"`
	Role            string            `json:"role"`
	Path            string            `json:"path"`
	Format          string            `json:"format"`
	SizeBytes       int64             `json:"sizeBytes"`
	SHA256          string            `json:"sha256"`
	Version         string            `json:"version"`
	PayloadVersion  string            `json:"payloadVersion,omitempty"`
	Architecture    string            `json:"architecture"`
	Compatibility   Compatibility     `json:"compatibility"`
	SourceRepo      *SourceRepo       `json:"sourceRepo,omitempty"`
	PackageVersions map[string]string `json:"packageVersions,omitempty"`
	InstallTarget   InstallTarget     `json:"installTarget"`
}

type Compatibility struct {
	RuntimeInterface  string          `json:"runtimeInterface"`
	Boot              json.RawMessage `json:"boot,omitempty"`
	RuntimeRoot       RuntimeRoot     `json:"runtimeRoot,omitempty"`
	KernelCommandLine []string        `json:"kernelCommandLine,omitempty"`
}

type RuntimeRoot struct {
	Interface      string `json:"interface,omitempty"`
	ArtifactPath   string `json:"artifactPath,omitempty"`
	ArtifactSHA256 string `json:"artifactSHA256,omitempty"`
}

type SourceRepo struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Minor   string `json:"minor"`
}

type InstallTarget struct {
	Kind         string `json:"kind"`
	Filesystem   string `json:"filesystem,omitempty"`
	MinSizeBytes int64  `json:"minSizeBytes,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Name         string `json:"name,omitempty"`
}

func ResolveDirectory(ctx context.Context, root string, expected manifest.KatlosImage) (Payload, error) {
	root = filepath.Clean(root)
	index, err := readIndex(filepath.Join(root, "katlos", "image.json"))
	if err != nil {
		return Payload{}, err
	}
	return validate(ctx, root, index, expected)
}

func (p Payload) FirstInstallRequest(request FirstInstallRequest) (generation.FirstInstallRequest, error) {
	if strings.TrimSpace(request.GenerationID) == "" {
		return generation.FirstInstallRequest{}, fmt.Errorf("generation id is required")
	}
	if strings.TrimSpace(request.RootSlot) == "" {
		return generation.FirstInstallRequest{}, fmt.Errorf("root slot is required")
	}
	if strings.TrimSpace(request.RootPartitionUUID) == "" {
		return generation.FirstInstallRequest{}, fmt.Errorf("root partition UUID is required")
	}
	if strings.TrimSpace(request.UKIPath) == "" {
		return generation.FirstInstallRequest{}, fmt.Errorf("UKI path is required")
	}
	sysextPath := request.KubernetesSysextPath
	if sysextPath == "" {
		sysextPath = path.Join("/var/lib/katl/generations", request.GenerationID, "sysext", "kubernetes.raw")
	}
	activationPath := request.KubernetesActivationPath
	if activationPath == "" {
		activationPath = "/run/extensions/kubernetes.raw"
	}
	return generation.FirstInstallRequest{
		GenerationID:          request.GenerationID,
		RuntimeVersion:        first(p.Runtime.Version, p.Index.Version),
		RuntimeInterface:      p.Index.RuntimeInterface,
		RuntimeArchitecture:   p.Index.Architecture,
		RootSlot:              request.RootSlot,
		RootPartitionUUID:     request.RootPartitionUUID,
		RuntimeArtifactSHA256: p.Runtime.SHA256,
		UKIPath:               request.UKIPath,
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            sysextPath,
			ActivationPath:  activationPath,
			SHA256:          p.Kubernetes.SHA256,
			ArtifactVersion: p.Kubernetes.Version,
			PayloadVersion:  p.Kubernetes.PayloadVersion,
			Architecture:    p.Kubernetes.Architecture,
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{p.Index.RuntimeInterface},
			},
		}},
		KernelCommandLine: append([]string(nil), p.Boot.Compatibility.KernelCommandLine...),
		CreatedAt:         request.CreatedAt,
	}, nil
}

type FirstInstallRequest struct {
	GenerationID             string
	RootSlot                 string
	RootPartitionUUID        string
	UKIPath                  string
	KubernetesSysextPath     string
	KubernetesActivationPath string
	CreatedAt                time.Time
}

func readIndex(path string) (Index, error) {
	file, err := os.Open(path)
	if err != nil {
		return Index{}, fmt.Errorf("open KatlOS image index: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var index Index
	if err := decoder.Decode(&index); err != nil {
		return Index{}, fmt.Errorf("decode KatlOS image index: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Index{}, fmt.Errorf("decode KatlOS image index: multiple JSON values")
	}
	return index, nil
}

func validate(ctx context.Context, root string, index Index, expected manifest.KatlosImage) (Payload, error) {
	if err := validateIndex(index, expected); err != nil {
		return Payload{}, err
	}
	byRole := make(map[string]Component, len(index.Components))
	for _, component := range index.Components {
		select {
		case <-ctx.Done():
			return Payload{}, ctx.Err()
		default:
		}
		if err := validateComponent(root, component, index); err != nil {
			return Payload{}, err
		}
		if previous, ok := byRole[component.Role]; ok {
			return Payload{}, fmt.Errorf("KatlOS image component role %q appears more than once (%q and %q)", component.Role, previous.Name, component.Name)
		}
		byRole[component.Role] = component
	}
	runtime, err := required(byRole, ComponentRuntimeRoot)
	if err != nil {
		return Payload{}, err
	}
	boot, err := required(byRole, ComponentRuntimeUKI)
	if err != nil {
		return Payload{}, err
	}
	kubernetes, err := required(byRole, ComponentKubernetes)
	if err != nil {
		return Payload{}, err
	}
	if boot.Compatibility.RuntimeRoot.ArtifactSHA256 != runtime.SHA256 {
		return Payload{}, fmt.Errorf("runtime UKI root digest %q does not match runtime root %q", boot.Compatibility.RuntimeRoot.ArtifactSHA256, runtime.SHA256)
	}
	if kubernetes.Compatibility.RuntimeRoot.ArtifactSHA256 != runtime.SHA256 {
		return Payload{}, fmt.Errorf("Kubernetes sysext root digest %q does not match runtime root %q", kubernetes.Compatibility.RuntimeRoot.ArtifactSHA256, runtime.SHA256)
	}
	if len(boot.Compatibility.KernelCommandLine) == 0 {
		return Payload{}, fmt.Errorf("runtime UKI kernel command line is required")
	}
	return Payload{
		Root:       root,
		Index:      index,
		Runtime:    runtime,
		Boot:       boot,
		Kubernetes: kubernetes,
	}, nil
}

func validateIndex(index Index, expected manifest.KatlosImage) error {
	if index.APIVersion != APIVersion {
		return fmt.Errorf("KatlOS image apiVersion must be %s", APIVersion)
	}
	if index.Kind != Kind {
		return fmt.Errorf("KatlOS image kind must be %s", Kind)
	}
	if index.ImageRole != RoleInstall {
		return fmt.Errorf("KatlOS image role must be %s", RoleInstall)
	}
	if index.Format != FormatSquashFS {
		return fmt.Errorf("KatlOS image format must be %s", FormatSquashFS)
	}
	if index.Version != expected.Version {
		return fmt.Errorf("KatlOS image version %q does not match manifest %q", index.Version, expected.Version)
	}
	if index.Architecture != expected.Architecture {
		return fmt.Errorf("KatlOS image architecture %q does not match manifest %q", index.Architecture, expected.Architecture)
	}
	if strings.TrimSpace(index.RuntimeInterface) == "" {
		return fmt.Errorf("KatlOS image runtime interface is required")
	}
	if expected.RuntimeInterface != "" && index.RuntimeInterface != expected.RuntimeInterface {
		return fmt.Errorf("KatlOS image runtime interface %q does not match manifest %q", index.RuntimeInterface, expected.RuntimeInterface)
	}
	if len(index.Components) == 0 {
		return fmt.Errorf("KatlOS image components are required")
	}
	return nil
}

func validateComponent(root string, component Component, index Index) error {
	if strings.TrimSpace(component.Name) == "" {
		return fmt.Errorf("KatlOS image component name is required")
	}
	if strings.TrimSpace(component.Role) == "" {
		return fmt.Errorf("KatlOS image component %q role is required", component.Name)
	}
	if err := validateRelativePath(component.Path); err != nil {
		return fmt.Errorf("KatlOS image component %q path: %w", component.Name, err)
	}
	if component.SizeBytes <= 0 {
		return fmt.Errorf("KatlOS image component %q size must be positive", component.Name)
	}
	if err := validateSHA256(component.SHA256); err != nil {
		return fmt.Errorf("KatlOS image component %q SHA-256 is invalid: %w", component.Name, err)
	}
	if strings.TrimSpace(component.Version) == "" {
		return fmt.Errorf("KatlOS image component %q version is required", component.Name)
	}
	if component.Architecture != index.Architecture {
		return fmt.Errorf("KatlOS image component %q architecture %q does not match image %q", component.Name, component.Architecture, index.Architecture)
	}
	if component.Compatibility.RuntimeInterface != "" && component.Compatibility.RuntimeInterface != index.RuntimeInterface {
		return fmt.Errorf("KatlOS image component %q runtime interface %q does not match image %q", component.Name, component.Compatibility.RuntimeInterface, index.RuntimeInterface)
	}
	if err := validateComponentRole(component, index); err != nil {
		return err
	}
	return verifyComponentFile(filepath.Join(root, filepath.FromSlash(component.Path)), component)
}

func validateComponentRole(component Component, index Index) error {
	switch component.Role {
	case ComponentRuntimeRoot:
		if component.InstallTarget.Kind != "root-slot" {
			return fmt.Errorf("runtime root install target must be root-slot")
		}
	case ComponentRuntimeUKI:
		if component.InstallTarget.Kind != "esp-or-xbootldr" {
			return fmt.Errorf("runtime UKI install target must be esp-or-xbootldr")
		}
		if component.Compatibility.RuntimeRoot.ArtifactSHA256 == "" {
			return fmt.Errorf("runtime UKI compatible runtime digest is required")
		}
	case ComponentKubernetes:
		if component.InstallTarget.Kind != "systemd-sysext" {
			return fmt.Errorf("Kubernetes sysext install target must be systemd-sysext")
		}
		if strings.TrimSpace(component.PayloadVersion) == "" {
			return fmt.Errorf("Kubernetes sysext payload version is required")
		}
		if component.Compatibility.RuntimeRoot.ArtifactSHA256 == "" {
			return fmt.Errorf("Kubernetes sysext compatible runtime digest is required")
		}
	default:
		return fmt.Errorf("KatlOS image component %q role %q is unsupported", component.Name, component.Role)
	}
	if component.Compatibility.RuntimeInterface == "" {
		return fmt.Errorf("KatlOS image component %q runtime interface is required", component.Name)
	}
	return nil
}

func verifyComponentFile(path string, component Component) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat KatlOS image component %q: %w", component.Name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("KatlOS image component %q is not a regular file", component.Name)
	}
	if info.Size() != component.SizeBytes {
		return fmt.Errorf("KatlOS image component %q size %d does not match index %d", component.Name, info.Size(), component.SizeBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open KatlOS image component %q: %w", component.Name, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash KatlOS image component %q: %w", component.Name, err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != component.SHA256 {
		return fmt.Errorf("KatlOS image component %q digest %s does not match index %s", component.Name, got, component.SHA256)
	}
	return nil
}

func validateRelativePath(value string) error {
	if value == "" {
		return fmt.Errorf("is required")
	}
	if filepath.IsAbs(value) {
		return fmt.Errorf("%q must be relative", value)
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("%q must be a clean relative path", value)
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("must be %d lowercase hex characters", sha256.Size*2)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("must be lowercase hex")
	}
	_, err := hex.DecodeString(value)
	return err
}

func required(components map[string]Component, role string) (Component, error) {
	component, ok := components[role]
	if !ok {
		return Component{}, fmt.Errorf("KatlOS image missing required component role %q", role)
	}
	return component, nil
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
