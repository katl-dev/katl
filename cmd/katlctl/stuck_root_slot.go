package main

import (
	"fmt"
	"regexp"
	"strings"
)

// stuckRootSlotRefusal matches the refusal that agents through v2026.11.4
// give when a failed upgrade staging left the inactive root slot with the
// partial or pending partition type that sysupdate derives from the root type.
// Those agents report neither the system disk nor its partitions, so the
// message is all katlctl can learn: the root type and the disk's kernel name.
var stuckRootSlotRefusal = regexp.MustCompile(`find inactive root slot: found 1 root partitions with type ([0-9a-fA-F-]{36}) on system disk (\S+), want 2`)

// partialInstallRefusal matches systemd-sysupdate's refusal to install an
// update whose root slot was written but whose UKI was not. The agent reports
// it as the failure of the staging step. It names the disk sysupdate
// discovered but neither the root type nor the slot.
var partialInstallRefusal = regexp.MustCompile(`already acquired and partially installed`)

// discoveredDisk extracts the disk from sysupdate's "Automatically discovered
// root block device" line.
var discoveredDisk = regexp.MustCompile(`root block device '(/dev/[^']+)'`)

// staleSysfsRefusal matches the refusal that agents through v2026.11.4 give
// when a failed staging left the kernel reporting no PARTUUID for the inactive
// root partition. The disk keeps the root type, so a reboot clears it.
var staleSysfsRefusal = regexp.MustCompile(`read root partition types on \S+: partition \d+ has PARTUUID [0-9a-fA-F-]+, but sysfs reported "[^"]*" for system disk \S+; retry the upgrade`)

// staleSysfsGuidance explains that retrying in the same boot cannot succeed.
const staleSysfsGuidance = `
Retrying the upgrade in the same boot will not work. An earlier failed upgrade left this node's kernel without the partition IDs the upgrade needs, and only a reboot restores them.

Reboot the node once, then run the upgrade again. This is one-time and per node: only nodes whose upgrade failed need it, and nothing changes for healthy nodes.`

// stuckRootSlotError is a host-upgrade failure that katlctl can explain. It
// keeps the agent's message and unwraps to it.
type stuckRootSlotError struct {
	cause error
	guide string
}

func (e *stuckRootSlotError) Error() string { return e.cause.Error() + "\n" + e.guide }
func (e *stuckRootSlotError) Unwrap() error { return e.cause }

// explainHostUpgradeError adds recovery guidance to a stuck root slot refusal
// or a stale partition ID refusal and returns any other error unchanged. Every
// host-upgrade failure path passes its error through here.
func explainHostUpgradeError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if staleSysfsRefusal.MatchString(msg) {
		return &stuckRootSlotError{cause: err, guide: staleSysfsGuidance}
	}
	if match := stuckRootSlotRefusal.FindStringSubmatch(msg); match != nil {
		return &stuckRootSlotError{cause: err, guide: stuckRootSlotGuidance("whose PARTTYPE is not "+strings.ToLower(match[1]), "/dev/"+match[2], strings.ToLower(match[1]))}
	}
	if partialInstallRefusal.MatchString(msg) {
		disk := "/dev/DISK"
		if match := discoveredDisk.FindStringSubmatch(msg); match != nil {
			disk = match[1]
		}
		const rootType = "ROOT_TYPE"
		return &stuckRootSlotError{cause: err, guide: stuckRootSlotGuidance("whose PARTTYPE differs from the partition mounted at / (ROOT_TYPE is that partition's PARTTYPE)", disk, rootType)}
	}
	return err
}

// stuckRootSlotGuidance is the one-time recovery for a node whose failed
// upgrade left the inactive root slot with a partial or pending type and
// leftover UKI files on the ESP. The agent does not report which partition the
// slot is, so the operator finds it with lsblk.
func stuckRootSlotGuidance(slotTest, disk, rootType string) string {
	return fmt.Sprintf(`
This node cannot be upgraded because an earlier failed upgrade left its inactive root slot half-installed. The repair is one-time and per node: only nodes whose upgrade failed need it, and healthy nodes are unaffected.

On the node, as root:

1. Delete the leftover UKI files of the failed upgrade:

     rm -f /efi/EFI/katl/.sysupdate.partial.* /efi/EFI/katl/.sysupdate.pending.*

2. Make sure the ESP has at least 90 MiB free, and free space if it does not:

     df -h /efi

3. Find the root slot that is not mounted at / and %[1]s:

     lsblk -o NAME,PARTN,PARTTYPE,PARTLABEL,MOUNTPOINT %[2]s

   Set that slot's type back, with its PARTN in place of PARTNUMBER:

     sfdisk --no-reread --no-tell-kernel --part-type %[2]s PARTNUMBER %[3]s

   Never run this against the partition mounted at /.

4. Reboot the node, then run the upgrade again.`, slotTest, disk, rootType)
}
