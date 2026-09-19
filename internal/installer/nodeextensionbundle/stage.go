package nodeextensionbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/generation"
)

const (
	sysextRole     = "systemd-sysext"
	provenanceRole = "package-provenance"
	catalogRole    = "catalog-fragment"
)

var ErrInvalidBundle = errors.New("invalid node extension bundle")

type Request struct {
	Source           string
	Ref              string
	CacheDir         string
	RuntimeInterface string
	Architecture     string
	Client           *http.Client
	ActivationPath   string
}

type Staged struct {
	AppID                string
	PayloadVersion       string
	ArtifactVersion      string
	Architecture         string
	BundleManifestDigest string
	SysextPayloadDigest  string
	BundleDir            string
	SysextDir            string
	SysextPath           string
	ExtensionRef         generation.ExtensionRef
}

type ref struct {
	AppID          string
	PayloadVersion string
	BundleDigest   string
}

func FetchAndStage(ctx context.Context, request Request) (Staged, error) {
	if err := validateRequest(request); err != nil {
		return Staged{}, err
	}
	source := strings.TrimRight(strings.TrimSpace(request.Source), "/")
	ref, err := parseRef(request.Ref)
	if err != nil {
		return Staged{}, err
	}
	client := request.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	indexURL := source + "/index.json"
	indexBytes, err := fetch(ctx, client, indexURL)
	if err != nil {
		return Staged{}, fmt.Errorf("fetch node extension index %s: %w", inventory.Redact(indexURL), err)
	}
	var index Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return Staged{}, fmt.Errorf("%w: decode index: %v", ErrInvalidBundle, err)
	}
	entry, err := selectEntry(index, ref, request)
	if err != nil {
		return Staged{}, err
	}

	bundlePath, err := cleanRelativePath("bundle manifest", entry.BundleManifestPath)
	if err != nil {
		return Staged{}, err
	}
	bundleURL := source + "/" + bundlePath
	bundleBytes, err := fetch(ctx, client, bundleURL)
	if err != nil {
		return Staged{}, fmt.Errorf("fetch node extension bundle %s: %w", inventory.Redact(bundleURL), err)
	}
	if digest := sha256Digest(bundleBytes); digest != ref.BundleDigest {
		return Staged{}, fmt.Errorf("%w: bundle manifest digest got %s want %s", ErrInvalidBundle, digest, ref.BundleDigest)
	}
	var bundle Bundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		return Staged{}, fmt.Errorf("%w: decode bundle manifest: %v", ErrInvalidBundle, err)
	}
	if err := validateBundle(bundle, entry, ref, request); err != nil {
		return Staged{}, err
	}

	payload, err := descriptor(bundle.Payloads, sysextRole)
	if err != nil {
		return Staged{}, err
	}
	if payload == nil {
		return Staged{}, fmt.Errorf("%w: missing systemd-sysext payload descriptor", ErrInvalidBundle)
	}
	if payload.Digest != entry.SysextPayloadDigest {
		return Staged{}, fmt.Errorf("%w: bundle sysext digest does not match index entry", ErrInvalidBundle)
	}
	if want := fmt.Sprintf("katl-node-extension-%s-%s-%s.sysext.raw", bundle.AppID, bundle.PayloadVersion, bundle.Architecture); payload.FileName != want {
		return Staged{}, fmt.Errorf("%w: systemd-sysext fileName got %q want %q", ErrInvalidBundle, payload.FileName, want)
	}
	payloadBytes, err := fetchDescriptor(ctx, client, source, *payload, SysextRawMediaType)
	if err != nil {
		return Staged{}, err
	}

	provenance, err := descriptor(bundle.Metadata, provenanceRole)
	if err != nil {
		return Staged{}, err
	}
	if provenance == nil {
		return Staged{}, fmt.Errorf("%w: missing package provenance descriptor", ErrInvalidBundle)
	}
	if provenance.FileName != "package-provenance.json" {
		return Staged{}, fmt.Errorf("%w: package provenance fileName got %q want package-provenance.json", ErrInvalidBundle, provenance.FileName)
	}
	provenanceBytes, err := fetchDescriptor(ctx, client, source, *provenance, PackageMediaType)
	if err != nil {
		return Staged{}, err
	}
	if err := validatePackageProvenance(provenanceBytes, bundle, *payload); err != nil {
		return Staged{}, err
	}

	catalog, err := descriptor(bundle.Metadata, catalogRole)
	if err != nil {
		return Staged{}, err
	}
	if catalog == nil {
		return Staged{}, fmt.Errorf("%w: missing catalog fragment descriptor", ErrInvalidBundle)
	}
	if catalog.FileName != "catalog-entry.json" {
		return Staged{}, fmt.Errorf("%w: catalog fragment fileName got %q want catalog-entry.json", ErrInvalidBundle, catalog.FileName)
	}
	catalogBytes, err := fetchDescriptor(ctx, client, source, *catalog, CatalogEntryMediaType)
	if err != nil {
		return Staged{}, err
	}
	if err := validateCatalogFragment(catalogBytes, bundle, entry, *payload); err != nil {
		return Staged{}, err
	}

	return stage(request, bundle, bundleBytes, payloadBytes, provenanceBytes, catalogBytes, *payload)
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.CacheDir) == "" {
		return fmt.Errorf("cache dir is required")
	}
	if strings.TrimSpace(request.RuntimeInterface) == "" {
		return fmt.Errorf("runtime interface is required")
	}
	if strings.TrimSpace(request.Architecture) == "" {
		return fmt.Errorf("architecture is required")
	}
	source := strings.TrimSpace(request.Source)
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%w: source must be an absolute HTTPS URL", ErrInvalidBundle)
	}
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".raw") || strings.HasSuffix(strings.ToLower(parsed.Path), ".sysext.raw") {
		return fmt.Errorf("%w: raw sysext URLs are not node extension bundle sources", ErrInvalidBundle)
	}
	return nil
}

