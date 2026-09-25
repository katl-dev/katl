package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
	"github.com/katl-dev/katl/internal/resourcetest"
)

const componentsHeading = "## Packages\n"

type releaseComponents struct {
	kernels    [2]string
	packages   map[string]string
	extensions map[string]string
}

func writeReleaseComponents(dir string, flavours []string) error {
	seen := make(map[string]bool)
	components := make(map[string]releaseComponents)
	var order []string
	var releaseVersion string
	for _, value := range flavours {
		value, err := flavour.Normalize(value)
		if err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("duplicate release flavour %q", value)
		}
		seen[value] = true
		order = append(order, value)
		component := releaseComponents{packages: make(map[string]string), extensions: make(map[string]string)}
		inventoryName := publicationName("katl-runtime.extensions.json", value)
		inventory, err := readReleaseExtensionInventory(filepath.Join(dir, inventoryName), value)
		if err != nil {
			return fmt.Errorf("%s: %w", inventoryName, err)
		}
		if releaseVersion == "" {
			releaseVersion = inventory.Version
		} else if inventory.Version != releaseVersion {
			return fmt.Errorf("%s: release version %q does not match %q", inventoryName, inventory.Version, releaseVersion)
		}
		for _, extension := range inventory.Extensions {
			for field, text := range map[string]string{"name": extension.Name, "payloadVersion": extension.PayloadVersion} {
				if strings.TrimSpace(text) == "" || strings.ContainsAny(text, "|`\r\n") {
					return fmt.Errorf("%s: invalid extension %s", inventoryName, field)
				}
			}
			component.extensions[extension.Name] = extension.PayloadVersion
		}
		kernel := "kernel-core"
		if value == flavour.LTS {
			kernel = "kernel-longterm-core"
		}
		for index, image := range []string{"installer", "runtime"} {
			name := publicationName("katl-"+image+".packages.tsv", value)
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			packages, err := resourcetest.ParseRPMPackages(strings.NewReader(string(data)))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			required := []string{kernel}
			if image == "runtime" {
				required = append(required, "systemd", "containerd", "crun")
			}
			for _, packageName := range required {
				version, err := releasePackageVersion(packages, packageName)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				if packageName == kernel {
					component.kernels[index] = version
				} else {
					component.packages[packageName] = version
				}
			}
		}
		components[value] = component
	}
	if len(order) == 0 {
		return fmt.Errorf("release has no flavours")
	}
	for _, value := range order[1:] {
		for _, name := range []string{"systemd", "containerd", "crun"} {
			if components[value].packages[name] != components[order[0]].packages[name] {
				return fmt.Errorf("%s differs between %s and %s", name, order[0], value)
			}
		}
		for name, version := range components[order[0]].extensions {
			if components[value].extensions[name] != version {
				return fmt.Errorf("extension %s differs between %s and %s", name, order[0], value)
			}
		}
		if len(components[value].extensions) != len(components[order[0]].extensions) {
			return fmt.Errorf("extension set differs between %s and %s", order[0], value)
		}
	}
	for _, value := range order {
		if components[value].kernels[0] != components[value].kernels[1] {
			return fmt.Errorf("installer and runtime kernel versions differ for %s", value)
		}
	}

	var section strings.Builder
	section.WriteString(componentsHeading + "\nInstalled RPM versions from the runtime image and installer kernel. Kubernetes extensions are distributed separately.\n\n")
	section.WriteString("### Kernels\n\n| Package |")
	for _, value := range order {
		fmt.Fprintf(&section, " %s |", releaseFlavourName(value))
	}
	section.WriteString("\n| --- |" + strings.Repeat(" --- |", len(order)) + "\n")
	section.WriteString("| Kernel |")
	for _, value := range order {
		fmt.Fprintf(&section, " `%s` |", components[value].kernels[1])
	}
	section.WriteByte('\n')
	section.WriteString("\n### Versions\n\n| Package | Version |\n| --- | --- |\n")
	for _, name := range []string{"systemd", "containerd", "crun"} {
		fmt.Fprintf(&section, "| %s | `%s` |\n", name, components[order[0]].packages[name])
	}
	section.WriteString("\n### Extensions\n\n| Extension | Version |\n| --- | --- |\n")
	names := make([]string, 0, len(components[order[0]].extensions))
	for name := range components[order[0]].extensions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&section, "| %s | `%s` |\n", name, components[order[0]].extensions[name])
	}
	path := filepath.Join(dir, "RELEASE_NOTES.md")
	notes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := string(notes)
	if strings.HasPrefix(body, "## Included components\n") {
		next := strings.Index(body, "\n## ")
		if next < 0 {
			return fmt.Errorf("%s: missing release notes after component table", path)
		}
		body = body[next+1:]
	}
	start := strings.Index(body, componentsHeading)
	if start >= 0 {
		end := strings.Index(body[start:], "\n## Verify\n")
		if end < 0 {
			return fmt.Errorf("%s: missing verification section after packages", path)
		}
		body = body[:start] + body[start+end+1:]
	}
	verify := strings.Index(body, "## Verify\n")
	if verify < 0 {
		return fmt.Errorf("%s: missing verification section", path)
	}
	return os.WriteFile(path, []byte(body[:verify]+section.String()+"\n"+body[verify:]), 0o644)
}

