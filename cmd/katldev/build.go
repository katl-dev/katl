package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	"github.com/katl-dev/katl/internal/kubernetesrelease"
	"github.com/spf13/cobra"
)

type buildCommandRunner func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error

type installerISOArtifact struct {
	Path      string
	Metadata  string
	Checksum  string
	SHA256    string
	SizeBytes int64
}

type kubernetesBuildArtifact struct {
	Path           string
	PayloadVersion string
	Architecture   string
	SHA256         string
	SizeBytes      int64
}

var katlOSBuildVersionPattern = regexp.MustCompile(`^[0-9]{4}\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

func newBuildCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build development artifacts from the current checkout",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newBuildISOCommand(ctx, stdout, stderr))
	cmd.AddCommand(newBuildKubernetesCommand(ctx, stdout, stderr))
	cmd.AddCommand(newBuildUpgradeCommand(ctx, stdout, stderr))
	return cmd
}

func newBuildKubernetesCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "kubernetes",
		Short: "Build and verify a Kubernetes upgrade image from the current checkout",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			repoRoot, err := repositoryRoot()
			if err != nil {
				return err
			}
			built, err := buildKubernetesUpgrade(ctx, repoRoot, version, stderr, runBuildCommand)
			if err != nil {
				return err
			}
			return writeKubernetesBuildArtifact(stdout, built)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Kubernetes payload version, for example v1.36.2")
	return cmd
}

func newBuildUpgradeCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Build and verify a KatlOS upgrade image from the current checkout",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			repoRoot, err := repositoryRoot()
			if err != nil {
				return err
			}
			artifact, err := buildKatlOSUpgrade(ctx, repoRoot, version, stderr, runBuildCommand)
			if err != nil {
				return err
			}
			return writeKatlOSUpgradeArtifact(stdout, artifact)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "KatlOS version to embed in the locally built image")
	return cmd
}

func newBuildISOCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "iso",
		Short: "Build and verify the current checkout's installer ISO",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			repoRoot, err := repositoryRoot()
			if err != nil {
				return err
			}
			artifact, err := buildInstallerISO(ctx, repoRoot, stderr, runBuildCommand)
			if err != nil {
				return err
			}
			return writeInstallerISOArtifact(stdout, artifact)
		},
	}
}

func buildInstallerISO(ctx context.Context, repoRoot string, stderr io.Writer, run buildCommandRunner) (installerISOArtifact, error) {
	if run == nil {
		return installerISOArtifact{}, fmt.Errorf("build command runner is required")
	}
	iso := filepath.Join(repoRoot, "_build", "mkosi", "katl-installer.iso")
	fmt.Fprintln(stderr, "katldev build: building the current checkout installer ISO")
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "mkosi"), []string{"build-installer-iso"}, nil, stderr, stderr); err != nil {
		return installerISOArtifact{}, fmt.Errorf("build installer ISO: %w", err)
	}
	fmt.Fprintln(stderr, "katldev build: verifying the completed installer ISO")
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "check-installer-iso"), []string{iso}, nil, stderr, stderr); err != nil {
		return installerISOArtifact{}, fmt.Errorf("verify installer ISO: %w", err)
	}
	digest, err := sha256File(iso)
	if err != nil {
		return installerISOArtifact{}, fmt.Errorf("identify installer ISO: %w", err)
	}
	info, err := os.Stat(iso)
	if err != nil {
		return installerISOArtifact{}, fmt.Errorf("inspect installer ISO: %w", err)
	}
	artifact := installerISOArtifact{
		Path:      iso,
		Metadata:  iso + ".json",
		Checksum:  iso + ".sha256",
		SHA256:    digest,
		SizeBytes: info.Size(),
	}
	for _, companion := range []struct {
		label string
		path  string
	}{{"metadata", artifact.Metadata}, {"checksum", artifact.Checksum}} {
		if _, err := os.Stat(companion.path); err != nil {
			return installerISOArtifact{}, fmt.Errorf("inspect installer ISO %s: %w", companion.label, err)
		}
	}
	return artifact, nil
}

func runBuildCommand(ctx context.Context, dir, name string, args, environment []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), environment...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func buildKatlOSUpgrade(ctx context.Context, repoRoot, version string, stderr io.Writer, run buildCommandRunner) (hostUpgradeBuildArtifact, error) {
	if run == nil {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("build command runner is required")
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("--version is required")
	}
	if !katlOSBuildVersionPattern.MatchString(version) {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("--version %q must look like 2026.7.0-dev.1", version)
	}
	architecture, err := developmentArtifactArchitecture(runtime.GOARCH)
	if err != nil {
		return hostUpgradeBuildArtifact{}, err
	}
	buildID := version
	if revision, dirty := checkoutRevision(repoRoot); revision != "" {
		buildID = revision
		if dirty {
			buildID += "-dirty"
		}
	}
	image := filepath.Join(repoRoot, "_build", "mkosi", "katlos-upgrade-"+version+"-"+architecture+".squashfs")
	fmt.Fprintf(stderr, "katldev build: building KatlOS %s upgrade image from the current checkout\n", version)
	environment := []string{"KATL_VERSION=" + version, "KATL_UPGRADE_VERSION=" + version, "KATL_ARCHITECTURE=" + architecture, "KATL_BUILD_COMMIT=" + buildID}
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "mkosi"), []string{"build-katlos-upgrade-image"}, environment, stderr, stderr); err != nil {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("build KatlOS upgrade image: %w", err)
	}
	fmt.Fprintln(stderr, "katldev build: verifying the completed KatlOS upgrade image")
	metadata, err := katlosimage.ReadArtifactMetadata(image+".json", katlosimage.RoleUpgrade)
	if err != nil {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("verify KatlOS upgrade metadata: %w", err)
	}
	if metadata.Version != version {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("KatlOS upgrade metadata version %q does not match requested %q", metadata.Version, version)
	}
	if metadata.Architecture != architecture {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("KatlOS upgrade metadata architecture %q does not match requested %q", metadata.Architecture, architecture)
	}
	if metadata.BuildID != buildID {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("KatlOS upgrade metadata buildID %q does not match current checkout %q", metadata.BuildID, buildID)
	}
	if err := metadata.VerifyFile(image); err != nil {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("verify KatlOS upgrade image: %w", err)
	}
	checksum := image + ".sha256"
	if info, err := os.Stat(checksum); err != nil {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("inspect KatlOS upgrade checksum: %w", err)
	} else if !info.Mode().IsRegular() {
		return hostUpgradeBuildArtifact{}, fmt.Errorf("KatlOS upgrade checksum is not a regular file")
	}
	return hostUpgradeBuildArtifact{
		Path:         image,
		Metadata:     image + ".json",
		Checksum:     checksum,
		Version:      metadata.Version,
		Architecture: metadata.Architecture,
		SHA256:       metadata.SHA256,
		SizeBytes:    metadata.SizeBytes,
	}, nil
}

func buildKubernetesUpgrade(ctx context.Context, repoRoot, version string, stderr io.Writer, run buildCommandRunner) (kubernetesBuildArtifact, error) {
	if run == nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("build command runner is required")
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return kubernetesBuildArtifact{}, fmt.Errorf("--version is required")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	supported, err := kubernetesrelease.DefaultSupportedVersions()
	if err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("load supported Kubernetes versions: %w", err)
	}
	selected, err := supported.Select(version)
	if err != nil {
		return kubernetesBuildArtifact{}, err
	}
	release := selected[0]
	architecture, err := developmentArtifactArchitecture(runtime.GOARCH)
	if err != nil {
		return kubernetesBuildArtifact{}, err
	}
	buildID := release.ArtifactVersion()
	if revision, dirty := checkoutRevision(repoRoot); revision != "" {
		buildID = revision
		if dirty {
			buildID += "-dirty"
		}
	}
	minor := sysextcatalog.KubernetesMinor(version)
	outputName := "katl-kubernetes-" + version
	image := filepath.Join(repoRoot, "_build", "mkosi", outputName+".raw")
	runtimeEnvironment := []string{
		"KATL_ARCHITECTURE=" + architecture,
		"KATL_BUILD_COMMIT=" + buildID,
	}
	environment := append(append([]string(nil), runtimeEnvironment...),
		"KATL_KUBERNETES_MINOR="+minor,
		"KATL_KUBERNETES_PAYLOAD_VERSION="+version,
		"KATL_KUBERNETES_ARTIFACT_REVISION="+strconv.Itoa(release.ArtifactRevision),
		"KATL_KUBERNETES_KUBEADM_VERSION="+release.Packages.Kubeadm,
		"KATL_KUBERNETES_KUBELET_VERSION="+release.Packages.Kubelet,
		"KATL_KUBERNETES_KUBECTL_VERSION="+release.Packages.Kubectl,
		"KATL_KUBERNETES_CRITOOLS_VERSION="+release.Packages.CRITools,
	)
	fmt.Fprintf(stderr, "katldev build: building Kubernetes %s upgrade image from the current checkout\n", version)
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "mkosi"), []string{"build-runtime"}, runtimeEnvironment, stderr, stderr); err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("build KatlOS runtime prerequisite: %w", err)
	}
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "build-kubernetes-sysext"), []string{"--output", outputName}, environment, stderr, stderr); err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("build Kubernetes upgrade image: %w", err)
	}
	fmt.Fprintln(stderr, "katldev build: verifying the completed Kubernetes upgrade image")
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "check-kubernetes-sysext"), []string{image}, environment, stderr, stderr); err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("verify Kubernetes upgrade image: %w", err)
	}
	meta, err := artifact.ReadLocal(image + ".json")
	if err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("read Kubernetes upgrade metadata: %w", err)
	}
	if err := meta.VerifyFile(image); err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("verify Kubernetes upgrade image: %w", err)
	}
	entry, err := sysextcatalog.EntryFromLocalMeta(meta)
	if err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("verify Kubernetes upgrade metadata: %w", err)
	}
	if err := sysextcatalog.ValidateForRuntime(entry, sysextcatalog.Runtime{Interface: "katl-runtime-1", Architecture: architecture}); err != nil {
		return kubernetesBuildArtifact{}, fmt.Errorf("verify Kubernetes upgrade compatibility: %w", err)
	}
	if meta.Name != sysextcatalog.KubernetesName || meta.PayloadVersion != version {
		return kubernetesBuildArtifact{}, fmt.Errorf("Kubernetes upgrade metadata identifies %s %s, want Kubernetes %s", meta.Name, meta.PayloadVersion, version)
	}
	wantPackages := map[string]string{
		"kubeadm": release.Packages.Kubeadm, "kubelet": release.Packages.Kubelet,
		"kubectl": release.Packages.Kubectl, "cri-tools": release.Packages.CRITools,
	}
	for name, want := range wantPackages {
		if got := meta.PackageVersions[name]; got != want {
			return kubernetesBuildArtifact{}, fmt.Errorf("Kubernetes upgrade metadata package %s is %q, want %q", name, got, want)
		}
	}
	return kubernetesBuildArtifact{
		Path: image, PayloadVersion: version, Architecture: architecture,
		SHA256: meta.SHA256, SizeBytes: meta.SizeBytes,
	}, nil
}

type hostUpgradeBuildArtifact struct {
	Path         string
	Metadata     string
	Checksum     string
	Version      string
	Architecture string
	SHA256       string
	SizeBytes    int64
}

func developmentArtifactArchitecture(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("host architecture %q cannot build a supported KatlOS image", goarch)
	}
}

func writeKatlOSUpgradeArtifact(stdout io.Writer, artifact hostUpgradeBuildArtifact) error {
	_, err := fmt.Fprintf(stdout, "KatlOS upgrade image ready.\nImage: %s\nUse with:\n  katlctl node upgrade NODE --config cluster.yaml --artifact %s\n", artifact.Path, artifact.Path)
	return err
}

func writeKubernetesBuildArtifact(stdout io.Writer, artifact kubernetesBuildArtifact) error {
	_, err := fmt.Fprintf(stdout, "Kubernetes upgrade image ready.\nImage: %s\nUse with:\n  katlctl kubernetes upgrade %s --config cluster.yaml --artifact %s\n", artifact.Path, artifact.PayloadVersion, artifact.Path)
	return err
}

func writeInstallerISOArtifact(stdout io.Writer, artifact installerISOArtifact) error {
	_, err := fmt.Fprintf(stdout, "Installer ISO ready.\nISO: %s\nMetadata: %s\nChecksum: %s\nSHA256: %s\nSize: %d bytes\n", artifact.Path, artifact.Metadata, artifact.Checksum, artifact.SHA256, artifact.SizeBytes)
	return err
}