func parseRef(value string) (ref, error) {
	parts := strings.Split(strings.TrimSpace(value), "@")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return ref{}, fmt.Errorf("%w: ref must be <appID>/<payloadVersion>@sha256:<digest>", ErrInvalidBundle)
	}
	selector := strings.Split(parts[0], "/")
	if len(selector) != 2 || !safePathSegment(selector[0]) || !safeRefComponent(selector[1]) {
		return ref{}, fmt.Errorf("%w: ref must be <appID>/<payloadVersion>@sha256:<digest>", ErrInvalidBundle)
	}
	if err := validateDigest(parts[1]); err != nil {
		return ref{}, fmt.Errorf("%w: ref digest: %v", ErrInvalidBundle, err)
	}
	return ref{AppID: selector[0], PayloadVersion: selector[1], BundleDigest: parts[1]}, nil
}

func selectEntry(index Index, ref ref, request Request) (IndexEntry, error) {
	if index.APIVersion != APIVersion || index.Kind != IndexKind {
		return IndexEntry{}, fmt.Errorf("%w: invalid index header", ErrInvalidBundle)
	}
	for _, entry := range index.Entries {
		if entry.AppID != ref.AppID || entry.PayloadVersion != ref.PayloadVersion || entry.BundleManifestDigest != ref.BundleDigest {
			continue
		}
		if entry.Deprecated {
			return IndexEntry{}, fmt.Errorf("%w: selected bundle is deprecated", ErrInvalidBundle)
		}
		if entry.Architecture != request.Architecture {
			return IndexEntry{}, fmt.Errorf("%w: architecture %q does not match runtime architecture %q", ErrInvalidBundle, entry.Architecture, request.Architecture)
		}
		if !contains(entry.SupportedRuntimeInterfaces, request.RuntimeInterface) {
			return IndexEntry{}, fmt.Errorf("%w: runtime interface %q is unsupported", ErrInvalidBundle, request.RuntimeInterface)
		}
		if len(entry.Capabilities) == 0 {
			return IndexEntry{}, fmt.Errorf("%w: selected bundle declares no capabilities", ErrInvalidBundle)
		}
		if _, err := cleanRelativePath("bundle manifest", entry.BundleManifestPath); err != nil {
			return IndexEntry{}, err
		}
		if _, err := cleanRelativePath("catalog entry", entry.CatalogEntryPath); err != nil {
			return IndexEntry{}, err
		}
		return entry, nil
	}
	return IndexEntry{}, fmt.Errorf("%w: no index entry matches ref %s/%s@%s", ErrInvalidBundle, ref.AppID, ref.PayloadVersion, ref.BundleDigest)
}

