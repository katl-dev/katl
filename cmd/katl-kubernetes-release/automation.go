package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/kubernetescompat"
	"github.com/katl-dev/katl/internal/kubernetesrelease"
	digest "github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

func runAutomation(args []string, stdout, stderr io.Writer, query packageQuery) error {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("payload-version", "", "exact stable Kubernetes version")
	root := flags.String("repo-root", ".", "recipe source directory")
	base := flags.String("base", "", "presubmit comparison revision")
	repoquery := flags.String("repoquery", "dnf", "repository query executable")
	repositoryName := flags.String("repository", kubernetescompat.Repository, "OCI repository")
	artifact := flags.String("artifact-version", "", "immutable candidate tag")
	manifestDigest := flags.String("manifest-digest", "", "verified candidate manifest digest")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch args[0] {
	case "presubmit":
		changed, err := kubernetesrelease.RecipeChanged(*root, *base)
		if err != nil {
			return err
		}
		matrix := releaseMatrix{Include: []releaseMatrixEntry{}}
		if changed {
			supported, err := kubernetesrelease.DefaultSupportedVersions()
			if err != nil {
				return err
			}
			matrix.Include = append(matrix.Include, releaseMatrixEntry{PayloadVersion: supported.Versions[len(supported.Versions)-1].PayloadVersion})
		}
		return json.NewEncoder(stdout).Encode(matrix)
	case "resolve":
		entry, err := kubernetescompat.ResolveAvailable(ctx, kubernetescompat.Request{KubernetesVersion: *version})
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(entry)
	case "discover":
		versions, err := kubernetesrelease.Discover(ctx, &http.Client{Timeout: 30 * time.Second}, "https://api.github.com/repos/kubernetes/kubernetes/releases", os.Getenv("GH_TOKEN"))
		if err != nil {
			return err
		}
		var entries []releaseMatrixEntry
		for _, version := range versions {
			entries = append(entries, releaseMatrixEntry{PayloadVersion: version})
		}
		return json.NewEncoder(stdout).Encode(releaseMatrix{Include: entries})
	case "candidate":
		if _, err := kubernetescompat.PromotionTag(kubernetescompat.Request{KubernetesVersion: *version}); err != nil {
			return err
		}
		packages, err := resolvePackageVersions(*version, *repoquery, query)
		if err != nil {
			return fmt.Errorf("Kubernetes packages are not ready; the next scheduled run will retry: %w", err)
		}
		entry := candidate(*version, packages)
		return json.NewEncoder(stdout).Encode(entry)
	case "inspect", "promote":
		request := kubernetescompat.Request{KubernetesVersion: *version, Architecture: "x86_64", RuntimeInterface: "katl-runtime-1"}
		promotion, err := kubernetescompat.PromotionTag(request)
		if err != nil {
			return err
		}
		if (*artifact != "" || args[0] == "promote") && (!artifactPattern.MatchString(*artifact) || !strings.HasPrefix(*artifact, *version+"-")) {
			return fmt.Errorf("artifact version must match payload version")
		}
		repository, err := remote.NewRepository(*repositoryName)
		if err != nil {
			return err
		}
		client := &auth.Client{Client: &http.Client{Timeout: time.Minute}, Cache: auth.NewCache()}
		if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
			client.Credential = store.Get
		}
		repository.Client = client
		identifier := *artifact
		if identifier == "" {
			identifier = *version
		}
		if args[0] == "promote" {
			if digest.Digest(*manifestDigest).Validate() != nil || !strings.HasPrefix(*manifestDigest, "sha256:") {
				return fmt.Errorf("promotion requires verified --manifest-digest")
			}
			identifier = *manifestDigest
		}
		entry, err := kubernetescompat.ResolveTarget(ctx, repository, *repositoryName, identifier, request)
		if args[0] == "inspect" && *artifact == "" && errors.Is(err, errdef.ErrNotFound) {
			entry, err = kubernetescompat.ResolveTarget(ctx, repository, *repositoryName, promotion, request)
		}
		if err != nil {
			if args[0] == "inspect" && errors.Is(err, errdef.ErrNotFound) {
				return json.NewEncoder(stdout).Encode(map[string]any{"exists": false})
			}
			return err
		}
		if *artifact != "" && entry.ArtifactVersion != *artifact {
			return fmt.Errorf("candidate artifact identity mismatch")
		}
		_, digest, _ := strings.Cut(entry.Bundle, "@")
		artifactVersion := entry.ArtifactVersion
		promoted := false
		if args[0] == "inspect" {
			descriptor, err := repository.Resolve(ctx, promotion)
			if err != nil && !errors.Is(err, errdef.ErrNotFound) {
				return err
			}
			promoted = err == nil && descriptor.Digest.String() == digest
		}
		if args[0] == "promote" {
			// Check every alias before writing any: retries must not replace a
			// previously verified upstream release. The workflow serializes writers.
			for _, tag := range []string{*version, *version + "-1", promotion} {
				existing, err := repository.Resolve(ctx, tag)
				if err != nil && !errors.Is(err, errdef.ErrNotFound) {
					return err
				}
				if err == nil && existing.Digest.String() != digest {
					return fmt.Errorf("Kubernetes %s is already published with a different digest", *version)
				}
			}
			descriptor, err := repository.Resolve(ctx, digest)
			if err != nil {
				return err
			}
			for _, tag := range []string{promotion, *version + "-1", *version} {
				if err := repository.Tag(ctx, descriptor, tag); err != nil {
					return err
				}
				resolved, err := repository.Resolve(ctx, tag)
				if err != nil {
					return err
				}
				if resolved.Digest.String() != digest {
					return fmt.Errorf("publication did not retain verified digest")
				}
			}
			promoted = true
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"exists": true, "promoted": promoted, "artifactVersion": artifactVersion, "bundle": entry.Bundle, "digest": digest, "promotionTag": promotion})
	}
	return fmt.Errorf("unknown automation command")
}

func candidate(version string, packages kubernetesrelease.PackageVersions) releaseMatrixEntry {
	// Publication is immutable for each upstream version, not a rebuild counter.
	const revision = 1
	minor := version[:strings.LastIndex(version, ".")]
	return releaseMatrixEntry{
		PayloadVersion: version, ArtifactRevision: revision,
		ArtifactVersion: fmt.Sprintf("%s-%d", version, revision), Minor: minor,
		KubeadmVersion: packages.Kubeadm, KubeletVersion: packages.Kubelet,
		KubectlVersion: packages.Kubectl, CRIToolsVersion: packages.CRITools,
	}
}
