package kubernetesrelease

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "KubernetesSupportedVersions"
)

//go:embed supported-versions.json
var defaultSupportedVersions []byte

var versionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

type SupportedVersions struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Versions   []string `json:"versions"`
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
	supported.Versions = append([]string(nil), supported.Versions...)
	return supported, nil
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
	if len(supported.Versions) == 0 {
		return fmt.Errorf("supported Kubernetes versions must not be empty")
	}
	seen := make(map[string]bool, len(supported.Versions))
	var previous [3]int
	for index, version := range supported.Versions {
		parsed, err := parseVersion(version)
		if err != nil {
			return fmt.Errorf("supported Kubernetes version %d: %w", index, err)
		}
		if seen[version] {
			return fmt.Errorf("supported Kubernetes version %q is duplicated", version)
		}
		seen[version] = true
		if index == 0 {
			previous = parsed
			continue
		}
		if compareVersions(previous, parsed) >= 0 {
			return fmt.Errorf("supported Kubernetes versions must be ordered from oldest to newest")
		}
		previous = parsed
	}
	return nil
}

func parseVersion(version string) ([3]int, error) {
	match := versionPattern.FindStringSubmatch(version)
	if match == nil {
		return [3]int{}, fmt.Errorf("%q must look like v1.36.0", version)
	}
	var parsed [3]int
	for index := range parsed {
		value, err := strconv.Atoi(match[index+1])
		if err != nil {
			return [3]int{}, fmt.Errorf("parse %q: %w", version, err)
		}
		parsed[index] = value
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