func validateBundle(bundle Bundle, entry IndexEntry, ref ref, request Request) error {
	if bundle.APIVersion != APIVersion || bundle.Kind != BundleKind {
		return fmt.Errorf("%w: invalid bundle header", ErrInvalidBundle)
	}
	if bundle.AppID != ref.AppID || bundle.AppID != entry.AppID || !safePathSegment(bundle.AppID) {
		return fmt.Errorf("%w: bundle appID does not match ref", ErrInvalidBundle)
	}
	if !safeRefComponent(bundle.PayloadVersion) {
		return fmt.Errorf("%w: bundle payload version is not a safe ref component", ErrInvalidBundle)
	}
	if bundle.ArtifactKind != ArtifactKind {
		return fmt.Errorf("%w: unexpected artifact kind %q", ErrInvalidBundle, bundle.ArtifactKind)
	}
	if bundle.PayloadVersion != ref.PayloadVersion || bundle.PayloadVersion != entry.PayloadVersion {
		return fmt.Errorf("%w: bundle payload version does not match ref", ErrInvalidBundle)
	}
	if bundle.ArtifactVersion != entry.ArtifactVersion {
		return fmt.Errorf("%w: bundle artifact version does not match index entry", ErrInvalidBundle)
	}
	if bundle.Architecture != request.Architecture || bundle.Architecture != entry.Architecture {
		return fmt.Errorf("%w: bundle architecture is incompatible", ErrInvalidBundle)
	}
	if !stringSlicesEqual(bundle.Compatibility.SupportedRuntimeInterfaces, entry.SupportedRuntimeInterfaces) {
		return fmt.Errorf("%w: bundle runtime interfaces do not match index entry", ErrInvalidBundle)
	}
	if !contains(bundle.Compatibility.SupportedRuntimeInterfaces, request.RuntimeInterface) {
		return fmt.Errorf("%w: bundle does not support runtime interface %q", ErrInvalidBundle, request.RuntimeInterface)
	}
	if !capabilitiesEqual(bundle.Capabilities, entry.Capabilities) {
		return fmt.Errorf("%w: bundle capabilities do not match index entry", ErrInvalidBundle)
	}
	if err := validateCapabilityMetadata(bundle); err != nil {
		return err
	}
	if err := validateExtensionMetadata(bundle); err != nil {
		return err
	}
	if bundle.Provenance.BuildInputDigest == "" {
		return fmt.Errorf("%w: provenance buildInputDigest is required", ErrInvalidBundle)
	}
	if len(bundle.Signatures) == 0 {
		return fmt.Errorf("%w: signature or unsigned-fixture marker is required", ErrInvalidBundle)
	}
	for _, signature := range bundle.Signatures {
		if signature.Type == "unsigned-fixture" {
			return nil
		}
	}
	return fmt.Errorf("%w: v0.1 accepts signed bundles only after signature policy lands; fixture requires unsigned-fixture marker", ErrInvalidBundle)
}

func validateCapabilityMetadata(bundle Bundle) error {
	if len(bundle.Capabilities) == 0 {
		return fmt.Errorf("%w: at least one capability is required", ErrInvalidBundle)
	}
	schemaIDs := map[string]struct{}{}
	for _, capability := range bundle.Capabilities {
		if strings.TrimSpace(capability.Name) == "" || strings.TrimSpace(capability.Version) == "" {
			return fmt.Errorf("%w: capability name and version are required", ErrInvalidBundle)
		}
		for _, id := range capability.ConfigSchemaIDs {
			schemaIDs[id] = struct{}{}
		}
	}
	for _, id := range bundle.Configuration.SupportedConfigSchemaIDs {
		if _, ok := schemaIDs[id]; !ok {
			return fmt.Errorf("%w: configuration schema %q is not declared by any capability", ErrInvalidBundle, id)
		}
	}
	return nil
}

