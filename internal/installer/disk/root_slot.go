package disk

import (
	"fmt"
	"strings"

	"github.com/zariel/katl/internal/installer/artifact"
)

type RootSlotWriteRequest struct {
	RuntimeArtifact artifact.ArtifactVerification
	CurrentSlot     RootSlot
}

type RootSlotWritePlan struct {
	Slot              RootSlot
	ActiveSlotGuard   RootSlot
	TargetPartition   RootSlotTarget
	ArtifactDigest    string
	ExpectedSizeBytes int64
	DestructiveSteps  []string
	ValidationSteps   []string
}

type RootSlotTarget struct {
	Name       string
	GPTLabel   string
	Filesystem string
	SizeMiB    uint64
}

func PlanRootSlotWrite(layout DiskLayoutPlan, request RootSlotWriteRequest) (RootSlotWritePlan, error) {
	if request.RuntimeArtifact.Kind != artifact.ArtifactRuntimeRoot {
		return RootSlotWritePlan{}, fmt.Errorf("runtime artifact kind = %q, want %q", request.RuntimeArtifact.Kind, artifact.ArtifactRuntimeRoot)
	}
	if strings.TrimSpace(request.RuntimeArtifact.SHA256) == "" {
		return RootSlotWritePlan{}, fmt.Errorf("runtime artifact digest is required")
	}
	if request.RuntimeArtifact.SizeBytes <= 0 {
		return RootSlotWritePlan{}, fmt.Errorf("runtime artifact size must be positive")
	}

	slot, err := selectWriteSlot(layout.Boot.RootSlot, request.CurrentSlot)
	if err != nil {
		return RootSlotWritePlan{}, err
	}

	target, err := findRootSlot(layout, slot)
	if err != nil {
		return RootSlotWritePlan{}, err
	}
	if target.Filesystem != "squashfs" {
		return RootSlotWritePlan{}, fmt.Errorf("%s filesystem = %q, want squashfs", target.Name, target.Filesystem)
	}
	if uint64(request.RuntimeArtifact.SizeBytes) > target.SizeMiB*1024*1024 {
		return RootSlotWritePlan{}, fmt.Errorf("runtime artifact size %d bytes exceeds %s size %d MiB", request.RuntimeArtifact.SizeBytes, target.Name, target.SizeMiB)
	}

	return RootSlotWritePlan{
		Slot:              slot,
		ActiveSlotGuard:   request.CurrentSlot,
		TargetPartition:   target,
		ArtifactDigest:    strings.ToLower(request.RuntimeArtifact.SHA256),
		ExpectedSizeBytes: request.RuntimeArtifact.SizeBytes,
		DestructiveSteps: []string{
			"write runtime artifact to " + target.GPTLabel,
			"flush " + target.GPTLabel,
		},
		ValidationSteps: []string{
			"verify runtime artifact digest before write",
			"verify " + target.GPTLabel + " first artifact-size bytes after write",
			"verify " + target.GPTLabel + " mounts read-only as squashfs",
		},
	}, nil
}

func selectWriteSlot(initial RootSlot, current RootSlot) (RootSlot, error) {
	switch current {
	case "":
		if initial == "" {
			return "", fmt.Errorf("initial root slot is required")
		}
		return initial, nil
	case RootSlotA:
		return RootSlotB, nil
	case RootSlotB:
		return RootSlotA, nil
	default:
		return "", fmt.Errorf("unsupported current root slot %q", current)
	}
}

func findRootSlot(layout DiskLayoutPlan, slot RootSlot) (RootSlotTarget, error) {
	name, label, err := rootSlotPartition(slot)
	if err != nil {
		return RootSlotTarget{}, err
	}
	for _, partition := range layout.Partitions {
		if partition.Name != name {
			continue
		}
		if partition.GPTLabel != label {
			return RootSlotTarget{}, fmt.Errorf("%s label = %q, want %q", name, partition.GPTLabel, label)
		}
		return RootSlotTarget{
			Name:       partition.Name,
			GPTLabel:   partition.GPTLabel,
			Filesystem: partition.Filesystem,
			SizeMiB:    partition.SizeMiB,
		}, nil
	}
	return RootSlotTarget{}, fmt.Errorf("%s partition not found", name)
}

func rootSlotPartition(slot RootSlot) (string, string, error) {
	switch slot {
	case RootSlotA:
		return "root-a", GPTLabelRootA, nil
	case RootSlotB:
		return "root-b", GPTLabelRootB, nil
	default:
		return "", "", fmt.Errorf("unsupported root slot %q", slot)
	}
}
