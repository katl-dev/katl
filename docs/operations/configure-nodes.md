# Apply Cluster Configuration

Use `katlctl cluster apply` for supported configuration changes after
installation. The same `ClusterConfig` remains the source of truth for every
node and for kubeadm-owned Kubernetes configuration.

Apply to every node, or select nodes by name:

```sh
katlctl cluster apply --config ./cluster.yaml
katlctl cluster apply --config ./cluster.yaml --node cp-1
katlctl cluster apply --config ./cluster.yaml --node cp-1 --node worker-1
```

Keep the complete ClusterConfig when using `--node`. Katl validates and applies
configuration only on selected nodes; it does not require other nodes to be
reachable. Unknown node names are rejected, and
repeating the same name does not apply it twice.

Configuration rollouts use a selected control plane to publish shared
configuration; an explicit `--coordinator` must be among the selected nodes.

Apply never joins nodes or changes cluster membership. Use
`katlctl node join NODE --config ./cluster.yaml` to join an installed node to an
existing cluster. An apply that mixes fresh and joined nodes stops before
mutation with join guidance; `--node` can configure a fresh node separately
before joining it. Use `katlctl cluster bootstrap` for a new cluster.

Selection limits node operations, not the scope of shared Kubernetes resources.
Applying shared kubeadm or kube-proxy configuration through a selected control
plane can affect the whole cluster. Apply shared kubelet changes to a control
plane before workers so the shared configuration is available for them to use.
Use per-node kubelet configuration for settings that should affect only one node.

Unchanged configuration is a no-op. Host changes that need a reboot are staged
for the next boot and reported; follow the reported reboot guidance.

## Supported Input

The normal source is the same `ClusterConfig` used for installation. The current
renderer carries:

- SSH authorized keys;
- operator-owned kernel command-line additions;
- native host configuration file sets, including systemd-networkd files and
  drop-ins;
- desired data disks under `storage.volumes`;
- per-node native kubelet configuration under `nodes[].kubernetes.kubelet`;
- operation-only system role and role-dependent Kubernetes bootstrap state.

Runtime-safe fields apply normally. Katl coordinates affected node generations
and kubeadm phases internally. System-disk installation selection and
Kubernetes version changes use the dedicated install and Kubernetes upgrade
workflows; data disks remain desired node storage.

## Inspect Effective Configuration

Resolve one node before applying a config:

```console
katlctl config resolve ./cluster.yaml --node cp-1
```

The YAML output uses ClusterConfig field names and shows the final values after
defaults, named-entry overlays, removals, and explicit empty collections. It
also reports where each effective value came from, derived hostnames, storage
mount paths and GPT labels, Katl-owned `/etc` paths, warnings, and the boundary
between normal apply and lifecycle operations. Use `--output json` for tooling.

Compare two revisions through the same effective per-node model:

```console
katlctl config diff ./cluster-before.yaml ./cluster-after.yaml --node cp-1
```

The default table classifies every changed public path. `operation-only`
changes name the required workflow; `staged-only` changes activate through a
node generation; storage changes call out live discovery and destructive
authorization; `target-only` means workstation targeting changes without a
node generation. Use `--output yaml` or `--output json` to retain before and
after values in review or automation.

If `spec.kubernetes.kubeadm` changes, cluster apply validates every selected node before
mutation and then reconciles every affected Kubernetes component online. A
Kubernetes configuration change never falls back to next-boot application or
requires a host reboot.

Per-node `kubernetes.kubelet.configFile` changes use kubeadm's node-local patch
path. Katl validates and stages the native KubeletConfiguration, refreshes only
that node's `/var/lib/kubelet/config.yaml`, restarts its kubelet, and checks node
health. It does not upload the overlay to the shared kubelet ConfigMap. Removing
the per-node input refreshes that node from the shared kubeadm configuration.
Use `config resolve` to see the selected native input and owned patch path, and
`config diff` to review its `kubeadm-aware operation` classification before
applying.

## Node Lifecycle Matrix

`spec.nodes` is both the install inventory and the set of nodes targeted by
`katlctl cluster apply`. Editing the list is not authority to mutate a node that
is no longer listed. In particular, omission never drains a Kubernetes Node,
removes an etcd member, powers off a host, changes partitions, formats storage,
or wipes KatlOS.

