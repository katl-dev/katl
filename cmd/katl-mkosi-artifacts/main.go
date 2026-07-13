package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

const (
	defaultVersion            = "0.0.0-dev"
	defaultInstallerInterface = "katl-installer-boot-1"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "katl-mkosi-artifacts: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, environ []string) error {
	command := "write"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	repoRoot, err := repoRoot()
	if err != nil {
		return err
	}
	cfg := configFromEnv(envMap(environ), repoRoot)

	switch command {
	case "write":
		indexPath := cfg.DefaultIndex
		if len(args) > 1 {
			return fmt.Errorf("write accepts at most one INDEX argument")
		}
		if len(args) == 1 {
			indexPath = absPath(repoRoot, args[0])
		}
		if err := writeIndex(indexPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "artifact index: %s\n", relPath(repoRoot, indexPath))
		return nil
	case "write-installer-artifacts":
		if len(args) != 0 {
			return fmt.Errorf("write-installer-artifacts does not accept arguments")
		}
		if err := writeInstallerArtifacts(cfg); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "installer artifact metadata written")
		return nil
	case "write-runtime-index":
		indexPath := cfg.DefaultIndex
		if len(args) > 1 {
			return fmt.Errorf("write-runtime-index accepts at most one INDEX argument")
		}
		if len(args) == 1 {
			indexPath = absPath(repoRoot, args[0])
		}
		if err := writeRuntimeIndex(indexPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "runtime artifact index: %s\n", relPath(repoRoot, indexPath))
		return nil
	case "path":
		if len(args) < 1 {
			return fmt.Errorf("path requires KIND")
		}
		if len(args) > 2 {
			return fmt.Errorf("path accepts KIND and optional INDEX")
		}
		indexPath := cfg.DefaultIndex
		if len(args) == 2 {
			indexPath = absPath(repoRoot, args[1])
		}
		path, err := pathForKind(indexPath, repoRoot, args[0])
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, path)
		return nil
	case "write-runtime-root":
		return runWriteRuntimeRoot(args, stdout, stderr, cfg)
	case "write-runtime-uki":
		return runWriteRuntimeUKI(args, stdout, stderr, cfg)
	case "write-kubernetes-sysext":
		return runWriteKubernetesSysext(args, stdout, stderr, cfg)
	case "write-kubernetes-sysext-from-log":
		return runWriteKubernetesSysextFromLog(args, stdout, stderr, cfg)
	case "write-katlos-index":
		return runWriteKatlOSIndex(args, stdout, stderr, cfg)
	case "write-katlos-artifact":
		return runWriteKatlOSArtifact(args, stdout, stderr, cfg)
	case "bind-install-manifest-image":
		return runBindInstallManifestImage(args, stdout, stderr, cfg)
	case "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
}

const usage = `Usage: katl-mkosi-artifacts [write [INDEX]]
	   katl-mkosi-artifacts write-installer-artifacts
	   katl-mkosi-artifacts write-runtime-index [INDEX]
       katl-mkosi-artifacts path KIND [INDEX]
       katl-mkosi-artifacts write-runtime-root --artifact PATH
       katl-mkosi-artifacts write-runtime-uki --artifact PATH --runtime-artifact PATH --runtime-sha256 SHA --kernel-version VERSION
       katl-mkosi-artifacts write-kubernetes-sysext --artifact PATH --payload-version VERSION --kubeadm-version VERSION --kubelet-version VERSION --kubectl-version VERSION --cri-tools-version VERSION --ethtool-version VERSION --socat-version VERSION
       katl-mkosi-artifacts write-kubernetes-sysext-from-log --artifact PATH --log PATH --repo-id ID --repo-base-url URL --repo-minor MINOR
       katl-mkosi-artifacts write-katlos-index --output PATH --runtime-root PATH --runtime-root-metadata PATH --runtime-uki PATH --runtime-uki-metadata PATH
       katl-mkosi-artifacts write-katlos-artifact --artifact PATH
       katl-mkosi-artifacts bind-install-manifest-image --template PATH --output PATH

Write or query the local mkosi artifact index.

Kinds:
  installer-uki
  installer-kernel
  installer-initrd
  installer-iso
  runtime-uki
  runtime-root
  katlos-install-image
`

type config struct {
	RepoRoot             string
	DefaultIndex         string
	InstallerUKI         string
	InstallerKernel      string
	InstallerInitrd      string
	InstallerISO         string
	InstallerISOExplicit bool
	RuntimeUKI           string
	RuntimeUKIMetadata   string
	RuntimeUKIChecksum   string
	RuntimeRoot          string
	RuntimeMetadata      string
	RuntimeChecksum      string
	KatlOSImage          string
	KatlOSMetadata       string
	KatlOSChecksum       string
	KatlOSExplicit       bool
	Generation           string
	Version              string
	Architecture         string
	InstallerInterface   string
}

func configFromEnv(env map[string]string, repo string) config {
	buildDir := filepath.Join(repo, "_build", "mkosi")
	version := envDefault(env, "KATL_VERSION", defaultVersion)
	architecture := envDefaultFunc(env, "KATL_ARCHITECTURE", hostArchitecture)
	katlosDefault := filepath.Join(buildDir, "katlos-install-"+version+"-"+architecture+".squashfs")
	installerISO, installerISOExplicit := envPathExplicit(env, repo, "KATL_INSTALLER_ISO", filepath.Join(buildDir, "katl-installer.iso"))
	runtimeUKI := envPath(env, repo, "KATL_RUNTIME_UKI", filepath.Join(buildDir, "katl-runtime.efi"))
	runtimeRoot := envPath(env, repo, "KATL_RUNTIME_ARTIFACT", filepath.Join(buildDir, "katl-runtime-root.squashfs"))
	katlosImage, katlosExplicit := envPathExplicit(env, repo, "KATL_KATLOS_IMAGE", katlosDefault)

	return config{
		RepoRoot:             repo,
		DefaultIndex:         filepath.Join(buildDir, "artifacts.json"),
		InstallerUKI:         envPath(env, repo, "KATL_INSTALLER_UKI", filepath.Join(buildDir, "katl-installer.efi")),
		InstallerKernel:      envPath(env, repo, "KATL_INSTALLER_KERNEL", filepath.Join(buildDir, "katl-installer.vmlinuz")),
		InstallerInitrd:      envPath(env, repo, "KATL_INSTALLER_INITRD", filepath.Join(buildDir, "katl-installer.initrd")),
		InstallerISO:         installerISO,
		InstallerISOExplicit: installerISOExplicit,
		RuntimeUKI:           runtimeUKI,
		RuntimeUKIMetadata:   envPath(env, repo, "KATL_RUNTIME_UKI_METADATA", runtimeUKI+".json"),
		RuntimeUKIChecksum:   envPath(env, repo, "KATL_RUNTIME_UKI_CHECKSUM", runtimeUKI+".sha256"),
		RuntimeRoot:          runtimeRoot,
		RuntimeMetadata:      envPath(env, repo, "KATL_RUNTIME_METADATA", runtimeRoot+".json"),
		RuntimeChecksum:      envPath(env, repo, "KATL_RUNTIME_CHECKSUM", runtimeRoot+".sha256"),
		KatlOSImage:          katlosImage,
		KatlOSMetadata:       envPath(env, repo, "KATL_KATLOS_IMAGE_METADATA", katlosImage+".json"),
		KatlOSChecksum:       envPath(env, repo, "KATL_KATLOS_IMAGE_CHECKSUM", katlosImage+".sha256"),
		KatlOSExplicit:       katlosExplicit,
		Generation:           envDefaultFunc(env, "KATL_BUILD_COMMIT", func() string { return gitDescribe(repo) }),
		Version:              version,
		Architecture:         architecture,
		InstallerInterface:   envDefault(env, "KATL_INSTALLER_INTERFACE", defaultInstallerInterface),
	}
}

type artifactIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	GeneratedAt   string          `json:"generatedAt"`
	Generation    string          `json:"generation"`
	Artifacts     []artifactEntry `json:"artifacts"`
}

type artifactEntry struct {
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	Format       string `json:"format"`
	SizeBytes    int64  `json:"sizeBytes"`
	SHA256       string `json:"sha256"`
	MetadataPath string `json:"metadataPath,omitempty"`
	ChecksumPath string `json:"checksumPath,omitempty"`
}

type bootMetadata struct {
	APIVersion               string   `json:"apiVersion"`
	Kind                     string   `json:"kind"`
	ArtifactRole             string   `json:"artifactRole"`
	Format                   string   `json:"format"`
	Version                  string   `json:"version"`
	BuildID                  string   `json:"buildID"`
	Architecture             string   `json:"architecture"`
	Path                     string   `json:"path"`
	SizeBytes                int64    `json:"sizeBytes"`
	SHA256                   string   `json:"sha256"`
	Compression              string   `json:"compression,omitempty"`
	CreatedAt                string   `json:"createdAt"`
	InstallerInterface       string   `json:"installerInterface"`
	DefaultKernelCommandLine []string `json:"defaultKernelCommandLine"`
	SupportedInputModes      []string `json:"supportedInputModes"`
}

type localMetadata struct {
	Name              string            `json:"name"`
	Kind              string            `json:"kind"`
	Format            string            `json:"format"`
	Path              string            `json:"path"`
	SizeBytes         int64             `json:"sizeBytes"`
	SHA256            string            `json:"sha256"`
	Compression       string            `json:"compression,omitempty"`
	Generation        string            `json:"generation,omitempty"`
	Version           string            `json:"version,omitempty"`
	PayloadVersion    string            `json:"payloadVersion,omitempty"`
	Architecture      string            `json:"architecture"`
	SourceRepo        *sourceRepo       `json:"sourceRepo,omitempty"`
	PackageVersions   map[string]string `json:"packageVersions,omitempty"`
	RuntimeInterface  string            `json:"runtimeInterface"`
	CompatibleBoot    *bootCompat       `json:"compatibleBoot,omitempty"`
	CompatibleRuntime *runtimeCompat    `json:"compatibleRuntime,omitempty"`
	KernelVersion     string            `json:"kernelVersion,omitempty"`
	KernelCommandLine []string          `json:"kernelCommandLine,omitempty"`
	Created           string            `json:"created"`
}

type bootCompat struct {
	Kind              string   `json:"kind"`
	RuntimeInterface  string   `json:"runtimeInterface"`
	KernelCommandLine []string `json:"kernelCommandLine,omitempty"`
}

type runtimeCompat struct {
	Interface      string `json:"interface"`
	ArtifactPath   string `json:"artifactPath,omitempty"`
	ArtifactSHA256 string `json:"artifactSHA256,omitempty"`
}

type sourceRepo struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Minor   string `json:"minor"`
}

type katlosIndex struct {
	APIVersion       string            `json:"apiVersion"`
	Kind             string            `json:"kind"`
	ImageRole        string            `json:"imageRole"`
	Format           string            `json:"format"`
	Version          string            `json:"version"`
	BuildID          string            `json:"buildID"`
	Architecture     string            `json:"architecture"`
	RuntimeInterface string            `json:"runtimeInterface"`
	CreatedAt        string            `json:"createdAt"`
	Components       []katlosComponent `json:"components"`
}

type katlosComponent struct {
	Name            string              `json:"name"`
	Role            string              `json:"role"`
	Path            string              `json:"path"`
	Format          string              `json:"format"`
	SizeBytes       int64               `json:"sizeBytes"`
	SHA256          string              `json:"sha256"`
	Version         string              `json:"version"`
	PayloadVersion  string              `json:"payloadVersion,omitempty"`
	Architecture    string              `json:"architecture"`
	Compatibility   katlosCompatibility `json:"compatibility"`
	SourceRepo      *sourceRepo         `json:"sourceRepo,omitempty"`
	PackageVersions map[string]string   `json:"packageVersions,omitempty"`
	InstallTarget   installTarget       `json:"installTarget"`
}

type katlosCompatibility struct {
	RuntimeInterface  string         `json:"runtimeInterface"`
	Boot              *bootCompat    `json:"boot,omitempty"`
	RuntimeRoot       *runtimeCompat `json:"runtimeRoot,omitempty"`
	KernelCommandLine []string       `json:"kernelCommandLine,omitempty"`
}

