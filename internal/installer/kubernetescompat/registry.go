package kubernetescompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const Repository = "ghcr.io/katl-dev/kubernetes"

var (
	stableVersion = regexp.MustCompile(`^v1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	tagComponent  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

// PromotionTag separates selectable releases from candidates awaiting validation.
func PromotionTag(request Request) (string, error) {
	if !stableVersion.MatchString(request.KubernetesVersion) {
		return "", fmt.Errorf("Kubernetes %q is not available: require a stable v1.MINOR.PATCH version", request.KubernetesVersion)
	}
	architecture, runtime := request.Architecture, request.RuntimeInterface
	if architecture == "" {
		architecture = "x86_64"
	}
	if runtime == "" {
		runtime = "katl-runtime-1"
	}
	if !tagComponent.MatchString(architecture) || !tagComponent.MatchString(runtime) {
		return "", fmt.Errorf("invalid Kubernetes architecture or runtime interface")
	}
	return "compatible-" + request.KubernetesVersion + "-" + architecture + "-" + runtime, nil
}

// ResolveAvailable retains the release's pinned selections for offline use and
// discovers additions without requiring a new operator binary.
func ResolveAvailable(ctx context.Context, request Request) (Entry, error) {
	if entry, err := Resolve(request); err == nil {
		return entry, nil
	}
	tag, err := PromotionTag(request)
	if err != nil {
		return Entry{}, err
	}
	repository, err := remote.NewRepository(Repository)
	if err != nil {
		return Entry{}, err
	}
	repository.Client = &auth.Client{Client: &http.Client{Timeout: 30 * time.Second}, Cache: auth.NewCache()}
	entry, err := ResolveTarget(ctx, repository, Repository, request.KubernetesVersion, request)
	if errors.Is(err, errdef.ErrNotFound) {
		entry, err = ResolveTarget(ctx, repository, Repository, tag, request)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("Kubernetes %s is not available for this KatlOS runtime: %w; check registry access and choose a published compatible version", request.KubernetesVersion, err)
	}
	return entry, nil
}

// ResolveImage resolves a user-selected reference once and validates its
// metadata before the immutable digest is carried into an operation.
func ResolveImage(ctx context.Context, ref reference.Named, request Request) (Entry, error) {
	repository, err := remote.NewRepository(ref.Name())
	if err != nil {
		return Entry{}, err
	}
	repository.Client = &auth.Client{Client: &http.Client{Timeout: 30 * time.Second}, Cache: auth.NewCache()}
	identifier := ""
	if tagged, ok := ref.(reference.Tagged); ok {
		identifier = tagged.Tag()
	}
	if pinned, ok := ref.(reference.Digested); ok {
		identifier = pinned.Digest().String()
	}
	return ResolveTarget(ctx, repository, ref.Name(), identifier, request)
}

// ResolveTarget verifies small OCI metadata without downloading the sysext.
func ResolveTarget(ctx context.Context, target payloadbundle.Target, repository, tag string, request Request) (Entry, error) {
	reference := repository + ":" + tag
	if strings.HasPrefix(tag, "sha256:") {
		reference = repository + "@" + tag
	}
	ref, err := payloadbundle.ParseReference(reference)
	if err != nil {
		return Entry{}, err
	}
	fetched, err := payloadbundle.FetchTargetManifest(ctx, target, tag, ref, sysextcatalog.KubernetesBundleArtifactType, sysextcatalog.KubernetesBundleConfigType)
	if err != nil {
		return Entry{}, err
	}
	var bundle sysextcatalog.KubernetesPayloadBundle
	if err := json.Unmarshal(fetched.Config, &bundle); err != nil {
		return Entry{}, fmt.Errorf("decode Kubernetes compatibility: %w", err)
	}
	if bundle.APIVersion != kubernetesbundle.APIVersion || bundle.Kind != kubernetesbundle.BundleKind || bundle.Name != "katl-kubernetes" || bundle.ArtifactKind != "katl.kubernetes-payload.v1" {
		return Entry{}, fmt.Errorf("invalid Kubernetes bundle identity")
	}
	if bundle.PayloadVersion != request.KubernetesVersion {
		return Entry{}, fmt.Errorf("published payload %s does not match requested %s", bundle.PayloadVersion, request.KubernetesVersion)
	}
	architecture, runtime := request.Architecture, request.RuntimeInterface
	if architecture == "" {
		architecture = "x86_64"
	}
	if runtime == "" {
		runtime = "katl-runtime-1"
	}
	if bundle.Architecture != architecture || !contains(bundle.SupportedRuntimeInterfaces, runtime) {
		return Entry{}, fmt.Errorf("bundle does not support architecture %s and runtime %s", architecture, runtime)
	}
	roles := map[string]bool{"systemd-sysext": false, "sysext-metadata": false, "package-provenance": false, "catalog-fragment": false}
	for _, descriptor := range append(bundle.Payloads, bundle.Metadata...) {
		seen, known := roles[descriptor.Role]
		if !known || seen {
			return Entry{}, fmt.Errorf("unexpected or duplicate Kubernetes payload role %q", descriptor.Role)
		}
		roles[descriptor.Role] = true
	}
	for role, present := range roles {
		if !present {
			return Entry{}, fmt.Errorf("missing Kubernetes payload role %s", role)
		}
	}
	if err := payloadbundle.VerifyDescriptors(fetched.Manifest, append(bundle.Payloads, bundle.Metadata...)); err != nil {
		return Entry{}, err
	}
	entry := Entry{
		ArtifactVersion:   bundle.ArtifactVersion,
		KubernetesVersion: bundle.PayloadVersion,
		Bundle:            repository + "@" + fetched.ManifestDigest,
		Architectures:     []string{bundle.Architecture}, RuntimeInterfaces: bundle.SupportedRuntimeInterfaces,
	}
	if err := Validate(Catalog{APIVersion: APIVersion, Kind: Kind, Entries: []Entry{entry}}); err != nil {
		return Entry{}, err
	}
	return entry, nil
}
