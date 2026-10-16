// Package kernelrelease owns the retained RPM inputs for a KatlOS kernel.
package kernelrelease

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
)

const (
	ArtifactType    = "application/vnd.katl.kernel-rpms.v1"
	ConfigMediaType = "application/vnd.katl.kernel-rpms.config.v1+json"
	RPMMediaType    = "application/x-rpm"
)

type Package struct {
	Name      string `json:"name"`
	NEVRA     string `json:"nevra"`
	SourceURL string `json:"sourceURL"`
	payloadbundle.Descriptor
}

type Set struct {
	SchemaVersion int       `json:"schemaVersion"`
	FedoraRelease string    `json:"fedoraRelease"`
	Architecture  string    `json:"architecture"`
	Flavour       string    `json:"flavour"`
	KernelRelease string    `json:"kernelRelease"`
	Packages      []Package `json:"packages"`
}

type Lock struct {
	Set
	Reference string `json:"reference"`
}

var releasePattern = regexp.MustCompile(`^[0-9][a-zA-Z0-9._+-]*$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func Names(flavour string) ([]string, error) {
	prefix := "kernel"
	switch flavour {
	case "standard":
	case "lts":
		prefix = "kernel-longterm"
	default:
		return nil, fmt.Errorf("kernel flavour must be standard or lts")
	}
	return []string{prefix + "-core", prefix + "-modules", prefix + "-modules-core", prefix + "-devel"}, nil
}

func (set Set) Validate() error {
	names, err := Names(set.Flavour)
	if err != nil {
		return err
	}
	if set.SchemaVersion != 1 || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(set.FedoraRelease) || set.Architecture != "x86_64" {
		return fmt.Errorf("kernel inputs require schema version 1, a Fedora release and x86_64 architecture")
	}
	if !releasePattern.MatchString(set.KernelRelease) || !strings.HasSuffix(set.KernelRelease, ".fc"+set.FedoraRelease+"."+set.Architecture) {
		return fmt.Errorf("kernel release must match the selected Fedora release and architecture")
	}
	if len(set.Packages) != len(names) {
		return fmt.Errorf("kernel inputs require exactly core, modules, modules-core and devel RPMs")
	}
	for i, name := range names {
		pkg := set.Packages[i]
		role := "runtime"
		if i == len(names)-1 {
			role = "build"
		}
		if pkg.Name != name || pkg.NEVRA != name+"-0:"+set.KernelRelease || pkg.FileName != name+"-"+set.KernelRelease+".rpm" || pkg.Role != role || pkg.MediaType != RPMMediaType {
			return fmt.Errorf("kernel package %d must be %s from the selected kernel build", i, name)
		}
		if !digestPattern.MatchString(pkg.Digest) || pkg.SizeBytes <= 0 || !strings.HasPrefix(pkg.SourceURL, "https://") || strings.ContainsAny(pkg.SourceURL, "\n\r\x00") {
			return fmt.Errorf("kernel package %s requires an HTTPS origin, SHA-256 and size", name)
		}
	}
	return nil
}

func (lock Lock) Validate() error {
	if err := lock.Set.Validate(); err != nil {
		return err
	}
	ref, err := payloadbundle.ParseReference(lock.Reference)
	if err != nil || payloadbundle.ManifestDigest(ref) == "" {
		return fmt.Errorf("kernel lock requires a digest-pinned retained OCI reference")
	}
	return nil
}

func Read(path string) (Lock, error) {
	var lock Lock
	data, err := os.ReadFile(path)
	if err != nil {
		return lock, err
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return lock, err
	}
	return lock, lock.Validate()
}

func Write(path string, lock Lock) error {
	if err := lock.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func (set Set) Digest() (string, error) {
	if err := set.Validate(); err != nil {
		return "", err
	}
	// Mirror choice is capture provenance, not a dependency change. Discovery
	// can encounter identical signed RPM bytes at a different mirror next time.
	set.Packages = append([]Package(nil), set.Packages...)
	for i := range set.Packages {
		set.Packages[i].SourceURL = ""
	}
	data, err := json.Marshal(set)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (set Set) descriptors() []payloadbundle.Descriptor {
	result := make([]payloadbundle.Descriptor, len(set.Packages))
	for i, pkg := range set.Packages {
		result[i] = pkg.Descriptor
	}
	return result
}
