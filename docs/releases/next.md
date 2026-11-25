## Retry a host upgrade after a failed staging

A host upgrade that failed while staging the new OS (`failed-needs-repair`) could leave the inactive root slot unusable for every later upgrade, even after a reboot. The next `katlctl` upgrade plan was refused with `find inactive root slot: found 1 root partitions with type ..., want 2`.

This release lets the node accept that slot and puts it back in its normal state before staging. Retrying in the same boot now works.

Affects nodes running v2026.11.0-beta.1 through v2026.11.4 whose upgrade failed during staging. The node runs its installed agent to plan the upgrade, so a node that is already refused cannot upgrade to this release until you restore the slot by hand. Nodes that have never had a failed upgrade need no action.

To restore the slot, run as root on the node:

1. Find the root partition that is not mounted at `/` and has an unexpected partition type:

   ```sh
   lsblk -o NAME,PARTLABEL,PARTTYPE,MOUNTPOINTS /dev/disk/by-path/DISK
   ```

   The two root slots have PARTTYPE `4f68bce3-e8cd-4db1-96e7-fbcaf984b709`. The slot to fix is the one without a mount point at `/` that shows a different value.

2. Set the type back, naming the disk and the partition number of that slot:

   ```sh
   sfdisk --no-reread --no-tell-kernel --part-type /dev/DISK PARTNUMBER 4f68bce3-e8cd-4db1-96e7-fbcaf984b709
   ```

3. Retry the upgrade. If the plan then reports a PARTUUID mismatch, reboot the node first and retry.

Never run the command against the partition mounted at `/`.
