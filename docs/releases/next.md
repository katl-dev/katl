## Retry a host upgrade after a failed staging

A host upgrade that failed while staging the new OS (`failed-needs-repair`) could leave the node unable to upgrade again. The next `katlctl node upgrade` was refused with one of:

- `read root partition types …: partition N has PARTUUID …, but sysfs reported "" …; retry the upgrade`, or `find inactive root slot: found 1 root partitions with type …, want 2`, because the failed staging had changed the inactive root slot's partition type or removed it from the running kernel's view.
- `systemd-sysupdate: … Selected update '1' is already acquired and partially installed. Vacuum it to try installing again.`, because the failed staging left a partial UKI on the ESP, most often after the ESP ran out of space (a 512 MiB ESP holds a UKI of about 90 MiB for each slot, plus whatever else was written there).

This release clears those leftovers before every staging, leaves the kernel's view of the root slots alone, and checks that the ESP has room for the new UKI before it changes anything. When it does not, the upgrade is refused with `insufficient space on the EFI system partition (/efi)`, which says how much to free. After that, free the space and run the upgrade again; no reboot is needed.

### Recovering a node already stuck on v2026.11.0-beta.1 through v2026.11.4

Those releases are affected, and a stuck node plans its upgrade with its installed agent, so it cannot upgrade to this release until the leftovers are removed by hand. As root on the node:

1. Delete sysupdate's leftovers on the ESP:

   ```sh
   find /efi/EFI/katl -maxdepth 1 -name '.sysupdate.*' -print -delete
   ```

2. Make sure `/efi` has at least 100 MiB free (the UKI is about 90 MiB), and remove any files you added there if it does not:

   ```sh
   df -h /efi
   ```

3. Find the inactive root slot and set its partition type back to the root type. Read the partition table from the disk with `sfdisk`; `lsblk` can show a stale type. The inactive slot is the root slot whose partition is not the one mounted at `/`:

   ```sh
   ROOT=$(findmnt -no SOURCE /)
   DISK=/dev/$(lsblk -no PKNAME "$ROOT")
   echo "running root: $ROOT"
   sfdisk --dump "$DISK"
   ```

   In the dump, the inactive slot is the other partition with the same `size=` as `$ROOT`. A node that failed before the root slot was written still shows the root type there, and the command below changes nothing. Set its type, with its number in place of `PARTNUMBER`:

   ```sh
   sfdisk --no-reread --no-tell-kernel --part-type "$DISK" PARTNUMBER 4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709
   ```

   Never run this against `$ROOT`.

4. Reboot the node, then run `katlctl node upgrade` again.

`katlctl node upgrade` prints the matching steps when it meets either refusal.
