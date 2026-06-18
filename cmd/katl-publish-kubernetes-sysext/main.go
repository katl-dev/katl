package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/zariel/katl/internal/installer/sysextcatalog"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katl-publish-kubernetes-sysext: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-publish-kubernetes-sysext", flag.ContinueOnError)
	flags.SetOutput(stderr)

	metadataPath := flags.String("metadata", "_build/mkosi/katl-kubernetes.raw.json", "path to Kubernetes sysext metadata")
	artifactPath := flags.String("artifact", "", "path to Kubernetes sysext raw artifact, default from metadata path")
	outputDir := flags.String("output-dir", "_build/publish/kubernetes-sysext", "directory for staged publish outputs")
	baseURL := flags.String("base-url", "", "optional published artifact base URL for the catalog entry")

	if err := flags.Parse(args); err != nil {
		return err
	}

	staged, err := sysextcatalog.StageKubernetesSysext(sysextcatalog.StageRequest{
		MetadataPath: *metadataPath,
		ArtifactPath: *artifactPath,
		OutputDir:    *outputDir,
		BaseURL:      *baseURL,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "artifact: %s\n", staged.ArtifactPath)
	fmt.Fprintf(stdout, "checksum: %s\n", staged.ChecksumPath)
	fmt.Fprintf(stdout, "metadata: %s\n", staged.MetadataPath)
	fmt.Fprintf(stdout, "catalog: %s\n", staged.CatalogPath)
	fmt.Fprintf(stdout, "bundle: %s\n", staged.BundlePath)
	fmt.Fprintf(stdout, "bundle-manifest-digest: %s\n", staged.BundleManifestDigest)
	fmt.Fprintf(stdout, "bundle-index: %s\n", staged.IndexPath)
	fmt.Fprintf(stdout, "bundle-catalog: %s\n", staged.BundleCatalogPath)
	return nil
}
