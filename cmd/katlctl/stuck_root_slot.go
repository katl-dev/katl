package main

import (
	"fmt"
	"regexp"
)

// stuckRootSlotRefusal matches the refusal that agents through v2026.11.4
// give when a failed upgrade staging left the inactive root slot with the
// partial or pending partition type that sysupdate derives from the root type.
// Those agents report neither the system disk nor its partitions, so the
// message is all katlctl can learn: the disk's kernel name.
var stuckRootSlotRefusal = regexp.MustCompile(`find inactive root slot: found 1 root partitions with type [0-9a-fA-F-]{36} on system disk (\S+), want 2`)

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
		return &stuckRootSlotError{cause: err, guide: stuckRootSlotGuidance("/dev/" + match[1])}
	}
	if partialInstallRefusal.MatchString(msg) {
		disk := "/dev/DISK"
		if match := discoveredDisk.FindStringSubmatch(msg); match != nil {
			disk = match[1]
		}
		return &stuckRootSlotError{cause: err, guide: stuckRootSlotGuidance(disk)}
	}
	return err
}

// stuckRootSlotGuidance is the one-time recovery for a node whose failed
// upgrade left the inactive root slot with a partial or pending type and
// leftover UKI files on the ESP. The agent does not report which partition the
// slot is, so the operator finds it from the partition table: sysupdate gives
// the slot the partial or pending type derived from the root type, and Katl
// always labels the staged slot katl_1. The type to restore is the one the
// other root slot carries in the same dump, because `/` is a dm-verity device
// and says nothing about the partition it came from. The same commands appear in
// docs/releases/next.md.
func stuckRootSlotGuidance(disk string) string {
	return fmt.Sprintf(`
This node cannot be upgraded because an earlier failed upgrade left its inactive root slot half-installed. The repair is one-time and per node: only nodes whose upgrade failed need it, and healthy nodes are unaffected.

On the node, as root:

1. Delete the leftover UKI files of the failed upgrade:

     find /efi/EFI/katl -maxdepth 1 -name '.sysupdate.*' -print -delete

2. Make sure the ESP has at least 100 MiB free, and free space if it does not:

     df -h /efi

3. Find the half-installed root slot and the type of the other root slot. This only reads the partition table, from the disk:

     DISK=%s
     sfdisk --dump "$DISK" | {
       stuck=0 roots=0
       while read -r dev colon rest; do
         [[ $colon == : && $rest =~ type=([0-9A-Fa-f-]+) ]] || continue
         type=${BASH_REMATCH[1]^^}
         if [[ $rest == *'name="katl_1"'* && ( $type == 64362E66-88A7-463B-B325-7AF60C81BE06 || $type == 7A01D3C6-92AC-43E9-BD77-5726B602AD9D ) ]]; then
           stuck=$((stuck + 1))
           part=${dev##*[!0-9]}
         elif [[ $type == 4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709 || $type == B921B045-1DF0-41C3-AF44-4C6F280D3FAE ]]; then
           roots=$((roots + 1))
           root=$type
         fi
       done
       if ((stuck != 1)); then
         echo "found $stuck half-installed root slots, expected 1; do not change any partition type" >&2
         exit 1
       fi
       if ((roots != 1)); then
         echo "found $roots other root slots, expected 1; do not change any partition type" >&2
         exit 1
       fi
       echo "half-installed root slot: partition $part"
       echo "other root slot type: $root"
     }

   If it prints both lines, continue with step 4. If it finds no half-installed slot, skip step 4 and go to step 5. If it finds more than one half-installed slot, or anything other than one other root slot, stop and do not change any partition type.

4. Set the half-installed slot to the type of the other root slot, with the partition number and type printed by step 3 in place of PARTNUMBER and ROOTTYPE:

     sfdisk --no-reread --no-tell-kernel --part-type "$DISK" PARTNUMBER ROOTTYPE

   Never run this against the partition mounted at /.

5. Reboot the node, then run the upgrade again.`, disk)
}