func validateExtensionMetadata(bundle Bundle) error {
	if strings.TrimSpace(bundle.DisplayName) == "" || strings.TrimSpace(bundle.Description) == "" {
		return fmt.Errorf("%w: display name and description are required", ErrInvalidBundle)
	}
	if bundle.Systemd.ExtensionID != "katl-node-extension-"+bundle.AppID {
		return fmt.Errorf("%w: systemd extensionID does not match appID", ErrInvalidBundle)
	}
	if bundle.Systemd.ExtensionVersion != bundle.PayloadVersion {
		return fmt.Errorf("%w: systemd extensionVersion does not match payload version", ErrInvalidBundle)
	}
	if len(bundle.Systemd.EntrypointUnits) == 0 || len(bundle.Systemd.ReadinessUnits) == 0 {
		return fmt.Errorf("%w: systemd entrypoint and readiness units are required", ErrInvalidBundle)
	}
	for _, value := range bundle.Configuration.ConfigHandoffPaths {
		if !strings.HasPrefix(value, "/etc/katl/apps/"+bundle.AppID+"/") {
			return fmt.Errorf("%w: configuration path %q is outside Katl-owned app scope", ErrInvalidBundle, value)
		}
	}
	for _, value := range bundle.Configuration.GeneratedDropInPaths {
		if !strings.HasPrefix(value, "/etc/systemd/system/katl-app-"+bundle.AppID+".service.d/") {
			return fmt.Errorf("%w: generated drop-in path %q is outside Katl-owned app scope", ErrInvalidBundle, value)
		}
	}
	if bundle.Status.LiveStatusPath != "/run/katl/apps/"+bundle.AppID+"/status.json" {
		return fmt.Errorf("%w: live status path is outside Katl app status scope", ErrInvalidBundle)
	}
	if !strings.HasPrefix(bundle.Status.DurableSnapshotPath, "/var/lib/katl/operations/") || !strings.Contains(bundle.Status.DurableSnapshotPath, "/apps/"+bundle.AppID+"/") {
		return fmt.Errorf("%w: durable status path is outside Katl operation app scope", ErrInvalidBundle)
	}
	if strings.TrimSpace(bundle.Status.StatusSchemaID) == "" || strings.TrimSpace(bundle.Status.RedactionVersion) == "" || len(bundle.Status.HealthStates) == 0 {
		return fmt.Errorf("%w: status schema, redaction version, and health states are required", ErrInvalidBundle)
	}
	return nil
}

func validatePackageProvenance(data []byte, bundle Bundle, payload Descriptor) error {
	var provenance packageProvenance
	if err := json.Unmarshal(data, &provenance); err != nil {
		return fmt.Errorf("%w: decode package provenance: %v", ErrInvalidBundle, err)
	}
	if provenance.APIVersion != APIVersion || provenance.Kind != "NodeExtensionPackageProvenance" {
		return fmt.Errorf("%w: invalid package provenance header", ErrInvalidBundle)
	}
	if provenance.AppID != bundle.AppID || provenance.PayloadVersion != bundle.PayloadVersion || provenance.ArtifactVersion != bundle.ArtifactVersion {
		return fmt.Errorf("%w: package provenance identity does not match bundle", ErrInvalidBundle)
	}
	if provenance.SourceRepository == "" || provenance.SourceRevision == "" || provenance.CreatedAt == "" {
		return fmt.Errorf("%w: package provenance source and creation metadata are required", ErrInvalidBundle)
	}
	if provenance.SourceRepository != bundle.Provenance.SourceRepository || provenance.SourceRevision != bundle.Provenance.SourceRevision || provenance.CreatedAt != bundle.Provenance.CreatedAt {
		return fmt.Errorf("%w: package provenance source metadata does not match bundle", ErrInvalidBundle)
	}
	if provenance.BuildInputDigest != payload.Digest || bundle.Provenance.BuildInputDigest != payload.Digest {
		return fmt.Errorf("%w: package provenance build input does not match payload digest", ErrInvalidBundle)
	}
	return nil
}

func validateCatalogFragment(data []byte, bundle Bundle, entry IndexEntry, payload Descriptor) error {
	var fragment IndexEntry
	if err := json.Unmarshal(data, &fragment); err != nil {
		return fmt.Errorf("%w: decode catalog fragment: %v", ErrInvalidBundle, err)
	}
	if fragment.BundleManifestDigest != "" {
		return fmt.Errorf("%w: bundle-local catalog fragment must not include bundle manifest digest", ErrInvalidBundle)
	}
	fragment.BundleManifestDigest = entry.BundleManifestDigest
	if !indexEntriesEqual(fragment, entry) {
		return fmt.Errorf("%w: catalog fragment does not match selected index entry", ErrInvalidBundle)
	}
	if fragment.SysextPayloadDigest != payload.Digest {
		return fmt.Errorf("%w: catalog fragment payload digest does not match bundle", ErrInvalidBundle)
	}
	if !capabilitiesEqual(fragment.Capabilities, bundle.Capabilities) {
		return fmt.Errorf("%w: catalog fragment capabilities do not match bundle", ErrInvalidBundle)
	}
	return nil
}

