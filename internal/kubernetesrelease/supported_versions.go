package kubernetesrelease

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
)

const (
	APIVersion         = "katl.dev/v1alpha1"
	Kind               = "KubernetesSupportedVersions"
	CurrentRecipeScope = "kubernetes-bundle-v2"
)

//go:embed supported-versions.json
var defaultSupportedVersions []byte

var (
	versionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	packagePattern = regexp.MustCompile(`^0:([0-9]+)\.([0-9]+)\.([0-9]+)-[A-Za-z0-9._+~-]+$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	scopePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

type SupportedVersions struct {
	APIVersion   string             `json:"apiVersion"`
	Kind         string             `json:"kind"`
	RecipeScope  string             `json:"recipeScope,omitempty"`
	RecipeDigest string             `json:"recipeDigest"`
	Versions     []SupportedVersion `json:"versions"`
}

type SupportedVersion struct {
	PayloadVersion   string          `json:"payloadVersion"`
	ArtifactRevision int             `json:"artifactRevision"`
	Packages         PackageVersions `json:"packages"`
}

type PackageVersions struct {
	Kubeadm  string `json:"kubeadm"`
	Kubelet  string `json:"kubelet"`
	Kubectl  string `json:"kubectl"`
	CRITools string `json:"criTools"`
}

func (version SupportedVersion) ArtifactVersion() string {
	return fmt.Sprintf("%s-katl.%d", version.PayloadVersion, version.ArtifactRevision)
}

func DefaultSupportedVersions() (SupportedVersions, error) {
	return DecodeSupportedVersions(defaultSupportedVersions)
}

func DecodeSupportedVersions(data []byte) (SupportedVersions, error) {
	var supported SupportedVersions
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&supported); err != nil {
		return SupportedVersions{}, fmt.Errorf("decode supported Kubernetes versions: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return SupportedVersions{}, err
	}
	if err := validateSupportedVersions(supported); err != nil {
		return SupportedVersions{}, err
	}
	supported.Versions = copyVersions(supported.Versions)
	return supported, nil
}

func MarshalSupportedVersions(supported SupportedVersions) ([]byte, error) {
	if err := validateSupportedVersions(supported); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(supported, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal supported Kubernetes versions: %w", err)
	}
	return append(data, '\n'), nil
}

func (supported SupportedVersions) Select(payloadVersion string) ([]SupportedVersion, error) {
	if payloadVersion == "" {
		return copyVersions(supported.Versions), nil
	}
	for _, version := range supported.Versions {
		if version.PayloadVersion == payloadVersion {
			return []SupportedVersion{version}, nil
		}
	}
	return nil, fmt.Errorf("Kubernetes %s is not declared as supported", payloadVersion)
}

func (supported SupportedVersions) ChangedSince(previous SupportedVersions) ([]SupportedVersion, error) {
	recipeChanged := supported.RecipeDigest != previous.RecipeDigest
	scopeChangedWithoutRebuild := supported.RecipeScope != previous.RecipeScope
	previousByPayload := make(map[string]SupportedVersion, len(previous.Versions))
	for _, version := range previous.Versions {
		previousByPayload[version.PayloadVersion] = version
	}
	var changed []SupportedVersion
	for _, version := range supported.Versions {
		old, exists := previousByPayload[version.PayloadVersion]
		if !exists {
			changed = append(changed, version)
			continue
		}
		if old == version {
			if recipeChanged && !scopeChangedWithoutRebuild {
				return nil, fmt.Errorf("Kubernetes bundle recipe changed without advancing %s artifactRevision", version.PayloadVersion)
			}
			continue
		}
		if version.ArtifactRevision <= old.ArtifactRevision {
			return nil, fmt.Errorf("supported Kubernetes %s changed without advancing artifactRevision beyond %d", version.PayloadVersion, old.ArtifactRevision)
		}
		changed = append(changed, version)
	}
	return changed, nil
}

func (supported SupportedVersions) Upsert(version SupportedVersion) (SupportedVersions, bool, error) {
	supported.Versions = copyVersions(supported.Versions)
	found := false
	for index, existing := range supported.Versions {
		if existing.PayloadVersion != version.PayloadVersion {
			continue
		}
		if existing == version {
			return supported, false, nil
		}
		supported.Versions[index] = version
		found = true
		break
	}
	if !found {
		supported.Versions = append(supported.Versions, version)
	}
	sort.Slice(supported.Versions, func(left, right int) bool {
		leftVersion, _ := parseVersion(supported.Versions[left].PayloadVersion)
		rightVersion, _ := parseVersion(supported.Versions[right].PayloadVersion)
		return compareVersions(leftVersion, rightVersion) < 0
	})
	if err := validateSupportedVersions(supported); err != nil {
		return SupportedVersions{}, false, err
	}
	return supported, true, nil
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode supported Kubernetes versions: unexpected trailing JSON")
		}
		return fmt.Errorf("decode supported Kubernetes versions: %w", err)
	}
	return nil
}

func validateSupportedVersions(supported SupportedVersions) error {
	if supported.APIVersion != APIVersion {
		return fmt.Errorf("supported Kubernetes versions apiVersion must be %s", APIVersion)
	}
	if supported.Kind != Kind {
		return fmt.Errorf("supported Kubernetes versions kind must be %s", Kind)
	}
	if supported.RecipeScope != "" && !scopePattern.MatchString(supported.RecipeScope) {
		return fmt.Errorf("supported Kubernetes versions recipeScope must be a lowercase recipe scope")
	}
	if !digestPattern.MatchString(supported.RecipeDigest) {
		return fmt.Errorf("supported Kubernetes versions recipeDigest must be a sha256 digest")
	}
	if len(supported.Versions) == 0 {
		return fmt.Errorf("supported Kubernetes versions must not be empty")
	}
	seen := make(map[string]bool, len(supported.Versions))
	var previous [3]int
	for index, version := range supported.Versions {
		parsed, err := parseVersion(version.PayloadVersion)
		if err != nil {
			return fmt.Errorf("supported Kubernetes version %d: %w", index, err)
		}
		if seen[version.PayloadVersion] {
			return fmt.Errorf("supported Kubernetes version %q is duplicated", version.PayloadVersion)
		}
		seen[version.PayloadVersion] = true
		if index > 0 && compareVersions(previous, parsed) >= 0 {
			return fmt.Errorf("supported Kubernetes versions must be ordered from oldest to newest")
		}
		previous = parsed
		if version.ArtifactRevision < 1 {
			return fmt.Errorf("supported Kubernetes version %s artifactRevision must be at least 1", version.PayloadVersion)
		}
		if err := validatePackages(version, parsed); err != nil {
			return err
		}
	}
	return nil
}

func validatePackages(version SupportedVersion, payload [3]int) error {
	for _, item := range []struct {
		name  string
		value string
		exact bool
	}{
		{name: "kubeadm", value: version.Packages.Kubeadm, exact: true},
		{name: "kubelet", value: version.Packages.Kubelet, exact: true},
		{name: "kubectl", value: version.Packages.Kubectl, exact: true},
		{name: "criTools", value: version.Packages.CRITools},
	} {
		parsed, err := parsePackageVersion(item.value)
		if err != nil {
			return fmt.Errorf("supported Kubernetes version %s package %s: %w", version.PayloadVersion, item.name, err)
		}
		if item.exact && parsed != payload {
			return fmt.Errorf("supported Kubernetes version %s package %s does not match its payload", version.PayloadVersion, item.name)
		}
		if !item.exact && (parsed[0] != payload[0] || parsed[1] != payload[1]) {
			return fmt.Errorf("supported Kubernetes version %s package %s does not match its minor", version.PayloadVersion, item.name)
		}
	}
	return nil
}

func parseVersion(version string) ([3]int, error) {
	return parseNumericVersion(versionPattern.FindStringSubmatch(version), version, "v1.36.0")
}

func parsePackageVersion(version string) ([3]int, error) {
	return parseNumericVersion(packagePattern.FindStringSubmatch(version), version, "0:1.36.0-150500.1.1")
}

func parseNumericVersion(match []string, value, example string) ([3]int, error) {
	if match == nil {
		return [3]int{}, fmt.Errorf("%q must look like %s", value, example)
	}
	var parsed [3]int
	for index := range parsed {
		number, err := strconv.Atoi(match[index+1])
		if err != nil {
			return [3]int{}, fmt.Errorf("parse %q: %w", value, err)
		}
		parsed[index] = number
	}
	return parsed, nil
}

func compareVersions(left, right [3]int) int {
	for index := range left {
		switch {
		case left[index] < right[index]:
			return -1
		case left[index] > right[index]:
			return 1
		}
	}
	return 0
}

func copyVersions(versions []SupportedVersion) []SupportedVersion {
	return append([]SupportedVersion(nil), versions...)
}
