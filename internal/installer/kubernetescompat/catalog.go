package kubernetescompat

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "KubernetesCompatibilityCatalog"
)

//go:embed catalog.json
var defaultCatalog []byte

type Catalog struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Entries    []Entry `json:"entries"`
}

type Entry struct {
	KubernetesVersion string   `json:"kubernetesVersion"`
	Bundle            string   `json:"bundle"`
	Architectures     []string `json:"architectures"`
	RuntimeInterfaces []string `json:"runtimeInterfaces"`
}

type Request struct {
	KubernetesVersion string
	Architecture      string
	RuntimeInterface  string
}

func Resolve(request Request) (Entry, error) {
	catalog, err := Decode(defaultCatalog)
	if err != nil {
		return Entry{}, fmt.Errorf("load embedded Kubernetes compatibility catalog: %w", err)
	}
	version := strings.TrimSpace(request.KubernetesVersion)
	for _, entry := range catalog.Entries {
		if entry.KubernetesVersion != version {
			continue
		}
		if architecture := strings.TrimSpace(request.Architecture); architecture != "" && !contains(entry.Architectures, architecture) {
			return Entry{}, fmt.Errorf("Kubernetes %s is not available for architecture %s", version, architecture)
		}
		if runtime := strings.TrimSpace(request.RuntimeInterface); runtime != "" && !contains(entry.RuntimeInterfaces, runtime) {
			return Entry{}, fmt.Errorf("Kubernetes %s is not compatible with KatlOS runtime interface %s", version, runtime)
		}
		return copyEntry(entry), nil
	}
	return Entry{}, fmt.Errorf("Kubernetes %q is not available in this Katl release; choose a version listed by the release compatibility catalog", version)
}

func Decode(data []byte) (Catalog, error) {
	var catalog Catalog
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode catalog: %w", err)
	}
	if err := Validate(catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func Marshal(catalog Catalog) ([]byte, error) {
	sort.Slice(catalog.Entries, func(i, j int) bool {
		return catalog.Entries[i].KubernetesVersion < catalog.Entries[j].KubernetesVersion
	})
	if err := Validate(catalog); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal catalog: %w", err)
	}
	return append(data, '\n'), nil
}

func Upsert(catalog Catalog, entry Entry) (Catalog, error) {
	updated := false
	for i := range catalog.Entries {
		if catalog.Entries[i].KubernetesVersion == entry.KubernetesVersion {
			catalog.Entries[i] = copyEntry(entry)
			updated = true
			break
		}
	}
	if !updated {
		catalog.Entries = append(catalog.Entries, copyEntry(entry))
	}
	if err := Validate(catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func Validate(catalog Catalog) error {
	if catalog.APIVersion != APIVersion {
		return fmt.Errorf("Kubernetes compatibility catalog apiVersion must be %s", APIVersion)
	}
	if catalog.Kind != Kind {
		return fmt.Errorf("Kubernetes compatibility catalog kind must be %s", Kind)
	}
	if len(catalog.Entries) == 0 {
		return fmt.Errorf("Kubernetes compatibility catalog requires at least one entry")
	}
	seen := map[string]bool{}
	for index, entry := range catalog.Entries {
		image, err := kubernetesbundle.ParseImageReference(entry.Bundle)
		if err != nil {
			return fmt.Errorf("Kubernetes compatibility entry %d bundle: %w", index, err)
		}
		if image.PayloadVersion != entry.KubernetesVersion {
			return fmt.Errorf("Kubernetes compatibility entry %d bundle payload %q does not match version %q", index, image.PayloadVersion, entry.KubernetesVersion)
		}
		if image.ManifestDigest == "" {
			return fmt.Errorf("Kubernetes compatibility entry %d bundle must include an immutable OCI manifest digest", index)
		}
		if seen[entry.KubernetesVersion] {
			return fmt.Errorf("Kubernetes compatibility version %q is duplicated", entry.KubernetesVersion)
		}
		seen[entry.KubernetesVersion] = true
		if err := validateValues("architectures", entry.Architectures); err != nil {
			return fmt.Errorf("Kubernetes compatibility entry %d: %w", index, err)
		}
		if err := validateValues("runtimeInterfaces", entry.RuntimeInterfaces); err != nil {
			return fmt.Errorf("Kubernetes compatibility entry %d: %w", index, err)
		}
	}
	return nil
}

func validateValues(field string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s must not be empty", field)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("%s values must be non-empty and trimmed", field)
		}
		if seen[value] {
			return fmt.Errorf("%s value %q is duplicated", field, value)
		}
		seen[value] = true
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func copyEntry(entry Entry) Entry {
	entry.Architectures = append([]string(nil), entry.Architectures...)
	entry.RuntimeInterfaces = append([]string(nil), entry.RuntimeInterfaces...)
	return entry
}
