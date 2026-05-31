package manifest

import (
	"encoding/json"
	"fmt"
	"io"

	"git.cbannister.xyz/chris/katl/internal/installer/disk"
)

const (
	APIVersion = "install.katl.dev/v1alpha1"
	Kind       = "InstallManifest"
)

type Manifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Node       json.RawMessage `json:"node,omitempty"`
	Install    InstallConfig   `json:"install"`
	Artifacts  json.RawMessage `json:"artifacts,omitempty"`
	Etc        json.RawMessage `json:"etc,omitempty"`
	Trust      json.RawMessage `json:"trust,omitempty"`
	Boot       json.RawMessage `json:"boot,omitempty"`
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
	if manifest.APIVersion != APIVersion {
		return Manifest{}, fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if manifest.Kind != Kind {
		return Manifest{}, fmt.Errorf("kind must be %s", Kind)
	}
	if !manifest.Install.AllowDestructiveInstall {
		return Manifest{}, fmt.Errorf("install.allowDestructiveInstall must be true")
	}
	return manifest, nil
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
