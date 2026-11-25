## Retry a host upgrade after a failed staging

A host upgrade that failed while staging the new OS (`failed-needs-repair`) could leave the node unable to upgrade again. The next `katlctl node upgrade` was refused with one of:

- `read root partition types …: partition N has PARTUUID …, but sysfs reported "" …; retry the upgrade`, or `find inactive root slot: found 1 root partitions with type …, want 2`, because the failed staging had changed the inactive root slot's partition type or removed it from the running kernel's view.
- `systemd-sysupdate: … Selected update '1' is already acquired and partially installed. Vacuum it to try installing again.`, because the failed staging left a partial UKI on the ESP, most often after the ESP ran out of space (a 512 MiB ESP holds a UKI of about 90 MiB for each slot, plus whatever else was written there).

This release clears those leftovers before every staging, leaves the kernel's view of the root slots alone, and checks that the ESP has room for the new UKI before it changes anything. When it does not, the upgrade is refused with `insufficient space on the EFI system partition (/efi)`, which says how much to free. After that, free the space and run the upgrade again; no reboot is needed.

### Recovering a node already stuck on v2026.11.0 through v2026.11.4

Those releases are affected, and a stuck node plans its upgrade with its installed agent, so it cannot upgrade to this release until the leftovers are removed by hand. Earlier releases are not affected: they use systemd 259, whose `systemd-sysupdate` neither retypes the slot nor leaves a partially installed update. The repair is one-time and per node; nodes whose upgrade never failed need no action. As root on the node:

1. Delete sysupdate's leftovers on the ESP:

   ```sh
   find /efi/EFI/katl -maxdepth 1 -name '.sysupdate.*' -print -delete
   ```

2. Make sure `/efi` has at least 100 MiB free (the UKI is about 90 MiB), and remove any files you added there if it does not:

   ```sh
   df -h /efi
   ```

3. Find the half-installed root slot, with `DISK` set to the node's system disk (for example `/dev/nvme0n1`). A failed staging leaves it with sysupdate's partial or pending partition type and the label `katl_1`. This only reads the partition table, from the disk, because `lsblk` can show a stale type:

   ```sh
   DISK=/dev/DISK
   sfdisk --dump "$DISK" | awk '
     /name="katl_1"/ && toupper($0) ~ /TYPE=(64362E66-88A7-463B-B325-7AF60C81BE06|7A01D3C6-92AC-43E9-BD77-5726B602AD9D),/ {
       n++
       part = $1
     }
     END {
       if (n != 1) {
         print "found " n+0 " half-installed root slots, expected 1; do not change any partition type" > "/dev/stderr"
         exit 1
       }
       sub(/^.*[^0-9]/, "", part)
       print "half-installed root slot: partition " part
     }'
   ```

   If it prints a partition, continue with step 4. If it finds none, the node failed before the slot was retyped: skip step 4 and go to step 5. If it finds more than one, stop and do not change any partition type.

4. Set that slot's type to the type of the running root slot, with the partition number from step 3 in place of `PARTNUMBER` (`3` for `/dev/nvme0n1p3`):

   ```sh
   ROOT_TYPE=$(sfdisk --part-type "$DISK" "$(lsblk -no PARTN "/dev/block/$(findmnt -no MAJ:MIN /)")")
   echo "$ROOT_TYPE"
   sfdisk --no-reread --no-tell-kernel --part-type "$DISK" PARTNUMBER "$ROOT_TYPE"
   ```

   Never run this against the partition mounted at `/`.

5. Reboot the node, then run `katlctl node upgrade` again.

`katlctl node upgrade` prints these steps, with the node's disk filled in, when it meets the `found 1 root partitions` or `partially installed` refusal. For the `sysfs reported` refusal it asks for a reboot only.
