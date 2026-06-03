package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zariel/katl/internal/installer/disk"
)

const (
	APIVersion = "install.katl.dev/v1alpha1"
	Kind       = "InstallManifest"
)

type Manifest struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Node       NodeConfig    `json:"node"`
	Install    InstallConfig `json:"install"`
	Artifacts  Artifacts     `json:"artifacts"`
}

type NodeConfig struct {
	Identity NodeIdentity `json:"identity"`
}

type NodeIdentity struct {
	Hostname string      `json:"hostname"`
	SSH      SSHIdentity `json:"ssh"`
}

type SSHIdentity struct {
	AuthorizedKeys []string `json:"authorizedKeys"`
}

type InstallConfig struct {
	AllowDestructiveInstall bool         `json:"allowDestructiveInstall"`
	TargetDisk              DiskSelector `json:"targetDisk"`
	ExtraDisks              []ExtraDisk  `json:"extraDisks,omitempty"`
}

type DiskSelector struct {
	ByID       string `json:"byID,omitempty"`
	WWN        string `json:"wwn,omitempty"`
	Serial     string `json:"serial,omitempty"`
	MinSizeMiB uint64 `json:"minSizeMiB,omitempty"`
}

type ExtraDisk struct {
	Name       string       `json:"name"`
	Selector   DiskSelector `json:"selector"`
	Filesystem string       `json:"filesystem"`
	Mount      ExtraMount   `json:"mount"`
	Wipe       bool         `json:"wipe,omitempty"`
}

type ExtraMount struct {
	Path string `json:"path"`
}

type Artifacts struct {
	RuntimeRoot Artifact        `json:"runtimeRoot"`
	UKI         *Artifact       `json:"uki,omitempty"`
	Sysexts     []NamedArtifact `json:"sysexts,omitempty"`
}

type Artifact struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"sizeBytes,omitempty"`
}

type NamedArtifact struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"sizeBytes,omitempty"`
}

type RootDiskProfile struct {
	ESPSizeMiB      uint64
	XBOOTLDRSizeMiB uint64
	RootSlotSizeMiB uint64
	StateFilesystem string
	StateMinSizeMiB uint64
	InitialRootSlot disk.RootSlot
}

func Decode(reader io.Reader) (Manifest, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode install manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, fmt.Errorf("decode install manifest: multiple JSON values")
	}
	if manifest.APIVersion != APIVersion {
		return Manifest{}, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if manifest.Kind != Kind {
		return Manifest{}, fmt.Errorf("kind must be %s", Kind)
	}
	if !manifest.Install.AllowDestructiveInstall {
		return Manifest{}, fmt.Errorf("install.allowDestructiveInstall must be true")
	}
	if strings.TrimSpace(manifest.Node.Identity.Hostname) == "" {
		return Manifest{}, fmt.Errorf("node.identity.hostname is required")
	}
	if len(manifest.Node.Identity.SSH.AuthorizedKeys) == 0 {
		return Manifest{}, fmt.Errorf("node.identity.ssh.authorizedKeys must not be empty")
	}
	if err := validateDiskSelector("install.targetDisk", manifest.Install.TargetDisk); err != nil {
		return Manifest{}, err
	}
	if err := validateArtifact("artifacts.runtimeRoot", manifest.Artifacts.RuntimeRoot); err != nil {
		return Manifest{}, err
	}
	if manifest.Artifacts.UKI != nil {
		if err := validateArtifact("artifacts.uki", *manifest.Artifacts.UKI); err != nil {
			return Manifest{}, err
		}
	}
	for i, sysext := range manifest.Artifacts.Sysexts {
		if strings.TrimSpace(sysext.Name) == "" {
			return Manifest{}, fmt.Errorf("artifacts.sysexts[%d].name is required", i)
		}
		if err := validateArtifact(fmt.Sprintf("artifacts.sysexts[%d]", i), Artifact{
			URL:       sysext.URL,
			SHA256:    sysext.SHA256,
			SizeBytes: sysext.SizeBytes,
		}); err != nil {
			return Manifest{}, err
		}
	}
	for i, extra := range manifest.Install.ExtraDisks {
		if strings.TrimSpace(extra.Name) == "" {
			return Manifest{}, fmt.Errorf("install.extraDisks[%d].name is required", i)
		}
		if err := validateDiskSelector(fmt.Sprintf("install.extraDisks[%d].selector", i), extra.Selector); err != nil {
			return Manifest{}, err
		}
		if strings.TrimSpace(extra.Filesystem) == "" {
			return Manifest{}, fmt.Errorf("install.extraDisks[%d].filesystem is required", i)
		}
		if strings.TrimSpace(extra.Mount.Path) == "" {
			return Manifest{}, fmt.Errorf("install.extraDisks[%d].mount.path is required", i)
		}
	}
	return manifest, nil
}

