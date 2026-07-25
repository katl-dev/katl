package sysextcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
)

const (
	KubernetesBundleArtifactType = "application/vnd.katl.kubernetes.payload.bundle.v1"
	KubernetesBundleConfigType   = "application/vnd.katl.kubernetes.payload.bundle.v1+json"
)

func PackStaged(ctx context.Context, staged StagedArtifact, annotations map[string]string) (payloadbundle.Packed, error) {
	request, err := stagedPublishRequest(staged, annotations)
	if err != nil {
		return payloadbundle.Packed{}, err
	}
	return payloadbundle.Pack(ctx, request)
}

func PublishStaged(ctx context.Context, staged StagedArtifact, reference string, annotations map[string]string) (payloadbundle.Published, error) {
	request, err := stagedPublishRequest(staged, annotations)
	if err != nil {
		return payloadbundle.Published{}, err
	}
	return payloadbundle.Publish(ctx, payloadbundle.PublishRequest{
		Reference: reference, ArtifactType: request.ArtifactType,
		ConfigMediaType: request.ConfigMediaType, Config: request.Config,
		Blobs: request.Blobs, Annotations: request.Annotations,
		UseDockerCredentials: true,
	})
}

func stagedPublishRequest(staged StagedArtifact, annotations map[string]string) (payloadbundle.PackRequest, error) {
	config, err := os.ReadFile(staged.BundlePath)
	if err != nil {
		return payloadbundle.PackRequest{}, fmt.Errorf("read staged Kubernetes bundle: %w", err)
	}
	var bundle KubernetesPayloadBundle
	if err := json.Unmarshal(config, &bundle); err != nil {
		return payloadbundle.PackRequest{}, fmt.Errorf("decode staged Kubernetes bundle: %w", err)
	}
	paths := map[string]string{
		"systemd-sysext":     staged.ArtifactPath,
		"sysext-metadata":    staged.MetadataPath,
		"package-provenance": staged.PackageProvenancePath,
		"catalog-fragment":   staged.CatalogFragmentPath,
	}
	descriptors := append(append([]BundleDescriptor(nil), bundle.Payloads...), bundle.Metadata...)
	blobs := make([]payloadbundle.Blob, 0, len(descriptors))
	for _, descriptor := range descriptors {
		path := paths[descriptor.Role]
		if path == "" {
			return payloadbundle.PackRequest{}, fmt.Errorf("staged Kubernetes bundle has unsupported role %q", descriptor.Role)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return payloadbundle.PackRequest{}, fmt.Errorf("read staged Kubernetes %s: %w", descriptor.Role, err)
		}
		blobs = append(blobs, payloadbundle.Blob{Descriptor: descriptor, Data: data})
	}
	return payloadbundle.PackRequest{
		ArtifactType: KubernetesBundleArtifactType, ConfigMediaType: KubernetesBundleConfigType,
		Config: config, Blobs: blobs, Annotations: annotations,
	}, nil
}
