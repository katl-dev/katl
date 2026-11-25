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

// stuckRootSlotError is a host-upgrade failure that katlctl can explain. It
// keeps the agent's message and unwraps to it.
type stuckRootSlotError struct {
	cause error
	guide string
}

func (e *stuckRootSlotError) Error() string { return e.cause.Error() + "\n" + e.guide }
func (e *stuckRootSlotError) Unwrap() error { return e.cause }

// explainHostUpgradeError adds recovery guidance to a root slot refusal and
// returns any other error unchanged. Every host-upgrade failure path passes
// its error through here.
func explainHostUpgradeError(err error) error {
	if err == nil {
		return nil
	}
	match := stuckRootSlotRefusal.FindStringSubmatch(err.Error())
	if match == nil {
		return err
	}
	return &stuckRootSlotError{cause: err, guide: stuckRootSlotGuidance(strings.ToLower(match[1]), match[2])}
}

// stuckRootSlotGuidance is the recovery for a node whose inactive root slot
// kept a partial or pending type after a failed upgrade. The agent does not
// report which partition that is, so the operator finds it with lsblk.
func stuckRootSlotGuidance(rootType, diskName string) string {
	return fmt.Sprintf(`
This node cannot be upgraded because an earlier failed upgrade left its inactive root slot with a different partition type, and rebooting does not clear it. The repair is one-time and per node: only nodes whose upgrade failed need it, and healthy nodes are unaffected.

On the node, as root:

1. Find the root slot that is not mounted at / and whose PARTTYPE is not %[1]s:

     lsblk -o NAME,PARTN,PARTTYPE,PARTLABEL,MOUNTPOINT %[2]s

2. Set that slot's type back, with its PARTN in place of PARTNUMBER:

     sfdisk --no-reread --no-tell-kernel --part-type %[2]s PARTNUMBER %[1]s

   Never run this against the partition mounted at /.

3. Reboot the node, then run the upgrade again.`, rootType, "/dev/"+diskName)
}
