# Apply cluster configuration

Use `katlctl cluster apply` for supported configuration changes after
installation. The same `ClusterConfig` remains the desired state for every node
and for kubeadm-owned Kubernetes configuration, so edit `cluster.yaml` and apply
it rather than changing nodes directly. Field semantics live in the
[ClusterConfig reference](../reference/cluster-config.md).

Before you start, keep the complete `cluster.yaml` from installation and make
sure the selected nodes are installed and reachable through the management API.
Apply never joins nodes or changes cluster membership: use
`katlctl cluster bootstrap` for a new cluster and the
[node lifecycle matrix](#node-lifecycle-matrix) for membership changes.

> **Caution:** Selection limits node operations, not the scope of shared
> Kubernetes resources. Applying shared kubeadm or kube-proxy configuration
> through a selected control plane can affect the whole cluster. Apply shared
> kubelet changes to a control plane before workers so the shared configuration
> is available for them to use. Use per-node kubelet configuration for settings
> that should affect only one node.

## Supported input

Apply supports these inputs:

- SSH authorized keys.
- Operator-owned kernel command-line additions.
- Native host configuration file sets, including systemd-networkd files and
  drop-ins.
- Desired data disks under `storage.volumes`.
- Node-local native kubelet configuration inherited from
  `spec.defaults.kubernetes.kubelet` or overridden under
  `nodes[].kubernetes.kubelet`.

Apply also reads each node's system role and role-dependent Kubernetes
bootstrap state to validate the plan, but it refuses a role change; see the
[node lifecycle matrix](#node-lifecycle-matrix). Runtime-safe fields apply
normally, and Katl coordinates affected node generations and kubeadm phases
internally. System-disk installation selection and Kubernetes version changes
use the dedicated install and Kubernetes upgrade workflows; data disks remain
desired node storage.

## Node lifecycle matrix

Use this table to choose the workflow for a change to `spec.nodes`. The list
names the nodes to install and the nodes that `katlctl cluster apply` targets.
Removing a node's entry only stops Katl targeting it: Katl never drains its
Kubernetes Node, removes its etcd member, powers it off, changes its partitions,
formats its storage, or wipes KatlOS because you deleted the entry.

| Desired change | `cluster apply` behavior | Supported operator path |
| --- | --- | --- |
| Reorder nodes or change ordinary supported fields | Applies by stable node name | Run `katlctl cluster apply --config ./cluster.yaml` |
| Add one installed, unenrolled node | Does not join it | Install the new named node from the config, then run `katlctl node join NAME --config ./cluster.yaml` |
| Replace hardware while keeping the node name and role | Does not join or erase a node | While the old node is still listed, run `katlctl node wipe NAME --config ./cluster.yaml --plan`, execute the reviewed wipe with the required kubeconfig, reinstall the replacement, then run `katlctl node join NAME --config ./cluster.yaml` |
| Remove a node from the cluster | Omission only stops Katl targeting; the old node and its data are preserved | Keep the node listed while planning and executing `katlctl node wipe NAME --config ./cluster.yaml --plan`; remove the entry only after the explicit Kubernetes/etcd-aware wipe succeeds |
| Rename an unenrolled installed node | Stages the hostname through normal next-boot configuration | Apply, reboot, verify the new hostname, and only then bootstrap Kubernetes |
| Rename an enrolled node | Refused before any node mutation | Keep the old name, or explicitly wipe it under the old config and reinstall it as a new node |
| Change `controlPlane` / node role | Refused as an operation-only change | Explicitly wipe the named node, reinstall it with the desired role, then use `katlctl node join NAME --config ./cluster.yaml` |
| Change only `management.address` | Changes only where `katlctl` connects; it does not change the node's IP address, which [networkd host configuration](../reference/cluster-config.md#networkd-files) owns | Verify the new address reaches the same node; no node generation or Kubernetes state changes |
| Change `kubernetes.address` | Refused as an operation-only change | Wipe the node, reinstall it, then run `katlctl node join NAME --config ./cluster.yaml`; see [Change a node's address](change-cluster-nodes.md#change-a-nodes-address) |

Removing one entry and adding another is never inferred to be a rename. It is a
preserved omitted node plus a distinct addition until the operator completes
the explicit removal/reinstall workflow. If an entry was removed too early,
put it back with its old name and address, inspect its current status, and run
the appropriate wipe plan. Do not reinstall merely to make the config match;
preserve and diagnose the existing node first.

The wipe workflow is intentionally separate because it names the affected
node, proves Kubernetes and stacked-etcd removal where required, reports what
disk state is preserved, and stops before installer formatting. See
[Add, replace, or remove cluster nodes](change-cluster-nodes.md) for each
membership workflow, including [the wipe plan](change-cluster-nodes.md#wipe-one-node).

## Inspect effective configuration

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

## Apply the cluster

1. Preview the change. Planning validates selected host changes without
   accepting operations; Kubernetes component readiness is checked on apply.

   ```sh
   katlctl cluster apply --config ./cluster.yaml --plan
   ```

2. Apply to every node, or select nodes by name:

   ```sh
   katlctl cluster apply --config ./cluster.yaml
   katlctl cluster apply --config ./cluster.yaml --node cp-1
   katlctl cluster apply --config ./cluster.yaml --node cp-1 --node worker-1
   ```

3. Follow any reported reboot guidance. Host changes that need a reboot are
   staged for the next boot and reported, and apply skips the Kubernetes
   component phases while any selected node is staged. Reboot the staged nodes
   with `katlctl node reboot`, then rerun the same apply.

Keep the complete ClusterConfig when using `--node`. Katl validates and applies
configuration only on selected nodes and does not require other nodes to be
reachable. Unknown node names are rejected, and repeating the same name does not
apply it twice. Configuration rollouts use a selected control plane to publish
shared configuration, so an explicit `--coordinator` must be among the selected
nodes.

An apply that mixes fresh and joined nodes stops before mutation with join
guidance. Use `--node` to configure a fresh node separately, then join it with
`katlctl node join NODE --config ./cluster.yaml`.

Unchanged configuration is a no-op. Use `--mode live` to refuse
reboot-requiring changes, or `--mode next-boot` to stage host changes
explicitly. `--timeout` bounds the complete operation and `--output json` emits
a structured report separately from progress. Routine apply and planning never
save or select a workstation context.

Katl compiles and validates every selected node configuration, and starts no
mutation if any selected node rejects the plan. It then applies node
configuration and the configured Kubernetes component phases in a safe serial
order, checking the affected nodes' health. An unchanged host configuration
reuses its generation. Kubernetes reconciliation still runs to finish an
interrupted or failed apply; matching, healthy components do not need a
restart.

If the configuration has already been compiled, pass the configuration bundle
through the same flag:

```sh
katlctl cluster apply --config ./katl-lab.katlcfg
```

Katl derives and verifies the bundle's integrity metadata from the file.

`katlctl` derives per-node generations, component phases, rollout ordering, and
operation identities internally. A successful return means the selected nodes'
supported configuration is active or staged with a reported reboot requirement;
unsupported plans fail with the node, field, and recovery action.

## Check status

Use `katlctl node status cp-1 --config ./cluster.yaml` for the current healthy
generation. Use `katlctl operations list --config ./cluster.yaml --node cp-1`
when diagnosing an accepted or recently completed configuration operation.

On-node evidence remains available under:

```text
/var/lib/katl/generations/<generation>/
/var/lib/katl/operations/<operation-id>/
/var/lib/katl/boot/selection.json
```

If a Kubernetes phase fails after host configuration succeeds, apply reports a
partial result and retains the active host configuration. Follow the reported
recovery action, then rerun the same command. You do not need to delete the
generation or edit an unrelated field to force a retry. Host generation rollback
does not undo Kubernetes API changes.

If status reports rollback failure or `failed-needs-repair`, stop and follow the
reported recovery action before submitting another cluster apply.

## Change Kubernetes configuration

If `spec.kubernetes.kubeadm` changes, cluster apply validates every selected
node before mutation and then reconciles every affected Kubernetes component
online. A Kubernetes configuration change never falls back to next-boot
application or requires a host reboot itself. If the same apply also stages host
changes for the next boot on a selected node, Katl skips the Kubernetes phases
until you reboot the staged nodes and rerun apply.

Resolved `kubernetes.kubelet.configFile` changes use kubeadm's node-local patch
path. Set a shared reference under `spec.defaults` and override it on individual
nodes when needed. Katl validates and stages the native KubeletConfiguration,
refreshes only the affected node's `/var/lib/kubelet/config.yaml`, restarts its
kubelet, and checks node health. It does not upload the overlay to the shared
kubelet ConfigMap. Removing the resolved input refreshes that node from the
shared kubeadm configuration. Use `config resolve` to see the selected native
input, its default or node provenance, and the owned patch path. Use
`config diff` to review its `kubeadm-aware operation` classification before
applying.

Native kubelet patches selected through `spec.kubernetes.kubeadm.patchesDir`
also apply during online refresh. Kubeadm applies those patches in native
directory and filename order, followed by the fields explicitly set in the
node-local kubelet overlay. Fields omitted from that overlay retain the
patched shared value. Removing a patch or overlay refreshes the node from the
remaining inputs.

The shared `KubeletConfiguration` stays separate from both patch layers. A
coordinator uploads changes to that shared base even when it also has a
node-local overlay. Katl checks the complete native result before restarting
kubelet and checks health afterward; an unchanged healthy result does not
restart kubelet.

You can apply kubelet and control-plane configuration after bootstrap and
before installing your CNI when the node is otherwise healthy and reports
only an uninitialized CNI. Apply checks the affected Kubernetes components and
permits that existing network condition. The node remains `NotReady` until you
[install your CNI](bootstrap-kubernetes.md#establish-cluster-networking).
A node that was Ready must regain Ready after the configuration change.

To change the memory reserved for the host, set `systemReserved.memory` in the
native `KubeletConfiguration` at the scope you need, as described in
[Memory reserved for the host](../reference/cluster-config.md#memory-reserved-for-the-host).
After applying, inspect `/var/lib/kubelet/config.yaml` on the node and check its
`status.allocatable.memory` through `kubectl get node NODE -o yaml`. The
allocatable value excludes the reservation and kubelet's eviction threshold.

## Destructive storage changes

A volume with `wipe: true` is formatted without a further prompt; see
[Wipe authorization](../reference/cluster-config.md#wipe-authorization) before
applying a storage change. To replace the device behind a bound volume, change
its selector to the replacement's exact identity, plan, and pass the reported
`--rebind-volume NODE/VOLUME` authority as described in
[Replace a bound volume](../reference/cluster-config.md#replace-a-bound-volume).

`katlctl node status NODE` reports the active exact mount source so the
operator can verify the retained identity before and after the change without
exposing the generation's internal binding metadata as a separate API.

## Configure kernel arguments

Set `kernel.commandLine` under defaults or a concrete node:

```yaml
spec:
  defaults:
    kernel:
      commandLine:
        - intel_iommu=on
        - iommu=pt
```

Apply, then reboot the affected nodes when `katlctl cluster apply` reports that
a reboot is required; the new arguments become active after that reboot. To
remove all operator additions for one node, set:

```yaml
spec:
  nodes:
    - name: worker-1
      kernel:
        commandLine: []
```

See [Kernel arguments](../reference/cluster-config.md#kernel-arguments) for the
arguments Katl owns and rejects.

## Enable and configure systemd units

Declare services, timers, sockets, or instantiated template units in
`hostConfiguration.enabledUnits`. For example, enable the shipped time service
and provide its native configuration:

```yaml
spec:
  defaults:
    hostConfiguration:
      enabledUnits:
        - systemd-timesyncd.service
      fileSets:
        time:
          files:
            - path: /etc/systemd/timesyncd.conf.d/80-site.conf
              content: |
                [Time]
                NTP=192.0.2.123
```

`katlctl cluster apply --config ./cluster.yaml` enables and starts newly listed
units without a reboot. Listing order does not determine startup ordering, and
removing a unit stops it without preventing other activation. See
[Enabled units](../reference/cluster-config.md#enabled-units) for inheritance,
removal, unit-file changes, and live-apply rollback.

## Keep unwanted services stopped

Use `hostConfiguration.maskedUnits` to stop unwanted services and prevent
systemd from starting them again:

```yaml
spec:
  defaults:
    hostConfiguration:
      maskedUnits:
        - bluetooth.service
```

Apply with `katlctl cluster apply --config cluster.yaml`. Mask-only changes
apply live without rebooting. See
[Masked units](../reference/cluster-config.md#masked-units) for inheritance,
protected units, and triggering units.

Verify through the node's SSH interface:

```console
systemctl show bluetooth.service --property=LoadState,ActiveState
```

The masked unit should report `LoadState=masked` and `ActiveState=inactive`.

## Disable Bluetooth drivers

Masking `bluetooth.service` stops only the userspace service; kernel drivers can
still load and retry missing firmware. On a node that does not use Bluetooth,
also add these native kernel arguments to its existing `kernel.commandLine`
list, keeping any other operator arguments:

```yaml
spec:
  defaults:
    kernel:
      commandLine:
        - module_blacklist=bluetooth,btusb,btmtk
        - modprobe.blacklist=bluetooth,btusb,btmtk
```

`modprobe.blacklist` suppresses automatic loading, and `module_blacklist` also
prevents explicit loading from early boot onward; Wi-Fi drivers on combined
Wi-Fi/Bluetooth hardware stay available. Like other
[kernel arguments](#configure-kernel-arguments), they take effect only after you
apply and reboot the node, and a service mask changed in the same apply
activates in the same next-boot generation. After the reboot, check that
`/sys/module/bluetooth` and `/sys/module/btusb` are absent. To restore
Bluetooth, remove the arguments and the mask, apply, and reboot.

## Configure native Linux facilities

Use `hostConfiguration.fileSets` for file-based Linux and systemd configuration
and `hostConfiguration.sysfs` for typed sysfs values. Katl carries the files in
the node's generation; the operator does not build or activate a confext.
Whether a file applies live or at the next boot depends on its type; see
[Apply behavior by file type](../reference/cluster-config.md#apply-behavior-by-file-type).

> **Caution:** Network files can apply live. Planning and apply warn that
> connectivity can be interrupted and running interfaces might differ from the
> saved configuration. A failed apply can restore the previous saved generation
> while leaving runtime network changes in place; follow the operation's
> recovery guidance to inspect the network and recreate affected interfaces or
> reboot. Some settings require recreating an interface; removing a `.netdev`
> file does not remove the existing device, and existing interfaces are not
> retriggered for `.link` changes. Use `--mode next-boot` when you prefer to
> defer network changes until a reboot.

```yaml
spec:
  defaults:
    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
        - path: /sys/devices/system/cpu/cpu*/cpufreq/energy_performance_preference
          value: balance_performance
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

Use a separate node set for node-specific networkd drop-ins so it composes with
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

Any operator `.network` unit replaces Katl's generated DHCP fallback. Inspect
the network after applying. See the reference for
[file sets](../reference/cluster-config.md#file-sets),
[directory sets](../reference/cluster-config.md#directory-sets),
[networkd files and boot health](../reference/cluster-config.md#networkd-files),
[`onChange` notifications](../reference/cluster-config.md#apply-behavior-by-file-type),
and [sysfs settings](../reference/cluster-config.md#sysfs-settings).

## Boot generations and retention

Host configuration changes activate through a node generation; see
[Generations](../concepts/generations.md) for the two-slot model, boot health,
and why selecting an older generation does not restore Kubernetes, etcd, or
workload data. Inspect and manage generations through the management API:

```sh
katlctl node generations list cp-1 --config cluster.yaml
katlctl node generations select GENERATION cp-1 --config cluster.yaml
katlctl node generations select GENERATION cp-1 --one-shot --config cluster.yaml
katlctl node reboot cp-1 --config cluster.yaml
katlctl node generations remove GENERATION cp-1 --config cluster.yaml
```

Selection does not reboot the node. By default it changes the persistent boot
default. `--one-shot` preserves that default, including after boot health
passes; the following reboot returns to it. Selection accepts available healthy
generations. Listing shows each
generation's OS version, slot, availability, and protection reasons, and all
commands support `--output json`. Removing an already absent generation
succeeds. Removal refuses running, selected, rollback, and referenced
generations; select another default or finish the pending boot first where
applicable.

Automatic cleanup follows `generationRetention`; see
[Generation retention](../reference/cluster-config.md#generation-retention).
