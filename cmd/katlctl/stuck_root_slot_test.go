package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// refusalV20261104 is the error v2026.11.4 returns for a stuck inactive slot.
const refusalV20261104 = "host upgrade preflight: find inactive root slot: found 1 root partitions with type 4f68bce3-e8cd-4db1-96e7-fbcaf984b709 on system disk nvme0n1, want 2"

// sysfsRefusalV20261104 is the error v2026.11.4 returns on a retry in the
// same boot after a failed staging.
const sysfsRefusalV20261104 = `host upgrade preflight: read root partition types on /dev/disk/by-diskseq/1: partition 3 has PARTUUID fb9beb8d-6fac-46ca-94a0-d38cd9aeacd9, but sysfs reported "" for system disk vda; retry the upgrade`

func TestExplainHostUpgradeError(t *testing.T) {
	const wantReboot = `
Retrying the upgrade in the same boot will not work. An earlier failed upgrade left this node's kernel without the partition IDs the upgrade needs, and only a reboot restores them.

Reboot the node once, then run the upgrade again. This is one-time and per node: only nodes whose upgrade failed need it, and nothing changes for healthy nodes.`

	const want = `
After a reboot, this node still cannot be upgraded because an earlier failed upgrade left its inactive root slot with a different partition type. The repair is one-time and per node: only nodes whose upgrade failed need it, and healthy nodes are unaffected.

On the node, as root:

1. Find the root slot that is not mounted at / and whose PARTTYPE is not 4f68bce3-e8cd-4db1-96e7-fbcaf984b709:

     lsblk -o NAME,PARTN,PARTTYPE,PARTLABEL,MOUNTPOINT /dev/nvme0n1

2. Set that slot's type back, with its PARTN in place of PARTNUMBER:

     sfdisk --no-reread --no-tell-kernel --part-type /dev/nvme0n1 PARTNUMBER 4f68bce3-e8cd-4db1-96e7-fbcaf984b709

   Never run this against the partition mounted at /.

3. Reboot the node, then run the upgrade again.`

	t.Run("stuck slot refusal", func(t *testing.T) {
		agentErr := grpcstatus.Error(codes.FailedPrecondition, refusalV20261104)

		got := explainHostUpgradeError(agentErr)

		if wantText := agentErr.Error() + "\n" + want; got.Error() != wantText {
			t.Fatalf("error = %q, want %q", got.Error(), wantText)
		}
		if !errors.Is(got, agentErr) {
			t.Fatalf("explained error no longer wraps the agent's error")
		}
	})

	t.Run("refusal reported by a failed operation", func(t *testing.T) {
		opErr := fmt.Errorf("operation op-1 finished with result failed: find inactive root slot: found 1 root partitions with type 4f68bce3-e8cd-4db1-96e7-fbcaf984b709 on system disk nvme0n1, want 2")

		got := explainHostUpgradeError(opErr)

		if !strings.HasSuffix(got.Error(), want) {
			t.Fatalf("error = %q, want guidance suffix", got.Error())
		}
	})

	t.Run("stale sysfs refusal", func(t *testing.T) {
		agentErr := grpcstatus.Error(codes.FailedPrecondition, sysfsRefusalV20261104)

		got := explainHostUpgradeError(agentErr)

		if wantText := agentErr.Error() + "\n" + wantReboot; got.Error() != wantText {
			t.Fatalf("error = %q, want %q", got.Error(), wantText)
		}
		if !errors.Is(got, agentErr) {
			t.Fatalf("explained error no longer wraps the agent's error")
		}
	})

	t.Run("stale sysfs refusal reported by a failed operation", func(t *testing.T) {
		opErr := errors.New("operation op-1 finished with result failed: " + strings.TrimPrefix(sysfsRefusalV20261104, "host upgrade preflight: "))

		got := explainHostUpgradeError(opErr)

		if !strings.HasSuffix(got.Error(), wantReboot) {
			t.Fatalf("error = %q, want guidance suffix", got.Error())
		}
	})

	t.Run("stale sysfs refusal for another partition and disk", func(t *testing.T) {
		agentErr := grpcstatus.Error(codes.FailedPrecondition, `host upgrade preflight: read root partition types on /dev/nvme0n1: partition 7 has PARTUUID 0a1b2c3d-0000-4000-8000-0123456789ab, but sysfs reported "" for system disk nvme0n1; retry the upgrade`)

		got := explainHostUpgradeError(agentErr)

		if !strings.HasSuffix(got.Error(), wantReboot) {
			t.Fatalf("error = %q, want guidance suffix", got.Error())
		}
	})

	t.Run("other errors", func(t *testing.T) {
		for _, err := range []error{
			grpcstatus.Error(codes.FailedPrecondition, "host upgrade preflight: find inactive root slot: found 3 root partitions with type 4f68bce3-e8cd-4db1-96e7-fbcaf984b709 on system disk sda, want 2"),
			grpcstatus.Error(codes.FailedPrecondition, "host upgrade preflight: target KatlOS image is already installed"),
			errors.New("connection refused"),
		} {
			if got := explainHostUpgradeError(err); got != err {
				t.Errorf("explainHostUpgradeError(%q) = %q, want it unchanged", err, got)
			}
		}
		if got := explainHostUpgradeError(nil); got != nil {
			t.Errorf("explainHostUpgradeError(nil) = %v, want nil", got)
		}
	})
}
