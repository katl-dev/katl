package disk

import (
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/artifact"
)

func TestRootWriteFirst(t *testing.T) {
	layout := rootLayout(t, RootSlotA)

	plan, err := PlanRootSlotWrite(layout, RootSlotWriteRequest{
		RuntimeArtifact: rootArtifact(),
	})
	if err != nil {
		t.Fatalf("PlanRootSlotWrite() error = %v", err)
	}

	if plan.Slot != RootSlotA {
		t.Fatalf("slot = %q, want root-a", plan.Slot)
	}
	if plan.TargetPartition.GPTLabel != GPTLabelRootA {
		t.Fatalf("target = %#v, want root-a label", plan.TargetPartition)
	}
	if plan.ArtifactDigest != strings.Repeat("a", 64) || plan.ExpectedSizeBytes != 4096 {
		t.Fatalf("artifact plan = %#v", plan)
	}
	if len(plan.DestructiveSteps) == 0 || len(plan.ValidationSteps) == 0 {
		t.Fatalf("steps missing: %#v", plan)
	}
}

func TestRootWriteInactive(t *testing.T) {
	layout := rootLayout(t, RootSlotA)

	plan, err := PlanRootSlotWrite(layout, RootSlotWriteRequest{
		RuntimeArtifact: rootArtifact(),
		CurrentSlot:     RootSlotA,
	})
	if err != nil {
		t.Fatalf("PlanRootSlotWrite() error = %v", err)
	}

	if plan.Slot != RootSlotB {
		t.Fatalf("slot = %q, want root-b", plan.Slot)
	}
	if plan.TargetPartition.GPTLabel != GPTLabelRootB {
		t.Fatalf("target = %#v, want root-b label", plan.TargetPartition)
	}
}

func TestRootWriteRejectsBadArtifact(t *testing.T) {
	layout := rootLayout(t, RootSlotA)

	_, err := PlanRootSlotWrite(layout, RootSlotWriteRequest{
		RuntimeArtifact: artifact.ArtifactVerification{
			Name:      "kubernetes",
			Kind:      artifact.ArtifactSysext,
			SHA256:    strings.Repeat("b", 64),
			SizeBytes: 4096,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime artifact kind") {
		t.Fatalf("PlanRootSlotWrite() error = %v, want kind failure", err)
	}
}

func TestRootWriteRejectsSmallSlot(t *testing.T) {
	layout := rootLayout(t, RootSlotA)
	runtime := rootArtifact()
	runtime.SizeBytes = 5 * 1024 * 1024 * 1024

	_, err := PlanRootSlotWrite(layout, RootSlotWriteRequest{
		RuntimeArtifact: runtime,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds root-a size") {
		t.Fatalf("PlanRootSlotWrite() error = %v, want size failure", err)
	}
}

func rootLayout(t *testing.T, initial RootSlot) DiskLayoutPlan {
	t.Helper()
	plan, err := PlanDiskLayout(layoutFacts(
		diskForLayout("/dev/nvme0n1", "/dev/disk/by-id/nvme-root", 32768),
	), DiskLayoutRequest{
		TargetDisk:         TargetDiskSelector{ByID: "/dev/disk/by-id/nvme-root"},
		RootA:              RootSlotRequest{SizeMiB: 4096},
		RootB:              RootSlotRequest{SizeMiB: 4096},
		State:              StatePartitionRequest{Filesystem: "ext4", MinSizeMiB: 8192},
		InitialRootSlot:    initial,
		RuntimeRootSizeMiB: 2048,
	})
	if err != nil {
		t.Fatalf("PlanDiskLayout() error = %v", err)
	}
	return plan
}

func rootArtifact() artifact.ArtifactVerification {
	return artifact.ArtifactVerification{
		Name:      "runtime-root",
		Kind:      artifact.ArtifactRuntimeRoot,
		SHA256:    strings.Repeat("a", 64),
		SizeBytes: 4096,
	}
}