func descriptor(list []Descriptor, role string) (*Descriptor, error) {
	var found *Descriptor
	for i := range list {
		if list[i].Role == role {
			if found != nil {
				return nil, fmt.Errorf("%w: duplicate %s descriptor", ErrInvalidBundle, role)
			}
			found = &list[i]
		}
	}
	return found, nil
}

func fetchDescriptor(ctx context.Context, client *http.Client, source string, descriptor Descriptor, mediaType string) ([]byte, error) {
	if descriptor.MediaType != mediaType {
		return nil, fmt.Errorf("%w: descriptor %s media type got %q want %q", ErrInvalidBundle, descriptor.Role, descriptor.MediaType, mediaType)
	}
	if err := validateDigest(descriptor.Digest); err != nil {
		return nil, fmt.Errorf("%w: descriptor %s digest: %v", ErrInvalidBundle, descriptor.Role, err)
	}
	if descriptor.SizeBytes <= 0 {
		return nil, fmt.Errorf("%w: descriptor %s size must be positive", ErrInvalidBundle, descriptor.Role)
	}
	if _, err := cleanFileName(descriptor.FileName); err != nil {
		return nil, err
	}
	url := source + "/blobs/sha256/" + strings.TrimPrefix(descriptor.Digest, "sha256:")
	data, err := fetch(ctx, client, url)
	if err != nil {
		return nil, fmt.Errorf("fetch descriptor %s %s: %w", descriptor.Role, inventory.Redact(url), err)
	}
	if got := sha256Digest(data); got != descriptor.Digest {
		return nil, fmt.Errorf("%w: descriptor %s digest got %s want %s", ErrInvalidBundle, descriptor.Role, got, descriptor.Digest)
	}
	if int64(len(data)) != descriptor.SizeBytes {
		return nil, fmt.Errorf("%w: descriptor %s size got %d want %d", ErrInvalidBundle, descriptor.Role, len(data), descriptor.SizeBytes)
	}
	return data, nil
}

func stage(request Request, bundle Bundle, bundleBytes []byte, payloadBytes []byte, provenanceBytes []byte, catalogBytes []byte, payload Descriptor) (Staged, error) {
	bundleDigest := sha256Digest(bundleBytes)
	payloadDigest := sha256Digest(payloadBytes)
	bundleDir := filepath.Join(request.CacheDir, "bundles", digestDir(bundleDigest))
	sysextDir := filepath.Join(request.CacheDir, "sysext", digestDir(payloadDigest))
	tmp := filepath.Join(request.CacheDir, ".tmp-"+strings.TrimPrefix(bundleDigest, "sha256:"))
	if err := os.RemoveAll(tmp); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "bundle"), 0o755); err != nil {
		return Staged{}, fmt.Errorf("create staging temp dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "sysext"), 0o755); err != nil {
		return Staged{}, fmt.Errorf("create sysext temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := os.WriteFile(filepath.Join(tmp, "bundle", "bundle.json"), bundleBytes, 0o644); err != nil {
		return Staged{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bundle", "package-provenance.json"), provenanceBytes, 0o644); err != nil {
		return Staged{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bundle", "catalog-entry.json"), catalogBytes, 0o644); err != nil {
		return Staged{}, err
	}
	payloadName, err := cleanFileName(payload.FileName)
	if err != nil {
		return Staged{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "sysext", payloadName), payloadBytes, 0o644); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(filepath.Dir(sysextDir), 0o755); err != nil {
		return Staged{}, err
	}
	if err := replaceDir(filepath.Join(tmp, "bundle"), bundleDir); err != nil {
		return Staged{}, err
	}
	if err := replaceDir(filepath.Join(tmp, "sysext"), sysextDir); err != nil {
		return Staged{}, err
	}
	if err := writeLocalIndex(request.CacheDir, bundle, bundleDigest, payloadDigest); err != nil {
		return Staged{}, err
	}

	activation := strings.TrimSpace(request.ActivationPath)
	if activation == "" {
		activation = "/run/extensions/katl-node-extension-" + bundle.AppID + ".raw"
	}
	sysextPath := filepath.Join(sysextDir, payloadName)
	return Staged{
		AppID:                bundle.AppID,
		PayloadVersion:       bundle.PayloadVersion,
		ArtifactVersion:      bundle.ArtifactVersion,
		Architecture:         bundle.Architecture,
		BundleManifestDigest: bundleDigest,
		SysextPayloadDigest:  payloadDigest,
		BundleDir:            bundleDir,
		SysextDir:            sysextDir,
		SysextPath:           sysextPath,
		ExtensionRef: generation.ExtensionRef{
			Name:            bundle.AppID,
			Path:            sysextPath,
			ActivationPath:  activation,
			SHA256:          strings.TrimPrefix(payloadDigest, "sha256:"),
			ArtifactVersion: bundle.ArtifactVersion,
			PayloadVersion:  bundle.PayloadVersion,
			Architecture:    bundle.Architecture,
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: append([]string(nil), bundle.Compatibility.SupportedRuntimeInterfaces...),
			},
		},
	}, nil
}

func fetch(ctx context.Context, client *http.Client, value string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, cleanFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<30))
}

