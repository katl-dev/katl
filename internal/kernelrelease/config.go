package kernelrelease

import (
	"fmt"
	"path/filepath"
	"strings"
)

func (set Set) Config(profile, directory string) (string, error) {
	identity, err := set.Digest()
	if err != nil {
		return "", err
	}
	if profile != "runtime" && profile != "installer-image" {
		return "", fmt.Errorf("kernel inputs support runtime or installer-image profiles")
	}
	if !filepath.IsAbs(directory) || strings.ContainsAny(directory, "\n\r\x00") {
		return "", fmt.Errorf("kernel RPM directory must be an absolute path")
	}
	var result strings.Builder
	fmt.Fprintf(&result, "[Content]\nPackageDirectories=%s\nPackages=\n", directory)
	for _, pkg := range set.Packages {
		if pkg.Role == "runtime" {
			fmt.Fprintf(&result, "    /repository/%s\n", pkg.FileName)
		}
	}
	if profile == "runtime" {
		for _, pkg := range set.Packages {
			if pkg.Role == "build" {
				fmt.Fprintf(&result, "BuildPackages=/repository/%s\n", pkg.FileName)
			}
		}
	}
	// mkosi 26 fingerprints local RPM names and mtimes, not package contents.
	// Namespace its incremental images by the verified set's content identity.
	fmt.Fprintf(&result, "\n[Build]\nCacheKey=katl-%s-%s\n", profile, identity)
	return result.String(), nil
}
