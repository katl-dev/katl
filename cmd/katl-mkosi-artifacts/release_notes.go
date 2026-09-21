package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/resourcetest"
)

const componentsHeading = "## Included components\n"

func writeReleaseComponents(dir string, flavours []string) error {
	var section strings.Builder
	section.WriteString(componentsHeading + "\nExact installed RPM versions from the shipped package inventories (version-release.architecture, with RPM epoch where recorded). Kubernetes extensions are distributed separately.\n\n| Flavour | Image | Kernel | systemd | containerd | crun |\n| --- | --- | --- | --- | --- | --- |\n")
	seen := make(map[string]bool)
	for _, value := range flavours {
		value, err := flavour.Normalize(value)
		if err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("duplicate release flavour %q", value)
		}
		seen[value] = true
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
				versions = append(versions, "`"+matches[0]+"`")
			}
			if image == "installer" {
				versions = append(versions, "—", "—")
			}
			fmt.Fprintf(&section, "| %s |\n", strings.Join(versions, " | "))
		}
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