func cleanRelativePath(name string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) || hasUnsafePathSegment(trimmed) {
		return "", fmt.Errorf("%w: %s path %q is not relative", ErrInvalidBundle, name, value)
	}
	return cleaned, nil
}

func cleanFileName(value string) (string, error) {
	base := path.Base(strings.TrimSpace(value))
	if base == "." || base == "/" || base != strings.TrimSpace(value) || strings.Contains(base, "..") {
		return "", fmt.Errorf("%w: descriptor fileName %q is not a safe file name", ErrInvalidBundle, value)
	}
	return base, nil
}

func validateDigest(value string) error {
	if !strings.HasPrefix(value, "sha256:") {
		return fmt.Errorf("must start with sha256:")
	}
	hexPart := strings.TrimPrefix(value, "sha256:")
	if len(hexPart) != sha256.Size*2 || hexPart != strings.ToLower(hexPart) {
		return fmt.Errorf("must be lowercase sha256:<hex>")
	}
	_, err := hex.DecodeString(hexPart)
	return err
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestDir(digest string) string {
	return "sha256-" + strings.TrimPrefix(digest, "sha256:")
}

func safePathSegment(value string) bool {
	return value != "" && value == strings.ToLower(value) && !strings.ContainsAny(value, `/\`) && !strings.Contains(value, "..")
}

func safeRefComponent(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsAny(value, `/\`) && !strings.Contains(value, "..")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasUnsafePathSegment(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

func stringSlicesEqual(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func capabilitiesEqual(left []Capability, right []Capability) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].Version != right[i].Version {
			return false
		}
		if !stringSlicesEqual(left[i].ConfigSchemaIDs, right[i].ConfigSchemaIDs) || !stringSlicesEqual(left[i].OperationKinds, right[i].OperationKinds) {
			return false
		}
	}
	return true
}

func indexEntriesEqual(left IndexEntry, right IndexEntry) bool {
	return left.AppID == right.AppID &&
		left.PayloadVersion == right.PayloadVersion &&
		left.ArtifactVersion == right.ArtifactVersion &&
		left.Architecture == right.Architecture &&
		left.BundleManifestDigest == right.BundleManifestDigest &&
		left.BundleManifestPath == right.BundleManifestPath &&
		left.SysextPayloadDigest == right.SysextPayloadDigest &&
		left.CatalogEntryPath == right.CatalogEntryPath &&
		left.Deprecated == right.Deprecated &&
		stringSlicesEqual(left.SupportedRuntimeInterfaces, right.SupportedRuntimeInterfaces) &&
		capabilitiesEqual(left.Capabilities, right.Capabilities)
}

func cleanFetchError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
}

func replaceDir(src string, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func writeLocalIndex(cacheDir string, bundle Bundle, bundleDigest string, payloadDigest string) error {
	index := Index{
		APIVersion: APIVersion,
		Kind:       IndexKind,
		Entries: []IndexEntry{{
			AppID:                      bundle.AppID,
			PayloadVersion:             bundle.PayloadVersion,
			ArtifactVersion:            bundle.ArtifactVersion,
			Architecture:               bundle.Architecture,
			BundleManifestDigest:       bundleDigest,
			BundleManifestPath:         filepath.ToSlash(filepath.Join("bundles", digestDir(bundleDigest), "bundle.json")),
			SysextPayloadDigest:        payloadDigest,
			SupportedRuntimeInterfaces: append([]string(nil), bundle.Compatibility.SupportedRuntimeInterfaces...),
			Capabilities:               copyCapabilities(bundle.Capabilities),
			CatalogEntryPath:           filepath.ToSlash(filepath.Join("bundles", digestDir(bundleDigest), "catalog-entry.json")),
		}},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cacheDir, "index.json"), append(data, '\n'), 0o644)
}
