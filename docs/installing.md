# Install KatlOS

Use the release ISO to install KatlOS from USB or virtual media. If you want
to use the installed nodes to create a cluster, follow
[Build your first KatlOS cluster](getting-started.md) from start to finish.
Use this page when you need to select the installer address, disk, or SSH key
yourself.

## Prerequisites

- Choose the installer and `katlctl` from the same
  [KatlOS release](https://github.com/katl-dev/katl/releases). See
  [Release artifacts](reference/release-artifacts.md) for the files for your
  workstation and boot method. If you want to authenticate the downloads, see
  [Verify release artifacts](operations/verify-release.md).
- Use a trusted provisioning network that lets your workstation reach the
  installers on TCP port `8080`. The installer accepts handoff requests without
  authentication.
- Know the target nodes' IP addresses before running `katlctl`. With the
  default ISO networking, use DHCP reservations or read each address from the
  installer console or the DHCP lease table.

## Author one ClusterConfig

Use the [cluster configuration reference](reference/cluster-config.md) to
select disks, set node addresses, and supply native host or kubeadm settings.

For a static address on the installed node, put the static
[systemd-networkd configuration](operations/configure-nodes.md#configure-native-linux-facilities)
in the ClusterConfig and set `management.address` to that address. When the
installer has a temporary address, pass it to `install apply --endpoint`.

## PXE or Matchbox

If your machines boot over the network, follow
[Install KatlOS with PXE and Matchbox](install-pxe-matchbox.md). It covers both
Matchbox profiles and the Katl boot arguments for other PXE tools.

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
hostname, `host:port`, or HTTP(S) base URL.

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

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
```

`--node` selects an entry by name or bootstrap address. You can omit it when
the configuration contains one node. Katl validates the configuration and
installer image before authorizing the disk write. The installer accepts
one valid submission, then closes the handoff path.

If the installer is temporarily at a different address, add
`--endpoint 192.0.2.42` with the current address. This changes the connection
target while keeping the selected node and disk from the file. The command
requires either the node's `management.address` in the configuration or an
explicit `--endpoint`.

The command waits until the installer can reboot or reports a failure. If you use
`--no-wait`, use `katlctl install status --config ./cluster.yaml --node cp-1`
or watch the installer console for the result. Pass the same `--endpoint`
override to `install status` if you used one to install. Once the node boots
from its disk,
[check the installed host](operations/access.md#confirm-generation-0).

For separate [data volumes](reference/cluster-config.md#data-volumes),
`wipe: true` authorizes formatting during installation. With `wipe: false`,
Katl preserves a compatible filesystem and refuses changes that require
formatting.

### Advanced local input

The installer advertises `/v1/config-bundle` for the standard handoff.
A separate seed medium with label `KATLSEED` or disk ID `virtio-katl-seed`
can supply input locally. Put native networkd files under
`etc/systemd/network/` on the seed medium to configure the live installer
before networking starts.

The bundle contains one compiled `install.katl.dev/v1alpha1` `InstallManifest`
per node. The `/v1/install` endpoint and `katl.manifest.*` kernel arguments
remain available for advanced compiled-manifest integrations. A standalone
manifest lacks cluster-wide validation, embedded kubeadm files, and proof
that all nodes come from the same configuration.

## Bootstrap handoff

After all nodes boot from their disks, follow
[Bootstrap Kubernetes](operations/bootstrap-kubernetes.md). It uses the same
configuration to set up Kubernetes and write your kubeconfig.

## Apply runtime configuration

Follow [Apply cluster configuration](operations/configure-nodes.md) to change
supported settings on installed nodes.

## Upgrades

Use [Upgrade a KatlOS host](operations/upgrade-host.md) for the operating system
and [Upgrade Kubernetes](operations/upgrade-kubernetes.md) for Kubernetes.

## Troubleshooting

See [installer diagnostics](operations/troubleshoot.md#installer-evidence) to
inspect a refused install or collect evidence before retrying.
