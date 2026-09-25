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

const componentsHeading = "## Included components\n"

func writeReleaseComponents(dir string, flavours []string) error {
	var section strings.Builder
	section.WriteString(componentsHeading + "\nInstalled RPM versions from the shipped package inventories (version-release.architecture). Release-owned extension versions come from the OCI bundles selected by each image. Kubernetes extensions are distributed separately.\n\n| Flavour | Image | Kernel | systemd | containerd | crun |\n| --- | --- | --- | --- | --- | --- |\n")
	seen := make(map[string]bool)
	var releaseVersion string
	var extensions []struct {
		flavour string
		name    string
		version string
	}
	for _, value := range flavours {
		value, err := flavour.Normalize(value)
		if err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("duplicate release flavour %q", value)
		}
		seen[value] = true
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
			extensions = append(extensions, struct {
				flavour string
				name    string
				version string
			}{flavour: value, name: extension.Name, version: extension.PayloadVersion})
		}
		kernel := "kernel-core"
		if value == flavour.LTS {
			kernel = "kernel-longterm-core"
		}
		for _, image := range []string{"installer", "runtime"} {
			name := publicationName("katl-"+image+".packages.tsv", value)
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			packages, err := resourcetest.ParseRPMPackages(strings.NewReader(string(data)))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			required := []string{kernel, "systemd"}
			if image == "runtime" {
				required = append(required, "containerd", "crun")
			}
			versions := []string{value, image}
			for _, component := range required {
				var matches []string
				for _, pkg := range packages {
					if pkg.Name == component {
						matches = append(matches, pkg.NEVRA)
					}
				}
				if len(matches) != 1 {
					return fmt.Errorf("%s: expected exactly one %s package, found %d", name, component, len(matches))
				}
				if strings.ContainsAny(matches[0], "|`\r\n") {
					return fmt.Errorf("%s: invalid version for %s", name, component)
				}
				versions = append(versions, "`"+strings.TrimPrefix(matches[0], "0:")+"`")
			}
			if image == "installer" {
				versions = append(versions, "—", "—")
			}
			fmt.Fprintf(&section, "| %s |\n", strings.Join(versions, " | "))
		}
	}
	section.WriteString("\n| Flavour | Release extension | Version |\n| --- | --- | --- |\n")
	for _, extension := range extensions {
		fmt.Fprintf(&section, "| %s | %s | `%s` |\n", extension.flavour, extension.name, extension.version)
	}
	path := filepath.Join(dir, "RELEASE_NOTES.md")
	notes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := string(notes)
	// Bundling replaces the per-flavour table while preserving the change notes.
	if strings.HasPrefix(body, componentsHeading) {
		next := strings.Index(body[len(componentsHeading):], "\n## ")
		if next < 0 {
			return fmt.Errorf("%s: missing release notes after component table", path)
		}
		body = body[len(componentsHeading)+next+1:]
	}
	return os.WriteFile(path, []byte(section.String()+"\n"+body), 0o644)
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
