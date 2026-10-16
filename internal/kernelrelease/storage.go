package kernelrelease

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func Export(ctx context.Context, set Set, rpmDirectory, layout, repository string) (Lock, error) {
	if err := set.Validate(); err != nil {
		return Lock{}, err
	}
	if err := verifyDirectory(rpmDirectory, set); err != nil {
		return Lock{}, err
	}
	blobs := make([]payloadbundle.Blob, 0, len(set.Packages))
	for _, pkg := range set.Packages {
		data, err := os.ReadFile(filepath.Join(rpmDirectory, pkg.FileName))
		if err != nil {
			return Lock{}, err
		}
		blobs = append(blobs, payloadbundle.Blob{Descriptor: pkg.Descriptor, Data: data})
	}
	config, err := json.Marshal(set)
	if err != nil {
		return Lock{}, err
	}
	packed, err := payloadbundle.Export(ctx, layout, payloadbundle.PackRequest{
		ArtifactType: ArtifactType, ConfigMediaType: ConfigMediaType, Config: config, Blobs: blobs,
		Annotations: map[string]string{
			ocispec.AnnotationCreated: "1970-01-01T00:00:00Z",
			ocispec.AnnotationSource:  "https://github.com/katl-dev/katl",
		},
	})
	if err != nil {
		return Lock{}, err
	}
	lock := Lock{Set: set, Reference: repository + "@" + packed.ManifestDigest}
	return lock, lock.Validate()
}

func Publish(ctx context.Context, lock Lock, layout string) error {
	if _, err := fetch(ctx, lock, layout); err != nil {
		return err
	}
	_, err := payloadbundle.PublishLayout(ctx, payloadbundle.FetchRequest{LayoutDir: layout, Reference: lock.Reference, ArtifactType: ArtifactType, ConfigMediaType: ConfigMediaType, UseDockerCredentials: true})
	return err
}

func fetch(ctx context.Context, lock Lock, layout string) (payloadbundle.Fetched, error) {
	if err := lock.Validate(); err != nil {
		return payloadbundle.Fetched{}, err
	}
	fetched, err := payloadbundle.Fetch(ctx, payloadbundle.FetchRequest{LayoutDir: layout, Reference: lock.Reference, ArtifactType: ArtifactType, ConfigMediaType: ConfigMediaType})
	if err != nil {
		return fetched, err
	}
	var actual Set
	if err := json.Unmarshal(fetched.Config, &actual); err != nil {
		return fetched, err
	}
	if !reflect.DeepEqual(actual, lock.Set) {
		return fetched, fmt.Errorf("retained kernel inputs do not match the reviewed lock")
	}
	return fetched, payloadbundle.VerifyDescriptors(fetched.Manifest, lock.descriptors())
}

// Prepare verifies cached RPM bytes before allowing an image cache to be used.
// Publish a complete private staging directory atomically so concurrent builds
// cannot observe a partially downloaded kernel set.
func Prepare(ctx context.Context, lock Lock, cache, layout string) (string, error) {
	identity, err := lock.Digest()
	if err != nil {
		return "", err
	}
	if err := lock.Validate(); err != nil {
		return "", err
	}
	destination := filepath.Join(cache, identity)
	if _, err := os.Lstat(destination); err == nil {
		if err := verifyDirectory(destination, lock.Set); err != nil {
			return "", fmt.Errorf("cached kernel RPMs are invalid; remove %s and retry: %w", destination, err)
		}
		return destination, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	fetched, err := fetch(ctx, lock, layout)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(cache, ".kernel-rpms-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	for _, pkg := range lock.Packages {
		data, err := payloadbundle.Content(fetched, pkg.Descriptor)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(staging, pkg.FileName), data, 0o444); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staging, destination); err != nil {
		if verifyErr := verifyDirectory(destination, lock.Set); verifyErr != nil {
			return "", err
		}
	}
	return destination, nil
}

func verifyDirectory(directory string, set Set) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("kernel RPM cache must be a directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(set.Packages) {
		return fmt.Errorf("kernel RPM directory contains unexpected or missing inputs")
	}
	for _, pkg := range set.Packages {
		path := filepath.Join(directory, pkg.FileName)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != pkg.SizeBytes {
			return fmt.Errorf("invalid kernel RPM %s", pkg.FileName)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != pkg.Digest {
			return fmt.Errorf("kernel RPM %s failed SHA-256 verification", pkg.FileName)
		}
	}
	return nil
}
