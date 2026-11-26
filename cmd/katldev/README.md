# katldev

Use `katldev` from a Katl checkout in the [development shell](../../docs/developing.md#nix-dev-shell)
to build checkout artifacts and keep a persistent libvirt VM world for
hands-on KatlOS journeys through `katlctl`. For the automated harness,
see [vmtest-run](../../scripts/README.md#vmtest-run).

## Command help

Use `-h` or `--help` at any command level, or ask `katldev help` for
a command path. `katldev completion` generates shell completions.
The commands `katldev completion bash`, `katldev completion fish`,
`katldev completion powershell`, and `katldev completion zsh` accept
`--no-descriptions` to omit completion descriptions.

```sh
katldev help world start
katldev completion bash > katldev.bash
```

## Build artifacts

The `katldev build` commands build and verify local artifacts. They
report artifact paths and integrity information. Default build outputs
are under `_build/mkosi`.

| Command | Output |
| --- | --- |
| `katldev build iso` | Self-contained installer ISO with metadata and checksum. |
| `katldev build upgrade` | KatlOS upgrade image with metadata and checksum. |
| `katldev build kubernetes` | Kubernetes upgrade sysext for the supported payload selected with the required `--version` flag. |

The ISO and KatlOS upgrade commands take `--version`, then fall back
to `KATL_VERSION` or derive a version such as `2026.7.0-local.1a2b3c4`
from the nearest release tag and checkout commit. Set `--version` if
the checkout lacks a suitable tag. Use `--release-extensions` with
either command to build and include every release-owned extension.
The build fails if the image metadata omits the prepared manifest.
If a build or verification step fails, inspect its output before
using the artifacts. Compare the reported digest after copying
the ISO to a hypervisor.

```sh
katldev build iso --version 2026.7.0-dev.12
katldev build upgrade --release-extensions
katldev build kubernetes --version v1.36.2
```

## Start and inspect a world

The `katldev world` commands create and manage one persistent world
per checkout. The host needs system libvirt access (default URI
`qemu:///system`, override with `KATL_VMTEST_LIBVIRT_URI`) and
permission to create the world's NAT network. The `katldev installer`
group provides the same lifecycle for a one-VM journey and defaults
to one node in a fresh world.

| Command | Effect |
| --- | --- |
| `katldev world start`, `katldev installer start` | Create the world or start its stopped VMs. A missing VM gets a fresh disk. |
| `katldev world add-node` | Boot another installer, optionally with a positional name. Pass `--control-plane` for a control-plane node. Default names follow `cp-N` and `worker-N`. |
| `katldev world status`, `katldev installer status` | Report the network, VM power state, observed addresses, installer or KatlOS state, and matching config nodes. |
| `katldev world console`, `katldev installer console` | Attach to the serial console. The world form accepts a node name and otherwise selects the first VM. |
| `katldev world stop`, `katldev installer stop` | Request ACPI guest shutdown and wait. The world form accepts one or more node names and otherwise stops all nodes. Use `--timeout` to change the three-minute wait per VM. `--force` cuts power immediately. |
| `katldev world reset`, `katldev installer reset` | Prepare the installer ISO, delete the world's VMs and disks, and boot fresh installers. |
| `katldev world remove` | Delete the world's VMs, disks, and networks without starting new VMs. |

Start from a fresh checkout world, then follow the printed install command
after the installer API is ready. For an additional VM, follow the printed
`katlctl config add-node` command, then the install and join steps it gives.
See [Add one node](../../docs/operations/change-cluster-nodes.md#add-one-node).

```sh
katldev world start --nodes 2
katldev world add-node --control-plane
katldev world status
katldev world console cp-1
katldev world stop worker-1 --timeout 5m
```

On installed nodes, ACPI shutdown allows kubelet to use its configured
graceful shutdown budgets. If a shutdown times out, inspect status and
the console before deciding whether to wait longer. If libvirt guest
shutdown policy suspends VMs, set `ON_SHUTDOWN=shutdown` to avoid leases
and certificates expiring during suspend.

### Installer boot options

The two command groups share these options on `start` and `reset`.
`world add-node` also takes them, except for `--nodes`. Boot options
apply to VMs that the command boots.

| Flag | Effect |
| --- | --- |
| `--nodes` | Set the number of VMs in a new or reset world. A fresh world defaults to one. Reset retains the count unless you override it. To grow an existing world, use `world add-node`. |
| `--memory`, `--cpus`, `--disk-size` | Set guest memory in MiB (default 4096, minimum 1024), vCPUs (default 2), and fresh disk size (default `32G`). |
| `--timeout` | Set the installer DHCP and readiness timeout (default `3m`). |
| `--iso` | Boot an explicit installer ISO, such as a published release. |
| `--no-build` | Reuse the existing installer ISO without checking the checkout. If the ISO is absent, rerun with builds enabled. |
| `--preseed-dir` | Attach a directory as `KATLSEED` media, for example a directory containing `install-input.json` and its inputs. |
| `--manifest` | Generate `KATLSEED` media for automatic install from a manifest. Resolve the file named in `katlosImage.localRef` beside the manifest. |
| `--cmdline` | Append an installer kernel argument. Repeat it for additional arguments. `{gateway}` expands to the world network's host address. The arguments travel through SMBIOS and also apply on installed-system boots in the same VM. |
| `--force-config` | Allow replacement of the generated `cluster.yaml`. Use this when you intend to replace the existing config. |

A preseed directory and a manifest are mutually exclusive. When installers are waiting and the config file is absent, katldev generates
`cluster.yaml` from those installers. If the existing config lists any VM
in the world, katldev retains it by default. If installers are waiting and
the existing config lists none of the VMs, katldev stops with an error.
Inspect the file and use `--force-config` for an intentional replacement.

## Exercise network journeys

| Command | Effect |
| --- | --- |
| `katldev world serve` | Serve the given directory on the host address of the running world network until interrupted. Set `--port` to change the default of 8080. |
| `katldev world readdress` | Move a node's DHCP reservation to the first free address or to a free address you supply with `--address`. The old address stays reserved until the node answers at the new one. |
| `katldev world nat` | Use `--address` to translate a host-side IPv4 address to a VM's reserved address. Use `--remove` to remove the translation. |
| `katldev world multihome` | Attach an extra NIC on an isolated network that the host also reaches. Use `--remove` to detach it. The primary NIC retains the default route. |

For automatic install from an HTTP bundle, start the world first, then
serve the bundle in a separate terminal. With the server running, add
an installer VM with the boot inputs and follow it from the serial
console. Replace the node name and bundle with matching inputs.

```sh
# In a terminal with the world already running
katldev world serve ./bundles --port 8080

# In another terminal
katldev world add-node worker-1 \
  --cmdline 'katl.bundle.url=http://{gateway}:8080/cluster.katlcfg' \
  --cmdline katl.node=worker-1 --cmdline katl.install.mode=auto
katldev world console worker-1
```

For a worker re-address, run `katldev world readdress worker-1`. The
VM takes the new address the next time it requests a lease, typically at
reboot. Follow the printed cluster config change and the
`katlctl cluster apply --endpoint` and `katlctl node reboot --endpoint`
commands. See [Re-address a worker](../../docs/operations/change-cluster-nodes.md#re-address-a-worker).

```sh
katldev world nat cp-1 --address 198.18.0.10
katldev world nat cp-1 --remove
katldev world multihome cp-1
katldev world multihome cp-1 --remove
```

Both topology commands print the management and Kubernetes addresses
to set in `cluster.yaml`. NAT requires an unused unicast address outside
the world's networks and non-interactive `sudo` for iptables. If it
fails, check `sudo -n iptables -L` and rerun the command named in the
error. `start`, `add-node`, and `readdress` can restore recorded NAT
rules after address changes or a host firewall reload. A second NIC is
attached live on a running VM and survives VM recreation within the same
world. Reset and removal discard that configuration.

## State and recovery

The world's identity is stored in `_build/katldev/world-id`. The world
manifest, VM disks, and per-VM state and serial logs live in
`_build/katldev/world/`. Read the serial log named in an installer
readiness error or use `world console`. To inspect the VGA screen,
use `virsh -c LIBVIRT_URI screenshot DOMAIN out.ppm` with the world's
URI and domain name.

The development shell selects `_build/katldev/katlctl.yaml` as
`KATLCTL_CONFIG` for the world's contexts. Outside that shell, use
the export line that katldev prints. The ClusterConfig and management
secrets are separate files under `_build/katldev`.

Katldev checks libvirt ownership metadata before managing the world's
resources. Reset and removal retain the world ID, ClusterConfig, and
management secrets, but erase the world disks. If another network, VPN,
or route occupies the saved subnet, or the checkout still has a legacy
single VM, preserve any required disk contents before running
`katldev world reset` to recreate the world.
