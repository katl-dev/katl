package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

type buildCommandRunner func(context.Context, string, string, []string, io.Writer, io.Writer) error

type installerISOArtifact struct {
	Path      string
	Metadata  string
	Checksum  string
	SHA256    string
	SizeBytes int64
}

func newBuildCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build development artifacts from the current checkout",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newBuildISOCommand(ctx, stdout, stderr))
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
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "mkosi"), []string{"build-installer-iso"}, stderr, stderr); err != nil {
		return installerISOArtifact{}, fmt.Errorf("build installer ISO: %w", err)
	}
	fmt.Fprintln(stderr, "katldev build: verifying the completed installer ISO")
	if err := run(ctx, repoRoot, filepath.Join(repoRoot, "scripts", "check-installer-iso"), []string{iso}, stderr, stderr); err != nil {
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

func runBuildCommand(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func writeInstallerISOArtifact(stdout io.Writer, artifact installerISOArtifact) error {
	_, err := fmt.Fprintf(stdout, "Installer ISO ready.\nISO: %s\nMetadata: %s\nChecksum: %s\nSHA256: %s\nSize: %d bytes\n", artifact.Path, artifact.Metadata, artifact.Checksum, artifact.SHA256, artifact.SizeBytes)
	return err
}
