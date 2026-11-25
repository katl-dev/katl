## Retry a host upgrade after a failed staging

A host upgrade that failed while staging the new OS (`failed-needs-repair`) left the node unable to plan another upgrade until it rebooted. The next `katlctl node upgrade` was refused with `read root partition types …: partition N has PARTUUID …, but sysfs reported "" …; retry the upgrade`, and retrying did not help.

This release keeps the node's view of its root partitions intact during staging, so a retry in the same boot works.

Affects nodes running v2026.11.0-beta.1 through v2026.11.4 whose upgrade failed during staging. Those nodes still run the older agent when they plan the upgrade to this release, so on an affected node, reboot it once, then run the upgrade again. Nodes that have not had a failed upgrade need no action.

If, after a reboot, the upgrade is refused with `find inactive root slot: found 1 root partitions with type …, want 2`, the inactive root slot was left with a different partition type. Restore it once, as root on the node:

1. Find the root slot that is not mounted at `/` and whose PARTTYPE is not `4f68bce3-e8cd-4db1-96e7-fbcaf984b709`:

   ```sh
   lsblk -o NAME,PARTN,PARTTYPE,PARTLABEL,MOUNTPOINT /dev/DISK
   ```

2. Set that slot's type back, with its PARTN in place of PARTNUMBER:

   ```sh
   sfdisk --no-reread --no-tell-kernel --part-type /dev/DISK PARTNUMBER 4f68bce3-e8cd-4db1-96e7-fbcaf984b709
   ```

   Never run this against the partition mounted at `/`.

3. Reboot the node, then run the upgrade again.

`katlctl node upgrade` prints the matching steps, with the node's disk filled in, when it meets either refusal.
