package agent

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/disk"
)

// systemDiskGeneratorRelease is the first KatlOS release whose runtime finds
// /efi and /var on the disk holding its root. Earlier runtimes mount them by
// GPT partition label from whichever disk the kernel links first.
//
// A generation records its runtime only by version: the runtime's units live
// in its root slot image, which is not mounted while another generation runs.
// Only published release versions identify a runtime's contents. Development
// builds, versioned 0.0.0-dev or from the nearest earlier tag with a -local.
// suffix, are taken to be built from a tree that has the generator.
var systemDiskGeneratorRelease = [3]int{2026, 11, 2}

// runtimeReleasePattern matches published release versions, including
// alpha, beta and rc prereleases.
var runtimeReleasePattern = regexp.MustCompile(`^(\d{4})\.(\d+)\.(\d+)(?:-(?:alpha|beta|rc)\.\d+)?$`)

// mountsSystemDiskByLabel reports whether a runtime release predates the
// system disk generator.
func mountsSystemDiskByLabel(version string) bool {
	match := runtimeReleasePattern.FindStringSubmatch(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if match == nil {
		return false
	}
	var release [3]int
	for i := range release {
		release[i], _ = strconv.Atoi(match[i+1])
	}
	return slices.Compare(release[:], systemDiskGeneratorRelease[:]) < 0
}

// refuseLabelMountedSystemDisk refuses to select a generation whose runtime
// mounts /efi and /var by partition label while another disk carries those
// labels, since that runtime could boot with the other disk's state.
func refuseLabelMountedSystemDisk(ctx context.Context, devices volumeDevices, run ToolRunner, root, generationID string) error {
	spec, _, err := generation.ReadGeneration(root, generationID)
	if err != nil {
		return err
	}
	if !mountsSystemDiskByLabel(spec.Root.RuntimeVersion) {
		return nil
	}
	facts, err := discoverVolumeFacts(ctx, devices.source(run))
	if err != nil {
		return err
	}
	system, err := systemDiskDevice(facts, devices.systemDisk)
	if err != nil {
		return err
	}
	var others []string
	for _, device := range facts.BlockDevices {
		if device.Path == system.Path {
			continue
		}
		var labels []string
		for _, partition := range device.Partitions {
			if partition.GPTLabel == disk.GPTLabelESP || partition.GPTLabel == disk.GPTLabelState {
				labels = append(labels, partition.GPTLabel)
			}
		}
		if len(labels) == 0 {
			continue
		}
		name := device.Path
		if serial := strings.TrimSpace(device.Serial); serial != "" {
			name += " (serial " + serial + ")"
		}
		others = append(others, name+" with "+strings.Join(labels, " and "))
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("generation %s runs KatlOS %s, which mounts /efi and /var by partition label, and another disk carries those labels: %s. Booting it could mount that disk's state instead of this node's. Power off and detach those disks before rolling back, or select a generation of KatlOS %d.%d.%d or later", generationID, spec.Root.RuntimeVersion, strings.Join(others, "; "), systemDiskGeneratorRelease[0], systemDiskGeneratorRelease[1], systemDiskGeneratorRelease[2])
}
