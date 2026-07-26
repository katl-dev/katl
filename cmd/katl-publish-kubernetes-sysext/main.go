package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
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
	var publishRefs, annotationValues stringValues
	flags.Var(&publishRefs, "publish-ref", "optional immutable OCI destination REGISTRY/REPOSITORY:TAG (repeatable)")
	flags.Var(&annotationValues, "annotation", "OCI manifest annotation KEY=VALUE (repeatable)")

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
	annotations, err := parseAnnotations(annotationValues)
	if err != nil {
		return err
	}
	packed, err := sysextcatalog.PackStaged(context.Background(), staged, annotations)
	if err != nil {
		return err
	}
	manifestTag, err := payloadbundle.ManifestDigestTag(packed.ManifestDigest)
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
	fmt.Fprintf(stdout, "oci-manifest-digest: %s\n", packed.ManifestDigest)
	fmt.Fprintf(stdout, "oci-manifest-tag: %s\n", manifestTag)
	for _, reference := range publishRefs {
		published, err := sysextcatalog.PublishStaged(context.Background(), staged, reference, annotations)
		if err != nil {
			return err
		}
		action := "published"
		if published.Existing {
			action = "already-published"
		}
		fmt.Fprintf(stdout, "%s: %s@%s\n", action, reference, published.ManifestDigest)
	}
	return nil
}

type stringValues []string

func (values *stringValues) String() string {
	return strings.Join(*values, ",")
}

func (values *stringValues) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func parseAnnotations(values []string) (map[string]string, error) {
	annotations := make(map[string]string, len(values))
	for _, value := range values {
		key, annotation, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(annotation) == "" {
			return nil, fmt.Errorf("--annotation must be KEY=VALUE")
		}
		if _, exists := annotations[key]; exists {
			return nil, fmt.Errorf("--annotation key %q is repeated", key)
		}
		annotations[key] = annotation
	}
	return annotations, nil
}
