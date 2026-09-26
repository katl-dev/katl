package katlosimage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

// ResolveStagingDirectory reads only the boot contract. The target planner
// interprets the remaining image metadata before the source stages a trial.
func ResolveStagingDirectory(ctx context.Context, root string, expected manifest.KatlosImage) (Payload, error) {
	file, err := os.Open(filepath.Join(root, "katlos/image.json"))
	if err != nil {
		return Payload{}, fmt.Errorf("open KatlOS image index: %w", err)
	}
	defer file.Close()
	var envelope struct {
		APIVersion       string      `json:"apiVersion"`
		Kind             string      `json:"kind"`
		ImageRole        string      `json:"imageRole"`
		Format           string      `json:"format"`
		Version          string      `json:"version"`
		Architecture     string      `json:"architecture"`
		Flavour          string      `json:"flavour"`
		RuntimeInterface string      `json:"runtimeInterface"`
		Components       []Component `json:"components"`
	}
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&envelope); err != nil {
		return Payload{}, fmt.Errorf("decode KatlOS staging envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Payload{}, fmt.Errorf("decode KatlOS staging envelope: multiple JSON values")
	}
	index := Index{
		APIVersion: envelope.APIVersion, Kind: envelope.Kind,
		ImageRole: envelope.ImageRole, Format: envelope.Format,
		Version: envelope.Version, Architecture: envelope.Architecture,
		Flavour: envelope.Flavour, RuntimeInterface: envelope.RuntimeInterface,
	}
	for _, component := range envelope.Components {
		if component.Role == ComponentRuntimeRoot || component.Role == ComponentRuntimeUKI {
			index.Components = append(index.Components, component)
		}
	}
	if err := validateIndex(index, expected); err != nil {
		return Payload{}, err
	}
	byRole := make(map[string]Component, 2)
	for _, component := range index.Components {
		if err := ctx.Err(); err != nil {
			return Payload{}, err
		}
		if _, found := byRole[component.Role]; found {
			return Payload{}, fmt.Errorf("KatlOS staging component role %q appears twice", component.Role)
		}
		if err := validateComponent(root, component, index); err != nil {
			return Payload{}, err
		}
		byRole[component.Role] = component
	}
	runtime, err := required(byRole, ComponentRuntimeRoot)
	if err != nil {
		return Payload{}, err
	}
	boot, err := required(byRole, ComponentRuntimeUKI)
	if err != nil {
		return Payload{}, err
	}
	if boot.Compatibility.RuntimeRoot.ArtifactSHA256 != runtime.SHA256 {
		return Payload{}, fmt.Errorf("runtime UKI root digest does not match runtime root")
	}
	if len(boot.Compatibility.KernelCommandLine) == 0 {
		return Payload{}, fmt.Errorf("runtime UKI kernel command line is required")
	}
	return Payload{Root: root, Index: index, Runtime: runtime, Boot: boot}, nil
}
