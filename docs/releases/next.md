## Upgrade to keep nodes reachable after a power loss

Every release from 2026.9.0 through 2026.11.7 can leave a node without
network after a power loss that comes shortly after `katlctl cluster bootstrap`
or a live `katlctl cluster apply`. The release wrote the new boot entry and
told the firmware to boot it before the entry had reached the disk, so after
the power loss the node starts an older configuration that does not match its
recorded state. The console then shows `State: Needs repair` and
`Network: Waiting for an active interface`, and `katlctl` and SSH cannot
reach the node.

KatlOS 2026.11.8 writes each boot entry to disk before the firmware is told
to boot it. Upgrade `katlctl` and every node to 2026.11.8 with the
[host upgrade procedure](https://github.com/katl-dev/katl/blob/main/docs/operations/upgrade-host.md).

Nodes on 2026.11.4 or 2026.11.6 can upgrade straight to 2026.11.8, and a
cluster whose nodes run different releases can be upgraded one node at a time.
For nodes installed before 2026.11.5, first follow the
[2026.11.5 guidance](https://github.com/katl-dev/katl/blob/main/docs/releases/2026.11.5.md):

- If your configuration has neither `spec.managementAuthentication` nor
  `spec.managementIdentity`, add `managementAuthentication: trusted-network`
  under `spec`.
- Repair any node whose earlier host upgrade failed while staging before you
  upgrade it.
- Run `katlctl management enable-mtls` only after every node runs 2026.11.5
  or later.

### Recover a node that lost its network

A node in this state cannot be reached to upgrade it, so recover it first,
then upgrade it.

The console's advice to run `katlctl cluster apply` cannot help yet, because
the node has no network. First boot the newest configuration that still has a
boot menu entry:

1. Power the node on and tap the space bar from power-on until the boot menu
   appears.
2. Choose the `KatlOS` entry with the latest date. The menu titles show only
   the version and the creation time (see
   [boot menu entries](https://github.com/katl-dev/katl/blob/main/docs/operations/wipe-reinstall.md#boot-menu-entries)),
   and the entry highlighted by default is the oldest one. The configuration
   that was lost has no entry, so the latest entry listed is the one before
   it.

The node boots healthy, takes its address, and keeps booting that entry.
From your workstation, check it and restore the configuration that was lost:

```sh
katlctl node status NODE
katlctl node generations list NODE
```

The lost generation is listed as `cannot boot: boot artifact unavailable`,
followed by the path of its missing entry. It stays listed after recovery and
needs no action. Apply your configuration again, then reboot the node so it
boots the new generation:

```sh
katlctl cluster apply --config cluster.yaml
katlctl node reboot NODE
```

The reboot ends with `NODE rebooted successfully; health OK`, and
`katlctl node status NODE` shows the new generation running.

### Known limitation

2026.11.8 removes the cause, but a node still starts an older configuration
if its selected boot entry is missing for another reason, such as disk damage.
The boot-menu recovery above still returns such a node to service by booting
the newest configuration that has an entry; it does not repair the damage.

## Upgrade from 2026.11.5 or earlier through 2026.11.6

A node on KatlOS 2026.11.5 or earlier upgrades in two steps. First run `katlctl node upgrade NODE --config cluster.yaml --version 2026.11.6`, then upgrade to this release. `katlctl node upgrade` refuses the direct upgrade and prints that command for the node, with the same `--config`, `--context` or `--endpoint` you used.

An older `katlctl`, or an upgrade sent straight to the node, is not stopped. The node then writes its boot records in the format 2026.11.5 reads for as long as the old generation stays bootable, so a failed upgrade returns to it with management intact.

An upgrade or next-boot apply that starts while the node runs a generation other than its default, for example after you chose one from the boot menu, is refused with `select the running generation first with 'katlctl node generations select <running> <node>'` unless the default generation has already run a release that owns boot state. Select the running generation, then retry.

## Boot health changes

- A failed first boot no longer marks the node recovery-required. The generation stays pending and unknown, no reboot follows, and apply stays refused until a boot of it is healthy. Fix the cause and run `katlctl node reboot`.
- Only a boot trial reboots the node after a boot health failure. A failed boot of the default, a boot-menu generation, or a manual selection reports the failure and changes nothing. `katl-boot-recovery.service` names the running generation, the default, the next boot, and the next action in its journal.
- A failed trial with no usable fallback stays on the failed generation, reports that recovery is required, and does not reboot. A healthy boot of that generation, or of a known-good generation chosen from the boot menu, clears the state; the generation that booted becomes the default.
- `katl-boot-deadman.timer` is active only during a boot trial.

## Finishing an interrupted first trial

If power is lost inside the move that fails or promotes the first trial after upgrading from an earlier release, the node keeps a boot-move record that only this release completes. If the node comes back on the old generation, boot the new generation from the boot menu to finish the move. If the old generation already changed its boot records, the new generation discards the outdated move record when it next boots, and the old generation's decision stands.