type installTarget struct {
	Kind         string `json:"kind"`
	Filesystem   string `json:"filesystem,omitempty"`
	MinSizeBytes int64  `json:"minSizeBytes,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Name         string `json:"name,omitempty"`
}

type katlosArtifactMetadata struct {
	APIVersion        string `json:"apiVersion"`
	Kind              string `json:"kind"`
	ImageRole         string `json:"imageRole"`
	Format            string `json:"format"`
	Version           string `json:"version"`
	BuildID           string `json:"buildID"`
	Architecture      string `json:"architecture"`
	RuntimeInterface  string `json:"runtimeInterface"`
	Path              string `json:"path"`
	SizeBytes         int64  `json:"sizeBytes"`
	SHA256            string `json:"sha256"`
	ChecksumPath      string `json:"checksumPath"`
	EmbeddedIndexPath string `json:"embeddedIndexPath"`
	CreatedAt         string `json:"createdAt"`
}

func runWriteRuntimeRoot(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts write-runtime-root", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs"), "runtime root SquashFS artifact")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	artifactPath := absPath(cfg.RepoRoot, *artifact)
	size, digest, err := fileInfo(artifactPath)
	if err != nil {
		return err
	}
	if err := writeChecksum(artifactPath); err != nil {
		return err
	}
	metadata := localMetadata{
		Name:             "runtime-root",
		Kind:             "runtime-root",
		Format:           "squashfs",
		Path:             filepath.Base(artifactPath),
		SizeBytes:        size,
		SHA256:           digest,
		Compression:      "zstd",
		Generation:       cfg.Generation,
		Architecture:     cfg.Architecture,
		RuntimeInterface: "katl-runtime-1",
		CompatibleBoot: &bootCompat{
			Kind:              "uki",
			RuntimeInterface:  "katl-runtime-1",
			KernelCommandLine: []string{"rootfstype=squashfs", "ro"},
		},
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSON(metadataPath(artifactPath), metadata, cfg.RepoRoot); err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

func runWriteRuntimeUKI(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts write-runtime-uki", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", filepath.Join("_build", "mkosi", "katl-runtime.efi"), "runtime UKI artifact")
	runtimeArtifact := flags.String("runtime-artifact", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs"), "compatible runtime root artifact")
	runtimeSHA := flags.String("runtime-sha256", "", "compatible runtime root SHA-256")
	kernelVersion := flags.String("kernel-version", "", "runtime kernel version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*runtimeSHA) == "" {
		return fmt.Errorf("--runtime-sha256 is required")
	}
	if strings.TrimSpace(*kernelVersion) == "" {
		return fmt.Errorf("--kernel-version is required")
	}

	artifactPath := absPath(cfg.RepoRoot, *artifact)
	runtimePath := absPath(cfg.RepoRoot, *runtimeArtifact)
	size, digest, err := fileInfo(artifactPath)
	if err != nil {
		return err
	}
	if err := writeChecksum(artifactPath); err != nil {
		return err
	}
	metadata := localMetadata{
		Name:             "runtime-uki",
		Kind:             "runtime-uki",
		Format:           "uki",
		Path:             filepath.Base(artifactPath),
		SizeBytes:        size,
		SHA256:           digest,
		Version:          cfg.Generation,
		Architecture:     cfg.Architecture,
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &runtimeCompat{
			Interface:      "katl-runtime-1",
			ArtifactPath:   filepath.Base(runtimePath),
			ArtifactSHA256: *runtimeSHA,
		},
		KernelVersion:     *kernelVersion,
		KernelCommandLine: []string{"rootfstype=squashfs", "ro"},
		Created:           time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSON(metadataPath(artifactPath), metadata, cfg.RepoRoot); err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

func runWriteKubernetesSysext(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts write-kubernetes-sysext", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", filepath.Join("_build", "mkosi", "katl-kubernetes.raw"), "Kubernetes sysext artifact")
	payloadVersion := flags.String("payload-version", "", "Kubernetes payload version")
	kubeadmVersion := flags.String("kubeadm-version", "", "resolved kubeadm package version")
	kubeletVersion := flags.String("kubelet-version", "", "resolved kubelet package version")
	kubectlVersion := flags.String("kubectl-version", "", "resolved kubectl package version")
	criToolsVersion := flags.String("cri-tools-version", "", "resolved cri-tools package version")
	ethtoolVersion := flags.String("ethtool-version", "", "resolved ethtool package version")
	socatVersion := flags.String("socat-version", "", "resolved socat package version")
	runtimeArtifact := flags.String("runtime-artifact", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs"), "compatible runtime root artifact")
	runtimeMetadata := flags.String("runtime-metadata", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs.json"), "compatible runtime root metadata")
	runtimeSHA := flags.String("runtime-sha256", "", "compatible runtime root SHA-256 override")
	repoID := flags.String("repo-id", "", "Kubernetes package repository ID")
	repoBaseURL := flags.String("repo-base-url", "", "Kubernetes package repository base URL")
	repoMinor := flags.String("repo-minor", "", "Kubernetes package minor")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	for name, value := range map[string]string{
		"--payload-version":   *payloadVersion,
		"--kubeadm-version":   *kubeadmVersion,
		"--kubelet-version":   *kubeletVersion,
		"--kubectl-version":   *kubectlVersion,
		"--cri-tools-version": *criToolsVersion,
		"--ethtool-version":   *ethtoolVersion,
		"--socat-version":     *socatVersion,
		"--repo-id":           *repoID,
		"--repo-base-url":     *repoBaseURL,
		"--repo-minor":        *repoMinor,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	artifactPath := absPath(cfg.RepoRoot, *artifact)
	runtimePath := absPath(cfg.RepoRoot, *runtimeArtifact)
	digest, err := writeKubernetesSysextMetadata(kubernetesSysextRequest{
		ArtifactPath:    artifactPath,
		PayloadVersion:  *payloadVersion,
		RuntimePath:     runtimePath,
		RuntimeMetadata: absPath(cfg.RepoRoot, *runtimeMetadata),
		RuntimeSHA:      *runtimeSHA,
		Repo: sourceRepo{
			ID:      *repoID,
			BaseURL: *repoBaseURL,
			Minor:   *repoMinor,
		},
		Packages: map[string]string{
			"kubeadm":   *kubeadmVersion,
			"kubelet":   *kubeletVersion,
			"kubectl":   *kubectlVersion,
			"cri-tools": *criToolsVersion,
			"ethtool":   *ethtoolVersion,
			"socat":     *socatVersion,
		},
	}, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

func runWriteKubernetesSysextFromLog(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts write-kubernetes-sysext-from-log", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", filepath.Join("_build", "mkosi", "katl-kubernetes.raw"), "Kubernetes sysext artifact")
	logPath := flags.String("log", "", "mkosi output log containing resolved package lines")
	runtimeArtifact := flags.String("runtime-artifact", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs"), "compatible runtime root artifact")
	runtimeMetadata := flags.String("runtime-metadata", filepath.Join("_build", "mkosi", "katl-runtime-root.squashfs.json"), "compatible runtime root metadata")
	repoID := flags.String("repo-id", "", "Kubernetes package repository ID")
	repoBaseURL := flags.String("repo-base-url", "", "Kubernetes package repository base URL")
	repoMinor := flags.String("repo-minor", "", "Kubernetes package minor")
	expectedPayload := flags.String("expected-payload-version", "", "optional expected Kubernetes payload version")
	expectedKubeadm := flags.String("expected-kubeadm-version", "", "optional expected kubeadm package version")
	expectedKubelet := flags.String("expected-kubelet-version", "", "optional expected kubelet package version")
	expectedKubectl := flags.String("expected-kubectl-version", "", "optional expected kubectl package version")
	expectedCriTools := flags.String("expected-cri-tools-version", "", "optional expected cri-tools package version")
	expectedEthtool := flags.String("expected-ethtool-version", "", "optional expected ethtool package version")
	expectedSocat := flags.String("expected-socat-version", "", "optional expected socat package version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	for name, value := range map[string]string{
		"--log":           *logPath,
		"--repo-id":       *repoID,
		"--repo-base-url": *repoBaseURL,
		"--repo-minor":    *repoMinor,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	packages, err := resolveKubernetesPackages(*logPath, *repoID)
	if err != nil {
		return err
	}
	expected := map[string]string{
		"kubeadm":   *expectedKubeadm,
		"kubelet":   *expectedKubelet,
		"kubectl":   *expectedKubectl,
		"cri-tools": *expectedCriTools,
		"ethtool":   *expectedEthtool,
		"socat":     *expectedSocat,
	}
	for pkg, want := range expected {
		if strings.TrimSpace(want) != "" && packages[pkg] != want {
			return fmt.Errorf("%s resolved as %s, want %s", pkg, packages[pkg], want)
		}
	}
	payloadVersion, err := payloadVersionFromPackage(packages["kubeadm"])
	if err != nil {
		return err
	}
	if strings.TrimSpace(*expectedPayload) != "" && payloadVersion != *expectedPayload {
		return fmt.Errorf("Kubernetes payload version %s resolved from kubeadm, want %s", payloadVersion, *expectedPayload)
	}
	if !strings.HasPrefix(payloadVersion, *repoMinor+".") {
		return fmt.Errorf("Kubernetes payload version %s does not match selected minor %s", payloadVersion, *repoMinor)
	}

	digest, err := writeKubernetesSysextMetadata(kubernetesSysextRequest{
		ArtifactPath:    absPath(cfg.RepoRoot, *artifact),
		PayloadVersion:  payloadVersion,
		RuntimePath:     absPath(cfg.RepoRoot, *runtimeArtifact),
		RuntimeMetadata: absPath(cfg.RepoRoot, *runtimeMetadata),
		Repo: sourceRepo{
			ID:      *repoID,
			BaseURL: *repoBaseURL,
			Minor:   *repoMinor,
		},
		Packages: packages,
	}, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

type kubernetesSysextRequest struct {
	ArtifactPath    string
	PayloadVersion  string
	RuntimePath     string
	RuntimeMetadata string
	RuntimeSHA      string
	Repo            sourceRepo
	Packages        map[string]string
}

func writeKubernetesSysextMetadata(req kubernetesSysextRequest, cfg config) (string, error) {
	sha, err := resolveRuntimeSHA(cfg.RepoRoot, req.RuntimeSHA, req.RuntimeMetadata)
	if err != nil {
		return "", err
	}
	size, digest, err := fileInfo(req.ArtifactPath)
	if err != nil {
		return "", err
	}
	if err := writeChecksum(req.ArtifactPath); err != nil {
		return "", err
	}
	metadata := localMetadata{
		Name:             "kubernetes",
		Kind:             "sysext",
		Format:           "sysext",
		Path:             filepath.Base(req.ArtifactPath),
		SizeBytes:        size,
		SHA256:           digest,
		Version:          cfg.Generation,
		PayloadVersion:   req.PayloadVersion,
		Architecture:     cfg.Architecture,
		SourceRepo:       &req.Repo,
		PackageVersions:  req.Packages,
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &runtimeCompat{
			Interface:      "katl-runtime-1",
			ArtifactPath:   filepath.Base(req.RuntimePath),
			ArtifactSHA256: sha,
		},
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSON(metadataPath(req.ArtifactPath), metadata, cfg.RepoRoot); err != nil {
		return "", err
	}
	return digest, nil
}

func resolveKubernetesPackages(logPath, repoID string) (map[string]string, error) {
	file, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("open mkosi log %s: %w", logPath, err)
	}
	defer file.Close()

	wanted := map[string]string{
		"kubeadm":   "",
		"kubelet":   "",
		"kubectl":   "",
		"cri-tools": "",
		"ethtool":   "",
		"socat":     "",
	}
	repos := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if _, ok := wanted[fields[0]]; !ok {
			continue
		}
		wanted[fields[0]] = fields[2]
		repos[fields[0]] = fields[3]
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mkosi log %s: %w", logPath, err)
	}
	for pkg, version := range wanted {
		if version == "" {
			return nil, fmt.Errorf("could not determine resolved version for %s", pkg)
		}
		if isKubernetesRepoPackage(pkg) && repos[pkg] != repoID {
			return nil, fmt.Errorf("%s resolved from %s, want %s", pkg, repos[pkg], repoID)
		}
	}
	return wanted, nil
}

func isKubernetesRepoPackage(name string) bool {
	switch name {
	case "kubeadm", "kubelet", "kubectl", "cri-tools":
		return true
	default:
		return false
	}
}

func payloadVersionFromPackage(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	if idx := strings.Index(trimmed, "-"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("could not derive Kubernetes payload version from package version %s", version)
	}
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("could not derive Kubernetes payload version from package version %s", version)
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return "", fmt.Errorf("could not derive Kubernetes payload version from package version %s", version)
			}
		}
	}
	return "v" + trimmed, nil
}

func runWriteKatlOSIndex(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts write-katlos-index", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", filepath.Join("_build", "mkosi", "katlos-install-root", "katlos", "image.json"), "embedded KatlOS image index output")
	imageRole := flags.String("image-role", "install", "KatlOS image role")
	version := flags.String("version", cfg.Version, "KatlOS image version")
	buildID := flags.String("build-id", cfg.Generation, "KatlOS image build ID")
	architecture := flags.String("architecture", cfg.Architecture, "KatlOS image architecture")
	runtimeInterface := flags.String("runtime-interface", "katl-runtime-1", "KatlOS runtime interface")
	runtimeRoot := flags.String("runtime-root", cfg.RuntimeRoot, "runtime root artifact")
	runtimeRootMetadata := flags.String("runtime-root-metadata", cfg.RuntimeMetadata, "runtime root metadata")
	runtimeUKI := flags.String("runtime-uki", cfg.RuntimeUKI, "runtime UKI artifact")
	runtimeUKIMetadata := flags.String("runtime-uki-metadata", cfg.RuntimeUKIMetadata, "runtime UKI metadata")
	rootPath := flags.String("root-path", "components/runtime/root.squashfs", "embedded runtime root component path")
	ukiPath := flags.String("uki-path", "components/boot/katl.efi", "embedded runtime UKI component path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *imageRole != "install" && *imageRole != "upgrade" {
		return fmt.Errorf("unsupported KatlOS image role: %s", *imageRole)
	}
	if strings.TrimSpace(*runtimeInterface) == "" {
		return fmt.Errorf("--runtime-interface is required")
	}

	rootArtifact := absPath(cfg.RepoRoot, *runtimeRoot)
	ukiArtifact := absPath(cfg.RepoRoot, *runtimeUKI)
	rootMeta, err := readAndValidateLocalMetadata("runtime root", absPath(cfg.RepoRoot, *runtimeRootMetadata), rootArtifact)
	if err != nil {
		return err
	}
	ukiMeta, err := readAndValidateLocalMetadata("runtime UKI", absPath(cfg.RepoRoot, *runtimeUKIMetadata), ukiArtifact)
	if err != nil {
		return err
	}
	if err := validateKatlOSComponents(rootMeta, ukiMeta, *architecture, *runtimeInterface); err != nil {
		return err
	}

	if err := writeComponentChecksum(filepath.Join(filepath.Dir(filepath.Dir(absPath(cfg.RepoRoot, *output))), "components", "metadata", "runtime-root.sha256"), rootMeta.SHA256, "../runtime/root.squashfs"); err != nil {
		return err
	}
	if err := writeComponentChecksum(filepath.Join(filepath.Dir(filepath.Dir(absPath(cfg.RepoRoot, *output))), "components", "metadata", "runtime-uki.sha256"), ukiMeta.SHA256, "../boot/katl.efi"); err != nil {
		return err
	}

	index := katlosIndex{
		APIVersion:       "katl.dev/v1alpha1",
		Kind:             "KatlOSImage",
		ImageRole:        *imageRole,
		Format:           "squashfs",
		Version:          *version,
		BuildID:          *buildID,
		Architecture:     *architecture,
		RuntimeInterface: *runtimeInterface,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		Components: []katlosComponent{
			{
				Name:         "runtime-root",
				Role:         "runtime-root",
				Path:         *rootPath,
				Format:       rootMeta.Format,
				SizeBytes:    rootMeta.SizeBytes,
				SHA256:       rootMeta.SHA256,
				Version:      firstNonEmpty(rootMeta.Generation, rootMeta.Version, *buildID),
				Architecture: rootMeta.Architecture,
				Compatibility: katlosCompatibility{
					RuntimeInterface: rootMeta.RuntimeInterface,
					Boot:             rootMeta.CompatibleBoot,
				},
				InstallTarget: installTarget{
					Kind:         "root-slot",
					Filesystem:   rootMeta.Format,
					MinSizeBytes: rootMeta.SizeBytes,
				},
			},
			{
				Name:         "runtime-uki",
				Role:         "runtime-uki",
				Path:         *ukiPath,
				Format:       ukiMeta.Format,
				SizeBytes:    ukiMeta.SizeBytes,
				SHA256:       ukiMeta.SHA256,
				Version:      ukiMeta.Version,
				Architecture: ukiMeta.Architecture,
				Compatibility: katlosCompatibility{
					RuntimeInterface:  ukiMeta.RuntimeInterface,
					RuntimeRoot:       ukiMeta.CompatibleRuntime,
					KernelCommandLine: ukiMeta.KernelCommandLine,
				},
				InstallTarget: installTarget{
					Kind:     "esp-or-xbootldr",
					Filename: "katl.efi",
				},
			},
		},
	}
	if err := writeJSON(absPath(cfg.RepoRoot, *output), index, cfg.RepoRoot); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "katlos index: %s\n", relPath(cfg.RepoRoot, absPath(cfg.RepoRoot, *output)))
	return nil
}

func runWriteKatlOSArtifact(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts write-katlos-artifact", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", cfg.KatlOSImage, "KatlOS image artifact")
	imageRole := flags.String("image-role", "install", "KatlOS image role")
	version := flags.String("version", cfg.Version, "KatlOS image version")
	buildID := flags.String("build-id", cfg.Generation, "KatlOS image build ID")
	architecture := flags.String("architecture", cfg.Architecture, "KatlOS image architecture")
	runtimeInterface := flags.String("runtime-interface", "katl-runtime-1", "KatlOS runtime interface")
	embeddedIndexPath := flags.String("embedded-index-path", "katlos/image.json", "embedded KatlOS image index path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *imageRole != "install" && *imageRole != "upgrade" {
		return fmt.Errorf("unsupported KatlOS image role: %s", *imageRole)
	}
	artifactPath := absPath(cfg.RepoRoot, *artifact)
	size, digest, err := fileInfo(artifactPath)
	if err != nil {
		return err
	}
	if err := writeChecksum(artifactPath); err != nil {
		return err
	}
	metadata := katlosArtifactMetadata{
		APIVersion:        "katl.dev/v1alpha1",
		Kind:              "KatlOSImageArtifact",
		ImageRole:         *imageRole,
		Format:            "squashfs",
		Version:           *version,
		BuildID:           *buildID,
		Architecture:      *architecture,
		RuntimeInterface:  *runtimeInterface,
		Path:              filepath.Base(artifactPath),
		SizeBytes:         size,
		SHA256:            digest,
		ChecksumPath:      filepath.Base(artifactPath) + ".sha256",
		EmbeddedIndexPath: *embeddedIndexPath,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSON(metadataPath(artifactPath), metadata, cfg.RepoRoot); err != nil {
		return err
	}
	fmt.Fprintln(stdout, digest)
	return nil
}

func runBindInstallManifestImage(args []string, stdout, stderr io.Writer, cfg config) error {
	flags := flag.NewFlagSet("katl-mkosi-artifacts bind-install-manifest-image", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifactIndex := flags.String("artifact-index", cfg.DefaultIndex, "mkosi artifact index")
	template := flags.String("template", "", "input install manifest template")
	output := flags.String("output", "", "output install manifest")
	localRef := flags.String("local-ref", "", "clean relative image ref")
	targetDiskByID := flags.String("target-disk-by-id", "", "optional install.targetDisk.byID override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*template) == "" {
		return fmt.Errorf("--template is required")
	}
	if strings.TrimSpace(*output) == "" {
		return fmt.Errorf("--output is required")
	}
	if strings.TrimSpace(*targetDiskByID) != "" && !strings.HasPrefix(*targetDiskByID, "/dev/disk/by-id/") {
		return fmt.Errorf("--target-disk-by-id must be a /dev/disk/by-id path")
	}

	indexPath := absPath(cfg.RepoRoot, *artifactIndex)
	templatePath := absPath(cfg.RepoRoot, *template)
	outputPath := absPath(cfg.RepoRoot, *output)
	if filepath.Clean(templatePath) == filepath.Clean(outputPath) {
		return fmt.Errorf("output must not replace template in place")
	}
	image, metadata, err := katlosImageFromIndex(indexPath, cfg.RepoRoot)
	if err != nil {
		return err
	}
	ref := strings.TrimSpace(*localRef)
	if ref == "" {
		ref = filepath.Base(image)
	}
	if err := validateLocalRef(ref); err != nil {
		return err
	}

	data, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read template manifest %s: %w", relPath(cfg.RepoRoot, templatePath), err)
	}
	install, err := decodeInstallManifestTemplate(data)
	if err != nil {
		return fmt.Errorf("decode template manifest %s: %w", relPath(cfg.RepoRoot, templatePath), err)
	}
	install.KatlosImage = manifest.KatlosImage{
		LocalRef:         ref,
		SHA256:           metadata.SHA256,
		SizeBytes:        uint64(metadata.SizeBytes),
		Version:          metadata.Version,
		Architecture:     metadata.Architecture,
		RuntimeInterface: metadata.RuntimeInterface,
		Role:             metadata.ImageRole,
	}
	if strings.TrimSpace(*targetDiskByID) != "" {
		install.Install.TargetDisk.ByID = strings.TrimSpace(*targetDiskByID)
		install.Install.TargetDisk.WWN = ""
		install.Install.TargetDisk.Serial = ""
	}
	if err := manifest.Validate(install); err != nil {
		return fmt.Errorf("bound install manifest is invalid: %w", err)
	}
	out, err := yaml.Marshal(install)
	if err != nil {
		return fmt.Errorf("marshal bound install manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output manifest directory: %w", err)
	}
	if err := os.WriteFile(outputPath, out, 0o644); err != nil {
		return fmt.Errorf("write output manifest %s: %w", relPath(cfg.RepoRoot, outputPath), err)
	}

	link := filepath.Join(filepath.Dir(outputPath), filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create localRef directory: %w", err)
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace localRef %s: %w", relPath(cfg.RepoRoot, link), err)
	}
	if err := os.Symlink(image, link); err != nil {
		return fmt.Errorf("create localRef %s: %w", relPath(cfg.RepoRoot, link), err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return fmt.Errorf("resolve localRef %s: %w", relPath(cfg.RepoRoot, link), err)
	}
	if filepath.Clean(resolved) != filepath.Clean(image) {
		return fmt.Errorf("localRef %s does not resolve to indexed image", relPath(cfg.RepoRoot, link))
	}

	fmt.Fprintf(stdout, "install manifest: %s\n", relPath(cfg.RepoRoot, outputPath))
	fmt.Fprintf(stdout, "katlos image ref: %s\n", relPath(cfg.RepoRoot, link))
	return nil
}

func writeIndex(indexPath string, cfg config) error {
	for _, input := range []struct {
		label string
		path  string
	}{
		{"installer UKI", cfg.InstallerUKI},
		{"installer kernel", cfg.InstallerKernel},
		{"installer initrd", cfg.InstallerInitrd},
		{"runtime UKI", cfg.RuntimeUKI},
		{"runtime UKI metadata", cfg.RuntimeUKIMetadata},
		{"runtime UKI checksum", cfg.RuntimeUKIChecksum},
		{"runtime SquashFS", cfg.RuntimeRoot},
		{"runtime metadata", cfg.RuntimeMetadata},
		{"runtime checksum", cfg.RuntimeChecksum},
	} {
		if err := requireFile(input.label, input.path, cfg.RepoRoot); err != nil {
			return err
		}
	}

	includeKatlOS := cfg.KatlOSExplicit || fileExists(cfg.KatlOSImage)
	if includeKatlOS {
		for _, input := range []struct {
			label string
			path  string
		}{
			{"KatlOS install image", cfg.KatlOSImage},
			{"KatlOS install image metadata", cfg.KatlOSMetadata},
			{"KatlOS install image checksum", cfg.KatlOSChecksum},
		} {
			if err := requireFile(input.label, input.path, cfg.RepoRoot); err != nil {
				return err
			}
		}
	}
	includeInstallerISO := cfg.InstallerISOExplicit || fileExists(cfg.InstallerISO)
	if err := writeInstallerArtifacts(cfg); err != nil {
		return err
	}

	created := time.Now().UTC().Format(time.RFC3339)
	entries := []artifactEntry{}
	indexedArtifacts := []struct {
		kind     string
		format   string
		path     string
		metadata string
		checksum string
	}{
		{"installer-uki", "uki", cfg.InstallerUKI, metadataPath(cfg.InstallerUKI), checksumPath(cfg.InstallerUKI)},
		{"installer-kernel", "linux-kernel", cfg.InstallerKernel, metadataPath(cfg.InstallerKernel), checksumPath(cfg.InstallerKernel)},
		{"installer-initrd", "initrd", cfg.InstallerInitrd, metadataPath(cfg.InstallerInitrd), checksumPath(cfg.InstallerInitrd)},
		{"runtime-uki", "uki", cfg.RuntimeUKI, cfg.RuntimeUKIMetadata, cfg.RuntimeUKIChecksum},
		{"runtime-root", "squashfs", cfg.RuntimeRoot, cfg.RuntimeMetadata, cfg.RuntimeChecksum},
	}
	if includeInstallerISO {
		indexedArtifacts = append(indexedArtifacts, struct {
			kind     string
			format   string
			path     string
			metadata string
			checksum string
		}{"installer-iso", "iso", cfg.InstallerISO, metadataPath(cfg.InstallerISO), checksumPath(cfg.InstallerISO)})
	}
	for _, artifact := range indexedArtifacts {
		entry, err := newEntry(artifact.kind, artifact.format, artifact.path, artifact.metadata, artifact.checksum, cfg.RepoRoot)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
	}
	if includeKatlOS {
		entry, err := newEntry("katlos-install-image", "squashfs", cfg.KatlOSImage, cfg.KatlOSMetadata, cfg.KatlOSChecksum, cfg.RepoRoot)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
	}

	index := artifactIndex{
		SchemaVersion: 1,
		GeneratedAt:   created,
		Generation:    cfg.Generation,
		Artifacts:     entries,
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal artifact index: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return fmt.Errorf("create artifact index directory: %w", err)
	}
	if err := os.WriteFile(indexPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write artifact index %s: %w", relPath(cfg.RepoRoot, indexPath), err)
	}
	return nil
}

func writeInstallerArtifacts(cfg config) error {
	artifacts := []struct {
		label  string
		role   string
		format string
		path   string
	}{
		{"installer UKI", "installer-uki", "uki", cfg.InstallerUKI},
		{"installer kernel", "installer-kernel", "linux-kernel", cfg.InstallerKernel},
		{"installer initrd", "installer-initrd", "initrd", cfg.InstallerInitrd},
	}
	if cfg.InstallerISOExplicit || fileExists(cfg.InstallerISO) {
		artifacts = append(artifacts, struct {
			label  string
			role   string
			format string
			path   string
		}{"installer ISO", "installer-iso", "iso", cfg.InstallerISO})
	}
	for _, artifact := range artifacts {
		if err := requireFile(artifact.label, artifact.path, cfg.RepoRoot); err != nil {
			return err
		}
	}
	created := time.Now().UTC().Format(time.RFC3339)
	for _, artifact := range artifacts {
		if err := writeChecksum(artifact.path); err != nil {
			return err
		}
		if err := writeBootMetadata(artifact.role, artifact.format, artifact.path, created, cfg); err != nil {
			return err
		}
	}
	return nil
}

func writeRuntimeIndex(indexPath string, cfg config) error {
	for _, input := range []struct {
		label string
		path  string
	}{
		{"runtime UKI", cfg.RuntimeUKI},
		{"runtime UKI metadata", cfg.RuntimeUKIMetadata},
		{"runtime UKI checksum", cfg.RuntimeUKIChecksum},
		{"runtime SquashFS", cfg.RuntimeRoot},
		{"runtime metadata", cfg.RuntimeMetadata},
		{"runtime checksum", cfg.RuntimeChecksum},
	} {
		if err := requireFile(input.label, input.path, cfg.RepoRoot); err != nil {
			return err
		}
	}
	runtimeEntries := make([]artifactEntry, 0, 2)
	for _, artifact := range []struct {
		kind     string
		format   string
		path     string
		metadata string
		checksum string
	}{
		{"runtime-uki", "uki", cfg.RuntimeUKI, cfg.RuntimeUKIMetadata, cfg.RuntimeUKIChecksum},
		{"runtime-root", "squashfs", cfg.RuntimeRoot, cfg.RuntimeMetadata, cfg.RuntimeChecksum},
	} {
		entry, err := newEntry(artifact.kind, artifact.format, artifact.path, artifact.metadata, artifact.checksum, cfg.RepoRoot)
		if err != nil {
			return err
		}
		runtimeEntries = append(runtimeEntries, entry)
	}
	index := artifactIndex{SchemaVersion: 1}
	if data, err := os.ReadFile(indexPath); err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("decode artifact index %s: %w", relPath(cfg.RepoRoot, indexPath), err)
		}
		if index.SchemaVersion != 1 {
			return fmt.Errorf("artifact index %s has unsupported schemaVersion %d", relPath(cfg.RepoRoot, indexPath), index.SchemaVersion)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read artifact index %s: %w", relPath(cfg.RepoRoot, indexPath), err)
	}
	entries := make([]artifactEntry, 0, len(index.Artifacts)+2)
	for _, entry := range index.Artifacts {
		if entry.Kind != "runtime-uki" && entry.Kind != "runtime-root" {
			entries = append(entries, entry)
		}
	}
	index.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	index.Generation = cfg.Generation
	index.Artifacts = append(entries, runtimeEntries...)
	return writeJSON(indexPath, index, cfg.RepoRoot)
}

func writeBootMetadata(role, format, artifactPath, created string, cfg config) error {
	size, digest, err := fileInfo(artifactPath)
	if err != nil {
		return err
	}
	rel, err := artifactRel(cfg.RepoRoot, artifactPath)
	if err != nil {
		return err
	}
	metadata := bootMetadata{
		APIVersion:               "katl.dev/v1alpha1",
		Kind:                     "InstallerBootArtifact",
		ArtifactRole:             role,
		Format:                   format,
		Version:                  cfg.Version,
		BuildID:                  cfg.Generation,
		Architecture:             cfg.Architecture,
		Path:                     rel,
		SizeBytes:                size,
		SHA256:                   digest,
		Compression:              installerBootCompression(role),
		CreatedAt:                created,
		InstallerInterface:       cfg.InstallerInterface,
		DefaultKernelCommandLine: []string{"console=ttyS0,115200n8", "console=tty0", "systemd.log_target=console", "loglevel=6"},
		SupportedInputModes:      []string{"pxe-preseed", "local-handoff", "offline-media"},
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal installer boot metadata: %w", err)
	}
	path := metadataPath(artifactPath)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write installer boot metadata %s: %w", relPath(cfg.RepoRoot, path), err)
	}
	return nil
}

func installerBootCompression(role string) string {
	if role == "installer-initrd" {
		return "zstd"
	}
	return ""
}

func writeJSON(path string, value any, repoRoot string) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s directory: %w", relPath(repoRoot, path), err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", relPath(repoRoot, path), err)
	}
	return nil
}

func resolveRuntimeSHA(repoRoot, explicit, metadataPath string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	path := absPath(repoRoot, metadataPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read runtime metadata %s: %w", relPath(repoRoot, path), err)
	}
	var metadata struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode runtime metadata %s: %w", relPath(repoRoot, path), err)
	}
	if strings.TrimSpace(metadata.SHA256) == "" {
		return "", fmt.Errorf("runtime metadata missing sha256: %s", relPath(repoRoot, path))
	}
	return metadata.SHA256, nil
}

func readAndValidateLocalMetadata(label, metadataPath, artifactPath string) (localMetadata, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return localMetadata{}, fmt.Errorf("read %s metadata %s: %w", label, metadataPath, err)
	}
	var metadata localMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return localMetadata{}, fmt.Errorf("decode %s metadata %s: %w", label, metadataPath, err)
	}
	size, digest, err := fileInfo(artifactPath)
	if err != nil {
		return localMetadata{}, err
	}
	if metadata.SizeBytes != size {
		return localMetadata{}, fmt.Errorf("%s metadata sizeBytes %d does not match artifact %d", label, metadata.SizeBytes, size)
	}
	if metadata.SHA256 != digest {
		return localMetadata{}, fmt.Errorf("%s metadata sha256 %s does not match artifact %s", label, metadata.SHA256, digest)
	}
	return metadata, nil
}

func katlosImageFromIndex(indexPath, repoRoot string) (string, katlosArtifactMetadata, error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return "", katlosArtifactMetadata{}, fmt.Errorf("read artifact index %s: %w", relPath(repoRoot, indexPath), err)
	}
	var index artifactIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return "", katlosArtifactMetadata{}, fmt.Errorf("decode artifact index %s: %w", relPath(repoRoot, indexPath), err)
	}
	var matches []artifactEntry
	for _, entry := range index.Artifacts {
		if entry.Kind == "katlos-install-image" {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return "", katlosArtifactMetadata{}, fmt.Errorf("expected exactly one katlos-install-image artifact in %s, found %d", relPath(repoRoot, indexPath), len(matches))
	}
	entry := matches[0]
	if strings.TrimSpace(entry.MetadataPath) == "" {
		return "", katlosArtifactMetadata{}, fmt.Errorf("katlos-install-image artifact is missing metadataPath")
	}
	imagePath := absPath(repoRoot, entry.Path)
	size, digest, err := fileInfo(imagePath)
	if err != nil {
		return "", katlosArtifactMetadata{}, err
	}
	if entry.SizeBytes != size {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image size does not match artifact index")
	}
	if entry.SHA256 != digest {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image sha256 does not match artifact index")
	}
	metadataPath := absPath(repoRoot, entry.MetadataPath)
	data, err = os.ReadFile(metadataPath)
	if err != nil {
		return "", katlosArtifactMetadata{}, fmt.Errorf("read KatlOS image metadata %s: %w", relPath(repoRoot, metadataPath), err)
	}
	var metadata katlosArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", katlosArtifactMetadata{}, fmt.Errorf("decode KatlOS image metadata %s: %w", relPath(repoRoot, metadataPath), err)
	}
	if metadata.Kind != "KatlOSImageArtifact" {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image metadata kind must be KatlOSImageArtifact")
	}
	if metadata.ImageRole != "install" {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image role must be install")
	}
	if metadata.SizeBytes < 0 {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image sizeBytes must not be negative")
	}
	if metadata.SizeBytes != size {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image size does not match metadata")
	}
	if metadata.SHA256 != digest {
		return "", katlosArtifactMetadata{}, fmt.Errorf("KatlOS image sha256 does not match metadata")
	}
	return imagePath, metadata, nil
}

func decodeInstallManifestTemplate(data []byte) (manifest.Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var install manifest.Manifest
	if err := decoder.Decode(&install); err != nil {
		return manifest.Manifest{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return manifest.Manifest{}, fmt.Errorf("multiple YAML documents")
		}
		return manifest.Manifest{}, err
	}
	if install.APIVersion != manifest.APIVersion {
		return manifest.Manifest{}, fmt.Errorf("apiVersion must be %s", manifest.APIVersion)
	}
	if install.Kind != manifest.Kind {
		return manifest.Manifest{}, fmt.Errorf("kind must be %s", manifest.Kind)
	}
	return install, nil
}

func validateLocalRef(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("localRef is required")
	}
	if filepath.IsAbs(value) {
		return fmt.Errorf("localRef must be relative")
	}
	if filepath.ToSlash(filepath.Clean(value)) != value || strings.Contains(value, "//") {
		return fmt.Errorf("localRef must be clean")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("localRef must not contain dot segments")
		}
	}
	return nil
}

func validateKatlOSComponents(root, uki localMetadata, architecture, runtimeInterface string) error {
	if root.Architecture != architecture {
		return fmt.Errorf("runtime root architecture %s does not match image architecture %s", root.Architecture, architecture)
	}
	if uki.Architecture != architecture {
		return fmt.Errorf("runtime UKI architecture %s does not match image architecture %s", uki.Architecture, architecture)
	}
	if root.RuntimeInterface != runtimeInterface {
		return fmt.Errorf("runtime root interface %s does not match image interface %s", root.RuntimeInterface, runtimeInterface)
	}
	if uki.RuntimeInterface != runtimeInterface {
		return fmt.Errorf("runtime UKI interface %s does not match image interface %s", uki.RuntimeInterface, runtimeInterface)
	}
	if uki.CompatibleRuntime == nil {
		return fmt.Errorf("runtime UKI metadata missing compatibleRuntime")
	}
	if uki.CompatibleRuntime.ArtifactSHA256 != root.SHA256 {
		return fmt.Errorf("runtime UKI was built for root %s, want %s", uki.CompatibleRuntime.ArtifactSHA256, root.SHA256)
	}
	return nil
}

func writeComponentChecksum(path, digest, componentPath string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create component checksum directory: %w", err)
	}
	content := fmt.Sprintf("%s  %s\n", digest, componentPath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write component checksum %s: %w", path, err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func newEntry(kind, format, path, metadata, checksum, repoRoot string) (artifactEntry, error) {
	size, digest, err := fileInfo(path)
	if err != nil {
		return artifactEntry{}, err
	}
	rel, err := artifactRel(repoRoot, path)
	if err != nil {
		return artifactEntry{}, err
	}
	entry := artifactEntry{
		Kind:      kind,
		Path:      rel,
		Format:    format,
		SizeBytes: size,
		SHA256:    digest,
	}
	if metadata != "" {
		entry.MetadataPath = relPath(repoRoot, metadata)
	}
	if checksum != "" {
		entry.ChecksumPath = relPath(repoRoot, checksum)
	}
	return entry, nil
}

func pathForKind(indexPath, repoRoot, kind string) (string, error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("artifact index not found: %s", relPath(repoRoot, indexPath))
		}
		return "", fmt.Errorf("read artifact index %s: %w", relPath(repoRoot, indexPath), err)
	}
	var index artifactIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return "", fmt.Errorf("decode artifact index %s: %w", relPath(repoRoot, indexPath), err)
	}
	matches := []artifactEntry{}
	for _, artifact := range index.Artifacts {
		if artifact.Kind == kind {
			matches = append(matches, artifact)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("artifact kind not found in %s: %s", relPath(repoRoot, indexPath), kind)
	case 1:
		return absPath(repoRoot, matches[0].Path), nil
	default:
		return "", fmt.Errorf("artifact kind appears more than once in %s: %s", relPath(repoRoot, indexPath), kind)
	}
}

func writeChecksum(path string) error {
	_, digest, err := fileInfo(path)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("%s  %s\n", digest, filepath.Base(path))
	if err := os.WriteFile(checksumPath(path), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write checksum %s: %w", checksumPath(path), err)
	}
	return nil
}

func fileInfo(path string) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", fmt.Errorf("stat artifact %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("artifact is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", fmt.Errorf("read artifact %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return info.Size(), hex.EncodeToString(sum[:]), nil
}

func requireFile(label, path, repoRoot string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s not found: %s", label, relPath(repoRoot, path))
		}
		return fmt.Errorf("stat %s %s: %w", label, relPath(repoRoot, path), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file: %s", label, relPath(repoRoot, path))
	}
	return nil
}

func checksumPath(path string) string {
	return path + ".sha256"
}

func metadataPath(path string) string {
	return path + ".json"
}

func repoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err == nil {
		return filepath.Clean(strings.TrimSpace(string(output))), nil
	}
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		return "", fmt.Errorf("find repository root: %w", err)
	}
	return wd, nil
}

func gitDescribe(repo string) string {
	cmd := exec.Command("git", "-C", repo, "describe", "--always", "--dirty", "--abbrev=12")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func hostArchitecture() string {
	cmd := exec.Command("uname", "-m")
	output, err := cmd.Output()
	if err == nil {
		arch := strings.TrimSpace(string(output))
		if arch != "" {
			return arch
		}
	}
	return runtime.GOARCH
}

func envMap(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			env[name] = value
		}
	}
	return env
}

func envDefault(env map[string]string, key, fallback string) string {
	if value, ok := env[key]; ok && value != "" {
		return value
	}
	return fallback
}

func envDefaultFunc(env map[string]string, key string, fallback func() string) string {
	if value, ok := env[key]; ok && value != "" {
		return value
	}
	return fallback()
}

func envPath(env map[string]string, repoRoot, key, fallback string) string {
	path, _ := envPathExplicit(env, repoRoot, key, fallback)
	return path
}

func envPathExplicit(env map[string]string, repoRoot, key, fallback string) (string, bool) {
	value, ok := env[key]
	if !ok || value == "" {
		return fallback, false
	}
	return absPath(repoRoot, value), true
}

func absPath(repoRoot, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(repoRoot, path)
}

func artifactRel(repoRoot, path string) (string, error) {
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return "", fmt.Errorf("relativize artifact path %s: %w", path, err)
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("artifact path must be under repository root for local index: %s", path)
	}
	return filepath.ToSlash(rel), nil
}

func relPath(repoRoot, path string) string {
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return path
	}
	return filepath.ToSlash(rel)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
