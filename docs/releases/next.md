## Upgrade to keep nodes reachable after a power loss

Every release from 2026.9.0 through 2026.11.7 can leave a node without
network after a power loss that comes shortly after `katlctl cluster bootstrap`
or a live `katlctl cluster apply`. The release wrote the new boot entry and
told the firmware to boot it before the entry had reached the disk, so after
the power loss the node starts an older configuration that does not match its
recorded state. The console then shows `State: Needs repair` and
`Network: Waiting for an active interface`, and `katlctl` and SSH cannot
reach the node.

KatlOS 2026.11.8 writes boot entries and the boot selection to disk before
the firmware is told to boot them, so a node that loses power after a live
`katlctl cluster apply` returns on the configuration that was applied. Upgrade
`katlctl` and every node to 2026.11.8 with the
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
then upgrade it. The console's advice to run `katlctl cluster apply` cannot
help yet, because the node has no network. Instead, boot the configuration the
node expects from the boot menu.

1. On the node's console, find the boot health line that contains
   `does not match durable boot evidence GENERATION; boot recovery must
   reconcile the selected generation`. `GENERATION` is the configuration to
   boot, for example `0` or `bootstrap-init-20261128T064650Z-8f1f3a3ae5dc-candidate`.
2. Power-cycle the node and tap the space bar repeatedly from power-on until
   the boot menu appears. If the node boots without showing the menu,
   power-cycle it and try again.
3. Find the entry for `GENERATION`. The menu titles show only the version and
   a time (see
   [boot menu entries](https://github.com/katl-dev/katl/blob/main/docs/operations/wipe-reinstall.md#boot-menu-entries)),
   and the time is the CREATED time that `katlctl node generations list`
   shows. To be sure, check each `KatlOS` entry: highlight it, press `e`,
   then `End`, and read the `katl.generation=` value at the end of the line. Press `Esc` to return
   to the menu without changing anything. Note which generations have
   entries.
4. Highlight the entry whose `katl.generation=` matches `GENERATION` and press
   `Enter`. Do not press `d`, and do not edit the line.

The node boots, takes its address, and can be reached again. From your
workstation, check it. Add `--config cluster.yaml` to the `katlctl node`
commands below if you do not use a saved context.

```sh
katlctl node status NODE
katlctl node generations list NODE
```

Then bring it back to its full configuration:

- If `GENERATION` is `0`, the node has booted its installation, which does not
  run Kubernetes. Select the newest other generation that had an entry in the
  boot menu, then reboot. Go by the menu: `katlctl node generations list` can
  show `artifacts present` for a generation whose entry is gone, and selecting
  it strands the node again.

  ```sh
  katlctl node generations select NEWEST NODE
  katlctl node reboot NODE
  ```

- Otherwise, if `katlctl node generations list` shows a generation other than
  `GENERATION` as `next boot`, select the generation you booted so that the
  node keeps booting it:

  ```sh
  katlctl node generations select GENERATION NODE
  ```

Finally, apply your configuration again and reboot the node:

```sh
katlctl cluster apply --config cluster.yaml
katlctl node reboot NODE
```

The reboot ends with `NODE rebooted successfully; health OK`. Generations
whose entries were lost stay listed.

### Known limitations

A worker that loses power within seconds of `katlctl node join` can come back
reachable but with kubelet unable to start. `katlctl node status` then shows
`waiting-for-kubelet`, and running `katlctl node join` again is refused. A
worker stranded shortly after joining on an earlier release can also stay
without Kubernetes after the procedure above. In both cases, wipe the node and
join it again with the
[replace one node](https://github.com/katl-dev/katl/blob/main/docs/operations/change-cluster-nodes.md#replace-one-node-without-renaming-it)
procedure, booting the 2026.11.8 installer.

If a node's selected boot entry is missing for any other reason, such as disk
damage, the boot-menu procedure above is the way back to a configuration that
still has an entry. It does not repair the damage.