| Desired change | `cluster apply` behavior | Supported operator path |
| --- | --- | --- |
| Reorder nodes or change ordinary supported fields | Applies by stable node name | Run `katlctl cluster apply --config ./cluster.yaml` |
| Add one installed, unenrolled node | Does not join it | Install the new named node from the config, then run `katlctl node join NAME --config ./cluster.yaml` |
| Replace hardware while keeping the node name and role | Does not join or erase a node | While the old node is still listed, run `katlctl node wipe NAME --config ./cluster.yaml --plan`, execute the reviewed wipe with the required kubeconfig, reinstall the replacement, then run `katlctl node join NAME --config ./cluster.yaml` |
| Remove a node from the cluster | Omission only stops Katl targeting; the old node and its data are preserved | Keep the node listed while planning and executing `katlctl node wipe NAME --config ./cluster.yaml --plan`; remove the entry only after the explicit Kubernetes/etcd-aware wipe succeeds |
| Rename an unenrolled installed node | Stages the hostname through normal next-boot configuration | Apply, reboot, verify the new hostname, and only then bootstrap Kubernetes |
| Rename an enrolled node | Refused before any node mutation | Keep the old name, or explicitly wipe it under the old config and reinstall it as a new node |
| Change `controlPlane` / node role | Refused as an operation-only change | Explicitly wipe the named node, reinstall it with the desired role, then use `katlctl node join NAME --config ./cluster.yaml` |
| Change only `management.address` | Changes workstation targeting only | Verify the new address reaches the same node; no node generation or Kubernetes state changes |

Removing one entry and adding another is never inferred to be a rename. It is a
preserved omitted node plus a distinct addition until the operator completes
the explicit removal/reinstall workflow. If an entry was removed too early,
put it back with its old name and address, inspect its current status, and run
the appropriate wipe plan. Do not reinstall merely to make the config match;
preserve and diagnose the existing node first.