func validateDiskSelector(field string, selector DiskSelector) error {
	selected := 0
	for _, value := range []string{selector.ByID, selector.WWN, selector.Serial} {
		if strings.TrimSpace(value) != "" {
			selected++
		}
	}
	if selected != 1 {
		return fmt.Errorf("%s must set exactly one of byID, wwn, or serial", field)
	}
	return nil
}

func validateArtifact(field string, artifact Artifact) error {
	if strings.TrimSpace(artifact.URL) == "" {
		return fmt.Errorf("%s.url is required", field)
	}
	if strings.TrimSpace(artifact.SHA256) == "" {
		return fmt.Errorf("%s.sha256 is required", field)
	}
	return nil
}

func DefaultRootDiskProfile() RootDiskProfile {
	return RootDiskProfile{
		ESPSizeMiB:      disk.DefaultESPSizeMiB,
		RootSlotSizeMiB: 8192,
		StateFilesystem: "ext4",
		StateMinSizeMiB: 8192,
		InitialRootSlot: disk.RootSlotA,
	}
}

func BuildDiskLayoutRequest(manifest Manifest, profile RootDiskProfile, runtimeRootSizeMiB uint64) (disk.DiskLayoutRequest, error) {
	if profile.ESPSizeMiB == 0 {
		profile.ESPSizeMiB = disk.DefaultESPSizeMiB
	}
	if profile.RootSlotSizeMiB == 0 {
		profile.RootSlotSizeMiB = 8192
	}
	if profile.StateFilesystem == "" {
		profile.StateFilesystem = "ext4"
	}
	if profile.StateMinSizeMiB == 0 {
		profile.StateMinSizeMiB = 8192
	}
	if profile.InitialRootSlot == "" {
		profile.InitialRootSlot = disk.RootSlotA
	}

	extraDisks := make([]disk.ExtraDiskRequest, 0, len(manifest.Install.ExtraDisks))
	for _, extra := range manifest.Install.ExtraDisks {
		extraDisks = append(extraDisks, disk.ExtraDiskRequest{
			Name:       extra.Name,
			Selector:   diskSelector(extra.Selector),
			Filesystem: extra.Filesystem,
			MountPath:  extra.Mount.Path,
			Wipe:       extra.Wipe,
		})
	}

	return disk.DiskLayoutRequest{
		TargetDisk:         diskSelector(manifest.Install.TargetDisk),
		ESPSizeMiB:         profile.ESPSizeMiB,
		XBOOTLDRSizeMiB:    profile.XBOOTLDRSizeMiB,
		RootA:              disk.RootSlotRequest{SizeMiB: profile.RootSlotSizeMiB},
		RootB:              disk.RootSlotRequest{SizeMiB: profile.RootSlotSizeMiB},
		State:              disk.StatePartitionRequest{Filesystem: profile.StateFilesystem, MinSizeMiB: profile.StateMinSizeMiB},
		ExtraDisks:         extraDisks,
		InitialRootSlot:    profile.InitialRootSlot,
		RuntimeRootSizeMiB: runtimeRootSizeMiB,
	}, nil
}

func diskSelector(selector DiskSelector) disk.TargetDiskSelector {
	return disk.TargetDiskSelector{
		ByID:       selector.ByID,
		WWN:        selector.WWN,
		Serial:     selector.Serial,
		MinSizeMiB: selector.MinSizeMiB,
	}
}
