# Install KatlOS

Use the release ISO to install KatlOS from USB or virtual media. If you want
to use the installed nodes to create a cluster, follow
[Build your first KatlOS cluster](getting-started.md) from start to finish.
Use this page when you need to select the installer address, disk, or SSH key
yourself.

| Boot method and network | Procedure |
| --- | --- |
| ISO with DHCP on the provisioning network | Continue on this page. |
| ISO with a static address, or a workstation on another subnet | [Advanced installer networking](install-advanced.md) |
| Network boot with PXE, iPXE, or Matchbox | [Install KatlOS with PXE and Matchbox](install-pxe-matchbox.md) |
| Installer on the system disk's ESP, without USB or network boot | [Boot the installer from the system disk](#boot-the-installer-from-the-system-disk) |

## Prerequisites

- Choose the installer and `katlctl` from the same
  [KatlOS release](https://github.com/katl-dev/katl/releases). See
  [Release artifacts](reference/release-artifacts.md) for the files for your
  workstation and boot method. If you want to authenticate the downloads, see
  [Verify release artifacts](operations/verify-release.md).
- Give each machine at least 2 GiB of RAM; see the
  [supported surface](support.md#supported-home-lab-surface).
- Use a trusted provisioning network that lets your workstation reach the
  installers on TCP port `8080`, because the installer accepts handoff
  requests without authentication; see [Network trust](support.md#network-trust).
- Know the target nodes' IP addresses before running `katlctl`. With the
  default ISO networking, use DHCP reservations or read each address from the
  installer console or the DHCP lease table.

## ISO or local handoff

Boot `katl-installer.iso` in UEFI mode. Keep Secure Boot disabled. The
installer waits without touching the disks until it receives an install
request.

The console on `tty1` shows the release, address, handoff URL, disk mutation
state, and a live journal. Use `Ctrl+Alt+F2` for a local recovery shell. The
dashboard, serial journal, and installer SSH service operate independently.

### Create a configuration from waiting installers

From your workstation, give `katlctl` the address of each installer you intend
to use.

```sh
katlctl config init ./cluster.yaml \
  --installer 192.0.2.11 \
  --installer 192.0.2.21
```

Replace both addresses with those of your installers. The first address
becomes `cp-1`. Bare addresses use HTTP port `8080`. The flag also accepts a
hostname, `host:port`, or HTTP(S) base URL. To add a machine to an existing
`cluster.yaml` later, use `katlctl config add-node` as described in
[Add one node](operations/change-cluster-nodes.md#add-one-node).

The result is one ClusterConfig for every node. Edit `cluster.yaml` with the
[cluster configuration reference](reference/cluster-config.md) to select
disks, set node addresses, and supply native host or kubeadm settings. For a
static address on the installed node, put the static
[systemd-networkd configuration](operations/configure-nodes.md#configure-native-linux-facilities)
in `cluster.yaml` and set `management.address` to that address.

### Select the target disk

Configuration generation from live installers requires one selectable disk
per node. A selectable disk is a writable whole disk with a stable identity
and no mounted filesystems. If there is more than one, read the disk inventory
on the installer and identify the target by a stable by-id path, WWN, or
serial number. Supply the intended disk explicitly when creating the
configuration.

```sh
katlctl config init ./cluster.yaml \
  --node cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-KATL_CP_1_ROOT \
  --node worker-1=worker,192.0.2.21,/dev/disk/by-id/ata-KATL_WORKER_1_ROOT
```

Replace the sample addresses and disk paths. Use a stable selector in
`cluster.yaml` so a device name such as `/dev/sda` cannot direct the install
to a different disk.

### Select an SSH key

Configuration generation imports supported public keys from the active SSH
agent, including 1Password, or uses `~/.ssh/id_ed25519.pub`. Pass
`--ssh-authorized-key PATH` to select a public key explicitly. If Katl found
no key, add an authorized key to `cluster.yaml` before applying it.

### Apply the configuration

Run `katlctl config validate ./cluster.yaml`. Check that the selected system
disk matches the intended machine and can be erased. The next command
replaces the contents of that disk.

For separate [data volumes](reference/cluster-config.md#data-volumes),
[`wipe: true`](reference/cluster-config.md#wipe-authorization) authorizes formatting during installation. With `wipe: false`,
Katl preserves a compatible filesystem and refuses changes that require
formatting.

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
```

`--node` selects an entry by its name or its `management.address`. Katl
validates the configuration and installer image before authorizing the disk
write. The installer accepts one valid submission, then closes the handoff path.

Without `--node` or `--endpoint`, `install apply` installs every configured
node, one at a time in `cluster.yaml` order, without prompting. Each node gets
its own `--timeout`. Katl skips a node whose installer is unreachable or not
waiting for configuration and continues with the next node; if no node was
waiting, the command exits with status 1. Skipped nodes are not listed in text
output; add `--verbose` to log each skipped node and the reason. With
`--output json`, a skipped node's report has `skipped: true` and a
`skipReason`; a configuration with several nodes produces an array of reports.
In this mode, every node needs a `management.address`.

The command connects to the installer at the node's `management.address`. If
the installer is currently at another address, such as a temporary address
before the installed node takes its static address, pass that address with
`--endpoint 192.0.2.42`; the node and disk still come from `cluster.yaml`. If
the node has no `management.address`, `--endpoint` is required.

The command waits until the installer can reboot or reports a failure. If you use
`--no-wait`, use `katlctl install status --config ./cluster.yaml --node cp-1`
or watch the installer console for the result. Pass the same `--endpoint`
override to `install status` if you used one to install. Once the node boots
from its disk,
[check the installed host](operations/access.md#confirm-generation-0).

## Boot the installer from the system disk

A machine with one disk and no USB or network boot can start the installer
from that disk. Give the disk a GPT with one FAT32 EFI System Partition, and
copy the release's `katl-installer.efi` to `/EFI/BOOT/BOOTX64.EFI` on it. UEFI
firmware boots that removable-media path without a boot entry. Do not label the
filesystem `KATL_INSTALLER`: the installer mounts a filesystem with that label
as its install media, which makes the disk ineligible as a target.

The installer runs from RAM. It does not mount the ESP it booted from, and it
stages the configuration and OS image in memory, so the same disk is
selectable as the system disk. Because `katl-installer.efi` carries no OS
image, compile the configuration into a bundle that names a KatlOS image URL
the installer can reach, as described in
[Compile one cluster configuration bundle](install-pxe-matchbox.md#compile-one-cluster-configuration-bundle),
and apply the bundle:

```sh
katlctl install apply --config ./cluster.katlcfg --node cp-1
```

Katl then replaces the disk's partition layout, including the ESP that held
the installer, installs its own boot loader, and reboots into generation 0
without a firmware boot-order change.

## Next steps

After all nodes boot from their disks, follow
[Bootstrap Kubernetes](operations/bootstrap-kubernetes.md) with the same
`cluster.yaml`. If an install is refused, use the
[installer diagnostics](operations/troubleshoot.md#installer-evidence) to
inspect it and collect evidence before retrying.
