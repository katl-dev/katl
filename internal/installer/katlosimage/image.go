package katlosimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Resolver struct {
	MediaRoot string
	WorkDir   string
	Commands  CommandRunner
	Client    HTTPClient
}

func (r Resolver) ResolveKatlosImage(ctx context.Context, expected manifest.KatlosImage) (Payload, error) {
	switch {
	case strings.TrimSpace(expected.LocalRef) != "":
		return (LocalResolver{
			MediaRoot: r.MediaRoot,
			WorkDir:   r.WorkDir,
			Commands:  r.Commands,
		}).ResolveKatlosImage(ctx, expected)
	case strings.TrimSpace(expected.URL) != "":
		return (RemoteResolver{
			WorkDir:  r.WorkDir,
			Commands: r.Commands,
			Client:   r.Client,
		}).ResolveKatlosImage(ctx, expected)
	default:
		return Payload{}, fmt.Errorf("KatlOS image URL or localRef is required")
	}
}

type LocalResolver struct {
	MediaRoot string
	WorkDir   string
	Commands  CommandRunner
}

func (r LocalResolver) ResolveKatlosImage(ctx context.Context, expected manifest.KatlosImage) (Payload, error) {
	if strings.TrimSpace(expected.LocalRef) == "" {
		return Payload{}, fmt.Errorf("KatlOS image localRef is required for local resolver")
	}
	mediaRoot := r.MediaRoot
	if mediaRoot == "" {
		mediaRoot = "/"
	}
	imagePath, err := cleanLocalRef(mediaRoot, expected.LocalRef)
	if err != nil {
		return Payload{}, err
	}
	info, err := os.Stat(imagePath)
	if err != nil {
		return Payload{}, fmt.Errorf("stat KatlOS image localRef: %w", err)
	}
	if info.IsDir() {
		return ResolveDirectory(ctx, imagePath, expected)
	}
	if !info.Mode().IsRegular() {
		return Payload{}, fmt.Errorf("KatlOS image localRef %q is not a regular file", expected.LocalRef)
	}
	if expected.SizeBytes > 0 && uint64(info.Size()) != expected.SizeBytes {
		return Payload{}, fmt.Errorf("KatlOS image size %d does not match manifest %d", info.Size(), expected.SizeBytes)
	}
	if err := verifyImageFile(imagePath, expected.SHA256); err != nil {
		return Payload{}, err
	}
	mountPoint, err := mountImageFile(ctx, imagePath, expected.SHA256, r.WorkDir, r.Commands)
	if err != nil {
		return Payload{}, err
	}
	return ResolveDirectory(ctx, mountPoint, expected)
}

type RemoteResolver struct {
	WorkDir  string
	Commands CommandRunner
	Client   HTTPClient
}

func (r RemoteResolver) ResolveKatlosImage(ctx context.Context, expected manifest.KatlosImage) (Payload, error) {
	if strings.TrimSpace(expected.URL) == "" {
		return Payload{}, fmt.Errorf("KatlOS image URL is required for remote resolver")
	}
	workDir := r.WorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "katlos-image")
	}
	imagePath, err := downloadImage(ctx, expected, workDir, r.Client)
	if err != nil {
		return Payload{}, err
	}
	mountPoint, err := mountImageFile(ctx, imagePath, expected.SHA256, workDir, r.Commands)
	if err != nil {
		return Payload{}, err
	}
	return ResolveDirectory(ctx, mountPoint, expected)
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

func cleanLocalRef(root string, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("KatlOS image localRef is required")
	}
	if filepath.IsAbs(ref) {
		return "", fmt.Errorf("KatlOS image localRef %q must be relative", ref)
	}
	clean := path.Clean(ref)
	if clean != ref || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("KatlOS image localRef %q must be a clean relative path", ref)
	}
	return filepath.Join(filepath.Clean(root), filepath.FromSlash(clean)), nil
}

func verifyImageFile(imagePath string, expectedSHA256 string) error {
	if err := validateSHA256(expectedSHA256); err != nil {
		return fmt.Errorf("KatlOS image SHA-256 is invalid: %w", err)
	}
	file, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("open KatlOS image: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash KatlOS image: %w", err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != expectedSHA256 {
		return fmt.Errorf("KatlOS image digest %s does not match manifest %s", got, expectedSHA256)
	}
	return nil
}

func downloadImage(ctx context.Context, expected manifest.KatlosImage, workDir string, client HTTPClient) (string, error) {
	if err := validateSHA256(expected.SHA256); err != nil {
		return "", fmt.Errorf("KatlOS image SHA-256 is invalid: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, expected.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create KatlOS image request: %w", err)
	}
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch KatlOS image: %w", err)
	}
	if response == nil {
		return "", fmt.Errorf("fetch KatlOS image: empty response")
	}
	if response.Body == nil {
		return "", fmt.Errorf("fetch KatlOS image: empty response body")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch KatlOS image: status %s", response.Status)
	}
	downloadDir := filepath.Join(workDir, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", fmt.Errorf("create KatlOS image download dir: %w", err)
	}
	imagePath := filepath.Join(downloadDir, expected.SHA256+".squashfs")
	file, err := os.OpenFile(imagePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create KatlOS image download: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(file, io.TeeReader(response.Body, hash))
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("download KatlOS image: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close KatlOS image download: %w", closeErr)
	}
	if expected.SizeBytes > 0 && uint64(written) != expected.SizeBytes {
		return "", fmt.Errorf("KatlOS image size %d does not match manifest %d", written, expected.SizeBytes)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != expected.SHA256 {
		return "", fmt.Errorf("KatlOS image digest %s does not match manifest %s", got, expected.SHA256)
	}
	return imagePath, nil
}

func mountImageFile(ctx context.Context, imagePath string, digest string, workDir string, commands CommandRunner) (string, error) {
	if commands == nil {
		return "", fmt.Errorf("mount command runner is required")
	}
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "katlos-image")
	}
	mountPoint := filepath.Join(workDir, "mounts", digest)
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return "", fmt.Errorf("create KatlOS image mountpoint: %w", err)
	}
	if err := commands.Run(ctx, "mount", "-o", "ro,loop", imagePath, mountPoint); err != nil {
		return "", fmt.Errorf("mount KatlOS image: %w", err)
	}
	return mountPoint, nil
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
	return generation.FirstInstallRequest{
		GenerationID:          request.GenerationID,
		RuntimeVersion:        first(p.Runtime.Version, p.Index.Version),
		RuntimeInterface:      p.Index.RuntimeInterface,
		RuntimeArchitecture:   p.Index.Architecture,
		RootSlot:              request.RootSlot,
		RootPartitionUUID:     request.RootPartitionUUID,
		RuntimeArtifactSHA256: p.Runtime.SHA256,
		UKIPath:               request.UKIPath,
		KernelCommandLine:     append([]string(nil), p.Boot.Compatibility.KernelCommandLine...),
		CreatedAt:             request.CreatedAt,
	}, nil
}

type FirstInstallRequest struct {
	GenerationID      string
	RootSlot          string
	RootPartitionUUID string
	UKIPath           string
	CreatedAt         time.Time
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