The wipe workflow is intentionally separate because it names the affected
node, proves Kubernetes and stacked-etcd removal where required, reports what
disk state is preserved, and stops before installer formatting. See
[Wipe and reinstall KatlOS](wipe-reinstall.md#plan-one-node-replacement).

## Destructive Storage Changes

`wipe: true` authorizes formatting a selected node volume, including erasing
existing contents. `cluster apply` validates every selected node before mutation; no
additional wipe acknowledgement is required. `wipe: false` preserves compatible
filesystems and refuses changes requiring formatting. Reapplying unchanged
configuration or rebooting reuses the bound volume without wiping it again.

Katl records the exact PARTUUID or filesystem UUID selected for every
provisioned volume. Later generations mount that identity directly instead of
following the logical label again. If a selector changes to a different
device, planning fails until the operator supplies the reported one-shot
`--rebind-volume NODE/VOLUME` authority. Use an exact `byID`, `partUUID`, or
`filesystemUUID` selector for the replacement; an ambiguous `byVolumeName`
label remains an error even with rebind authority. Set `wipe: true` if the replacement should be formatted.
`katlctl node status NODE` reports the active exact mount source so the
operator can verify the retained identity before and after the change without
exposing the generation's internal binding metadata as a separate API.

## Configure Kernel Arguments

Set `kernel.commandLine` under defaults or a concrete node:

```yaml
spec:
  defaults:
    kernel:
      commandLine:
        - intel_iommu=on
        - iommu=pt
```

Each entry is one argument without whitespace. Applying a changed list creates
a next-boot generation; `katlctl cluster apply` reports that a reboot is
required, and the new arguments become active after that reboot. Reapplying the
same list is a no-op. To remove all operator additions for one node, set:

```yaml
spec:
  nodes:
    - name: worker-1
      kernel:
        commandLine: []
```

Katl preserves release-required arguments and owns root selection, immutable
runtime mounting, generation and machine identity, and recovery targets.
Attempts to configure those arguments fail validation with the offending list
entry.

## Keep Unwanted Services Stopped

Use `hostConfiguration.maskedUnits` to stop unwanted services and prevent
systemd from starting them again, including through dependencies or manual
start requests:

```yaml
spec:
  defaults:
    hostConfiguration:
      maskedUnits:
        - bluetooth.service
```

Apply with `katlctl cluster apply --config cluster.yaml`. Mask-only changes
apply live without rebooting; the masks persist across reboot and upgrades.
Katl refuses masks for its protected, release-critical units. Masking is
stronger than disabling: disabling only removes enablement links and can
still allow activation through sockets, dependencies, or D-Bus.

A node's `maskedUnits` list replaces the defaults. Set `maskedUnits: []` on a
node to clear inherited masks. Removing a mask restores the vendor unit's
normal activation behavior without starting it as part of the apply. Mask
triggering socket, timer, or path units too when you want to suppress those
activation attempts. Use concrete unit names, including an instance name for
template units.

Verify through the node's SSH interface:

```console
systemctl show bluetooth.service --property=LoadState,ActiveState
```

The masked unit should report `LoadState=masked` and `ActiveState=inactive`.

## Disable Bluetooth Drivers

Masking `bluetooth.service` stops the userspace service. It does not prevent
kernel drivers from loading or retrying missing firmware. For a node that
does not use Bluetooth, add these native kernel arguments to its existing
`kernel.commandLine` list:

```yaml
spec:
  defaults:
    kernel:
      commandLine:
        - module_blacklist=bluetooth,btusb,btmtk
        - modprobe.blacklist=bluetooth,btusb,btmtk
```

`modprobe.blacklist` suppresses automatic loading, while `module_blacklist`
also prevents explicit loading from early boot onward. Preserve any other
operator kernel arguments in the list. These settings leave Wi-Fi drivers
available on combined Wi-Fi/Bluetooth hardware.

Run `katlctl cluster apply --config cluster.yaml`, then reboot affected nodes
with `katlctl node reboot NODE`. Kernel arguments require a reboot; applying
the configuration does not unload drivers from a running node. If service
masks and kernel arguments change together, they activate in the same
next-boot generation.

After reboot, inspect `/proc/cmdline`, check that `/sys/module/bluetooth` and
`/sys/module/btusb` are absent, and inspect `journalctl -k -b` for firmware
retry messages. To restore Bluetooth, remove these arguments and the service
mask, apply, and reboot again.

## Configure Native Linux Facilities

Use `hostConfiguration.fileSets` for file-based Linux and systemd configuration.
Katl validates ownership and carries the files in the node's generation; the
operator does not build or activate a confext.

```yaml
spec:
  defaults:
    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
      fileSets:
        forwarding:
          files:
            - path: /etc/sysctl.d/80-home-lab-forwarding.conf
              content: |
                net.ipv4.ip_forward = 1

        ups-device:
          files:
            - path: /etc/udev/rules.d/80-home-lab-ups.rules
              source: files/80-home-lab-ups.rules

        storage-modules:
          files:
            - path: /etc/modules-load.d/80-home-lab-storage.conf
              content: |
                br_netfilter
                vfio_pci

        containerd:
          files:
            - path: /etc/containerd/conf.d/80-home-lab.toml
              content: |
                version = 4

                [debug]
                  level = "warn"

        network-common:
          files:
            - path: /etc/systemd/network/20-bond0.network
              content: |
                [Match]
                Name=bond0

                [Network]
                DNS=172.53.53.53
                LinkLocalAddressing=no
                VLAN=bond0.20
                VLAN=bond0.40
```

`source` is relative to the ClusterConfig directory and is embedded when
`katlctl` builds the self-contained configuration bundle. Use `content` or
`source`, never both. Files default to mode `0644`; `0600` and `0640` are also
accepted.


For a collection of native files, include a directory instead of listing every
source and destination:

```yaml
spec:
  defaults:
    hostConfiguration:
      fileSets:
        network:
          directory: files/network
          destination: /etc/systemd/network
  nodes:
    - name: cp-1
      hostConfiguration:
        fileSets:
          addresses:
            directory: files/nodes/cp-1/network
            destination: /etc/systemd/network
```

Paths inside each directory are preserved below `destination`, including nested
drop-ins. For example, `files/network/20-bond0.network.d/50-route.conf` becomes
`/etc/systemd/network/20-bond0.network.d/50-route.conf`. Directory paths are
relative to the ClusterConfig directory. Katl recursively includes every regular
file; keep only configuration inputs in the included directory. Symbolic links
and special files are rejected, and existing destination restrictions still
apply. Included files use mode `0644` regardless of workstation permissions; use
explicit `files` entries when a file needs `0600` or `0640`.

Use either `directory` with `destination`, or `files` in one set. Katl expands
directories when validating and building the bundle; nodes receive the same
self-contained files as explicit entries. `katlctl config resolve` shows the
expanded configuration. Duplicate destination paths are errors, including
collisions between inherited and node-specific sets. A node set with the same
name replaces the entire default set; directories do not introduce overlay
precedence.

Removing a source file removes it from the set's desired configuration on the
next apply. An empty directory is an error: use `state: absent` without a
directory declaration to remove the complete set. Network files retain their
existing next-boot apply behavior.

Defaults and concrete nodes use the same named-set model. A node set replaces a
default set with the same name. To remove an inherited set on one node:

```yaml
spec:
  nodes:
    - name: worker-1
      hostConfiguration:
        fileSets:
          storage-modules:
            state: absent
```

Use a separate node set for host-specific networkd drop-ins so it composes with
the shared set:

```yaml
spec:
  nodes:
    - name: cp-1
      hostConfiguration:
        fileSets:
          network-address:
            files:
              - path: /etc/systemd/network/20-bond0.network.d/50-address.conf
                content: |
                  [Network]
                  Address=10.254.1.1/31

                  [Route]
                  Gateway=10.254.1.0
```

Katl accepts native `.network`, `.netdev`, and `.link` units plus one-level
`*.network.d/*.conf`, `*.netdev.d/*.conf`, and `*.link.d/*.conf` drop-ins below
`/etc/systemd/network`. Users select the unit and drop-in names but cannot move
network configuration outside that Katl-controlled directory. Any operator
`.network` unit replaces Katl's generated DHCP fallback; auxiliary `.link`,
`.netdev`, and drop-in files can compose with the fallback.

The fallback offers DHCP only to Ethernet links that have no virtual netdev
kind. Interfaces created later by a CNI, including veth pairs, Cilium host
devices, overlays, and CNI bridges, therefore remain unmanaged by networkd.
To make a host bridge, bond, VLAN, or tunnel part of the node's own network,
declare its native networkd units here; the operator units then replace the
fallback and own that topology explicitly.

Sysctl files with a reversible concrete-key change can apply live. Udev rules
can reload live, but Katl does not retrigger existing devices. Module load,
modprobe, typed sysfs settings, containerd overlays, and networkd files are
next-boot-only.
Katl renders `hostConfiguration.sysfs` to an internal tmpfiles rule, applies
each value, and reads it back before boot health succeeds. Containerd imports
`/etc/containerd/conf.d/*.toml` when it starts. Other permitted files are
next-boot unless their set declares a bounded notification for an unprotected
existing unit:

```yaml
onChange:
  systemd:
    - unit: systemd-journald.service
      action: try-reload-or-restart
```

The accepted actions are `reload`, `try-reload-or-restart`, and `try-restart`.
Katl rejects protected paths, duplicate path ownership, executable or writable
modes, and attempts to notify release-critical units before rendering a
candidate generation. Each sysfs `path` must be a unique normalized `/sys/...`
path, and each `value` must be a non-empty single-line value without leading or
trailing whitespace. A node-level `sysfs` list replaces the defaults list; use
`sysfs: []` to clear inherited settings. Operator-authored files below
`/etc/tmpfiles.d` are rejected because Katl owns the generated sysfs rule.

## Apply The Cluster

Apply the source configuration directly:

```sh
katlctl cluster apply --config ./cluster.yaml
```

Katl compiles and validates every selected node configuration,
and starts no mutation if any selected node rejects the plan. It then applies node
configuration and all affected Kubernetes component phases in a safe serial
order, checking the affected nodes' health.

If the source has already been compiled, pass the bundle through the same flag:

```sh
katlctl cluster apply --config ./katl-lab.katlcfg
```

Katl derives and verifies the bundle's integrity metadata from the file.

`katlctl` derives per-node generations, component phases, rollout ordering, and
operation identities internally. A successful return means the selected nodes'
supported configuration is active or staged with a reported reboot requirement;
unsupported plans fail with the node, field, and recovery action.

## Check Status

Use `katlctl node status cp-1 --config ./cluster.yaml` for the current healthy
generation. Use `katlctl operations list --config ./cluster.yaml --node cp-1`
when diagnosing an accepted or recently completed configuration operation.

On-node evidence remains available under:

```text
/var/lib/katl/generations/<generation>/
/var/lib/katl/operations/<operation-id>/
/var/lib/katl/boot/selection.json
```

If status reports rollback failure or `failed-needs-repair`, stop and follow the
reported recovery action before submitting another cluster apply.
