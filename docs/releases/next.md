## Retry a host upgrade after a failed staging

A host upgrade that failed while staging the new OS (`failed-needs-repair`) could leave the inactive root slot unusable for every later upgrade, even after a reboot. The next `katlctl node upgrade` was refused with `find inactive root slot: found 1 root partitions with type ..., want 2`.

This release lets the node accept that slot and puts it back in its normal state before staging. Retrying in the same boot now works.

Affects nodes running v2026.11.0-beta.1 through v2026.11.4 whose upgrade failed during staging. The node runs its installed agent to plan the upgrade, so a node that is already refused cannot upgrade to this release until you restore the slot once by hand. The repair is one-time and per node: nodes that have never had a failed upgrade need no action. `katlctl node upgrade` prints these steps, with the node's disk filled in, when it meets a refused node.

To restore the slot, run as root on the node:

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