func releaseFlavourName(value string) string {
	if value == flavour.LTS {
		return "LTS"
	}
	return "Standard"
}

func releasePackageVersion(packages []resourcetest.Package, name string) (string, error) {
	var matches []string
	for _, pkg := range packages {
		if pkg.Name == name {
			matches = append(matches, pkg.NEVRA)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one %s package, found %d", name, len(matches))
	}
	if strings.ContainsAny(matches[0], "|`\r\n") {
		return "", fmt.Errorf("invalid version for %s", name)
	}
	return strings.TrimPrefix(matches[0], "0:"), nil
}

func readReleaseExtensionInventory(path, expectedFlavour string) (releaseExtensionInventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return releaseExtensionInventory{}, err
	}
	var inventory releaseExtensionInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return inventory, err
	}
	if inventory.SchemaVersion != 1 || inventory.ArtifactKind != releaseExtensionInventoryKind {
		return inventory, fmt.Errorf("unsupported release extension inventory")
	}
	if strings.TrimSpace(inventory.Version) == "" || strings.TrimSpace(inventory.Architecture) == "" || strings.TrimSpace(inventory.RuntimeInterface) == "" {
		return inventory, fmt.Errorf("incomplete release extension target")
	}
	if err := (kernelmodule.Target{Release: inventory.KernelRelease, RuntimeSHA256: inventory.RuntimeSHA256}).Validate(); err != nil {
		return inventory, fmt.Errorf("invalid release extension target: %w", err)
	}
	canonical, err := flavour.Normalize(inventory.Flavour)
	if err != nil || canonical != expectedFlavour {
		return inventory, fmt.Errorf("flavour %q does not match %q", inventory.Flavour, expectedFlavour)
	}
	if len(inventory.Extensions) == 0 {
		return inventory, fmt.Errorf("release extension inventory is empty")
	}
	seenNames := make(map[string]bool)
	seenRepositories := make(map[string]bool)
	for _, extension := range inventory.Extensions {
		if extension.Name == "" || extension.PayloadVersion == "" || extension.Repository == "" || extension.Reference == "" {
			return inventory, fmt.Errorf("incomplete release extension entry")
		}
		if err := extensionrelease.ValidateRepository(extension.Repository); err != nil {
			return inventory, err
		}
		ref, err := payloadbundle.ParseReference(extension.Reference)
		if err != nil || payloadbundle.ManifestDigest(ref) == "" || ref.Name() != extension.Repository {
			return inventory, fmt.Errorf("release extension %q reference does not pin its repository", extension.Name)
		}
		if seenNames[extension.Name] || seenRepositories[extension.Repository] {
			return inventory, fmt.Errorf("duplicate release extension %q", extension.Name)
		}
		seenNames[extension.Name] = true
		seenRepositories[extension.Repository] = true
	}
	if !sort.SliceIsSorted(inventory.Extensions, func(i, j int) bool {
		if inventory.Extensions[i].Name != inventory.Extensions[j].Name {
			return inventory.Extensions[i].Name < inventory.Extensions[j].Name
		}
		return inventory.Extensions[i].Repository < inventory.Extensions[j].Repository
	}) {
		return inventory, fmt.Errorf("release extension entries are not sorted")
	}
	return inventory, nil
}
