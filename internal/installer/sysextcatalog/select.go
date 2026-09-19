package sysextcatalog

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
)

const KubernetesName = "kubernetes"

var minorVersionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)

type SelectionRequest struct {
	Catalog          Catalog
	Version          string
	Runtime          Runtime
	ArtifactBasePath string
	ActivationPath   string
	Existing         *generation.ExtensionRef
}

func Select(request SelectionRequest) (generation.ExtensionRef, error) {
	rawVersion := strings.TrimSpace(request.Version)
	version := rawVersion
	if rawVersion == "default" {
		version = ""
	}
	if err := validateRequestedVersion(version); err != nil {
		return generation.ExtensionRef{}, err
	}
	if rawVersion == "" && request.Existing != nil {
		if err := validateExisting(request.Existing, request.Runtime); err != nil {
			return generation.ExtensionRef{}, err
		}
		return *request.Existing, nil
	}

	if err := Validate(request.Catalog); err != nil {
		return generation.ExtensionRef{}, err
	}
	var candidates []Entry
	for _, entry := range request.Catalog.Entries {
		if entry.Name != KubernetesName {
			continue
		}
		if matchesVersion(entry, version) {
			if err := ValidateForRuntime(entry, request.Runtime); err == nil {
				candidates = append(candidates, entry)
			}
		}
	}
	if len(candidates) == 0 {
		return generation.ExtensionRef{}, fmt.Errorf("%w: no Kubernetes sysext matches request %q for architecture %q and runtime interface %q", ErrInvalidCatalog, versionOrDefault(version), request.Runtime.Architecture, request.Runtime.Interface)
	}

	sort.Slice(candidates, func(i, j int) bool {
		if compare := comparePayloadVersion(candidates[i].PayloadVersion, candidates[j].PayloadVersion); compare != 0 {
			return compare > 0
		}
		return candidates[i].ArtifactVersion > candidates[j].ArtifactVersion
	})
	ref, err := extensionRef(candidates[0], request.ArtifactBasePath, request.ActivationPath)
	if err != nil {
		return generation.ExtensionRef{}, err
	}
	root := generation.RootSelection{
		RuntimeInterface: request.Runtime.Interface,
		Architecture:     request.Runtime.Architecture,
	}
	if err := generation.ValidatePair(root, ref); err != nil {
		return generation.ExtensionRef{}, err
	}
	return ref, nil
}

func matchesVersion(entry Entry, version string) bool {
	switch {
	case version == "":
		return true
	case minorVersionRE.MatchString(version):
		return entry.KubernetesMinor == version
	default:
		return entry.PayloadVersion == version
	}
}

func validateRequestedVersion(version string) error {
	if version == "" {
		return nil
	}
	if minorVersionRE.MatchString(version) || payloadVersionRE.MatchString(version) {
		return nil
	}
	return fmt.Errorf("%w: Kubernetes sysext request %q must be \"default\", vMAJOR.MINOR, or vMAJOR.MINOR.PATCH", ErrInvalidCatalog, version)
}

func extensionRef(entry Entry, artifactBasePath string, activationPath string) (generation.ExtensionRef, error) {
	refPath := entry.LocalPath
	if strings.TrimSpace(entry.LocalPath) != "" {
		cleanLocalPath, err := cleanRelativeLocalPath(entry.LocalPath)
		if err != nil {
			return generation.ExtensionRef{}, err
		}
		refPath = cleanLocalPath
	} else {
		refPath = entry.URL
	}
	if strings.TrimSpace(artifactBasePath) != "" && strings.TrimSpace(entry.LocalPath) != "" {
		refPath = path.Join(strings.TrimRight(artifactBasePath, "/"), refPath)
	}
	if strings.TrimSpace(activationPath) == "" {
		activationPath = "/run/extensions/" + path.Base(refPath)
	}
	return generation.ExtensionRef{
		Name:            entry.Name,
		Path:            refPath,
		ActivationPath:  activationPath,
		SHA256:          entry.SHA256,
		ArtifactVersion: entry.ArtifactVersion,
		PayloadVersion:  entry.PayloadVersion,
		Architecture:    entry.Architecture,
		Compatibility: generation.ExtensionCompatibility{
			RuntimeInterfaces: append([]string(nil), entry.RuntimeInterfaces...),
		},
	}, nil
}

func validateExisting(existing *generation.ExtensionRef, runtime Runtime) error {
	if existing == nil {
		return fmt.Errorf("%w: existing Kubernetes sysext is required", ErrInvalidCatalog)
	}
	if existing.Name != KubernetesName {
		return fmt.Errorf("%w: existing sysext name %q is not %q", ErrInvalidCatalog, existing.Name, KubernetesName)
	}
	if strings.TrimSpace(existing.Path) == "" || strings.TrimSpace(existing.ActivationPath) == "" {
		return fmt.Errorf("%w: existing Kubernetes sysext path and activation path are required", ErrInvalidCatalog)
	}
	if strings.TrimSpace(existing.SHA256) == "" || strings.TrimSpace(existing.ArtifactVersion) == "" || strings.TrimSpace(existing.PayloadVersion) == "" || strings.TrimSpace(existing.Architecture) == "" {
		return fmt.Errorf("%w: existing Kubernetes sysext artifact metadata is required", ErrInvalidCatalog)
	}
	if err := validateSHA256(existing.SHA256); err != nil {
		return fmt.Errorf("%w: existing Kubernetes sysext SHA-256 is invalid: %v", ErrInvalidCatalog, err)
	}
	if KubernetesMinor(existing.PayloadVersion) == "" {
		return fmt.Errorf("%w: existing Kubernetes sysext payload version %q must be vMAJOR.MINOR.PATCH", ErrInvalidCatalog, existing.PayloadVersion)
	}
	root := generation.RootSelection{
		RuntimeInterface: runtime.Interface,
		Architecture:     runtime.Architecture,
	}
	if err := generation.ValidatePair(root, *existing); err != nil {
		return fmt.Errorf("%w: existing Kubernetes sysext is incompatible: %v", ErrInvalidCatalog, err)
	}
	return nil
}

func cleanRelativeLocalPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: local artifact path is required", ErrInvalidCatalog)
	}
	if path.IsAbs(value) || filepath.IsAbs(value) {
		return "", fmt.Errorf("%w: local artifact path %q must be relative", ErrInvalidCatalog, value)
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: local artifact path %q escapes artifact directory", ErrInvalidCatalog, value)
	}
	return cleaned, nil
}

func comparePayloadVersion(left string, right string) int {
	leftParts, leftOK := parsePayloadVersion(left)
	rightParts, rightOK := parsePayloadVersion(right)
	if !leftOK || !rightOK {
		return strings.Compare(left, right)
	}
	for i := range leftParts {
		if leftParts[i] > rightParts[i] {
			return 1
		}
		if leftParts[i] < rightParts[i] {
			return -1
		}
	}
	return 0
}

func parsePayloadVersion(version string) ([3]int, bool) {
	var parsed [3]int
	match := payloadVersionRE.FindStringSubmatch(version)
	if match == nil {
		return parsed, false
	}
	for i := range parsed {
		var value int
		if _, err := fmt.Sscanf(match[i+1], "%d", &value); err != nil {
			return parsed, false
		}
		parsed[i] = value
	}
	return parsed, true
}

func versionOrDefault(version string) string {
	if strings.TrimSpace(version) == "" {
		return "default"
	}
	return version
}
