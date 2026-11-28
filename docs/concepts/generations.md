# Generations and boot health

A *generation* is a complete, versioned description of what one KatlOS node
boots and runs. Katl changes a node's host state by creating a new generation
rather than editing the running system in place, and it uses boot health to
decide whether a new generation may replace the previous one. This page
explains what a generation holds, which operations create one, and what host
rollback can and cannot restore.

## What a generation contains

Each generation combines three parts that Katl selects and rolls back as one
unit:

- the KatlOS release, as a runtime root in one of two root slots and its
  matching UKI;
- the selected system extensions, including the
  [Kubernetes bundle](kubernetes-releases.md), delivered as a system extension,
  once the node runs Kubernetes;
- the compiled host configuration, rendered from `ClusterConfig` as a
  configuration extension.

Rollback never switches only the root, only the extensions, or only the
configuration; it selects a complete previous generation.

The installer writes **generation 0**, the post-install KatlOS baseline.
Generation 0 has a working host, network, SSH, and the `katlc` management
agent, but no Kubernetes bundle, kubeadm state, or etcd data. Later generations
are created by these operations:

| Operation | New generation |
| --- | --- |
| `katlctl cluster bootstrap` and `katlctl node join` | The first Kubernetes-capable generation, committed only after kubeadm and the operation's health checks succeed. |
| `katlctl cluster apply` | One generation for each accepted host configuration change. Unchanged configuration is a no-op. |
| `katlctl node upgrade` | A generation with the target release staged into the inactive root slot; `--apply-config` adds host configuration to the same generation. |
| `katlctl kubernetes upgrade` | A generation with the target Kubernetes bundle, activated without a host reboot. |

## Generation IDs

Each generation has an ID, which `katlctl node generations list` prints in its
`GENERATION` column and which commands accept wherever they show the
`GENERATION` placeholder. Generation 0 has the ID `0`. Later generations get a
longer ID derived from the operation that created them, so copy the ID from the
list rather than counting. Current
[boot menu entries](../operations/wipe-reinstall.md#boot-menu-entries) are
labeled by release and creation time; the ID appears in their entry filenames
and kernel arguments.

## Staged, trial, and committed

A new generation reaches the node in one of two ways, and the way determines
when it becomes the persistent EFI boot default.

A generation that needs a reboot is *staged*. Katl writes it and selects it as
a systemd-boot one-shot entry for the next boot only, while the current healthy
generation stays the persistent EFI default and the node keeps running it.
`katlctl node status` reports the staged next boot, and `katlctl node reboot`
boots into it. Host upgrades and `cluster apply` changes that need a reboot
take this path. That one boot is the *trial*: when it passes boot health, Katl
records the generation as healthy and makes it the persistent default; when it
fails, the default still names the previous healthy generation, so the trial
is attempted once rather than repeatedly.

A change that can apply online activates in the current boot instead. This
covers `cluster apply` changes that apply live, `cluster bootstrap`,
`node join`, and `kubernetes upgrade`. Only after the operation's live checks
pass (the domain actions of a configuration change, or kubeadm and its health
checks for bootstrap, join, and Kubernetes upgrade) does Katl record the new
generation as healthy and make it the persistent default, with no reboot or
trial. The previous generation remains available as a rollback target. Because
the new generation is already healthy, its later boots are ordinary boots
rather than trials: its required services must still start on every boot, but
a failure there does not trigger automatic fallback. Select the previous
generation from the boot menu or console, or with
`katlctl node generations select` while the management API is reachable.

A generation is *committed* when the responsible install, apply, bootstrap,
join, or upgrade operation has accepted it as the node's desired host state.
Commitment does not by itself make a generation the default or a rollback
target; a staged `cluster apply` change, for example, is committed before its
trial. Only a generation that has passed boot health on its trial boot, or the
live checks of an online change, is a *healthy generation*.

## Boot health and fallback

Boot health passes when the node reaches `katl-boot-complete.target`, which is
ordered after the generation's required services. For every generation those
include writable state under `/var/lib/katl`, the EFI System Partition at
`/efi`, system extension and volume activation, the `/etc/kubernetes` mount,
`systemd-networkd`, `sshd`, the `katlc` management agent, and a consistent
machine identity; selected system extensions can add their own required units.
A generation that has not yet passed boot health must also bring its required
networkd links online, with at least one routable link, so that the node stays
reachable for management.

Boot health runs on every boot, and the management agent answers before it
finishes. While `katl-boot-health.service` is queued or running,
`katlctl node status` reports health `pending`, and the agent holds node
operations such as upgrades, configuration applies, and generation selection
until it finishes. A held request is refused with a retry message after five
minutes or shortly before its own timeout. Status, reboot, and shutdown requests
are not held. If a unit that boot health requires fails to start, boot health
does not run, status reports health `failed`, and node operations are
available for repair. After the hold ends, you can use `katlctl cluster apply`
and `katlctl node reboot` to repair a stalled or failed boot. If the agent
cannot read current-boot health from systemd, status reports `unknown`.

When a boot trial loses management networking, fails required activation, or
misses the 10-minute boot deadline, Katl rejects the candidate, keeps the
previous healthy generation as the default, and performs **one** automatic
reboot into it. A boot deadline runs only while a boot trial is undecided.
`katl-boot-recovery.service` or `katl-boot-deadman.service` requests that
reboot, and `katlctl` reports that the candidate was rejected after the node
reconnects on the source generation. The rejected generation stays listed as
failed and unhealthy, and its journal remains in the previous boot.

Every other failed boot reports the failure and changes nothing. That covers the
first boot after install, a reboot of the default generation, a generation
booted from the boot menu, and a manual selection. Boot health is `failed`, the
default and next boot stay as they were, and the node does not reboot again.
Fix the cause, then run `katlctl node reboot`.

Katl also does not reboot automatically when no healthy fallback exists, the
fallback's state cannot be checked, or required state or EFI storage is
unavailable. When a failed trial has no usable fallback, the failed generation
stays the default and the node reports that recovery is required; node
operations such as apply are refused until a healthy boot of that generation
clears the state. The fallback is not a trial, so a failed fallback stops
instead of boot-looping. In these cases use the console or out-of-band
management, preserve the failed boot's journal, and follow
[Troubleshoot KatlOS](../operations/troubleshoot.md).

## Retention

KatlOS keeps two root slots: one holds the active release and the other the
previous release. Configuration generations share the release image in their
slot, so the generation list can hold more history than two entries, but it
never holds more than two bootable KatlOS releases. Staging the next host
upgrade overwrites the inactive slot and makes every older generation that used
that slot unavailable for boot. Katl requires confirmed health for the running
boot before it stages a host upgrade or automatically prunes generations.

`katlc` also removes old generation records automatically according to
`generationRetention`. For each installed KatlOS release it keeps the newest
`keepLast` generations and every generation younger than `maxAge` (defaults:
`5` and `30d`). Running, selected, rollback, and referenced generations are
protected regardless of age or count, and the newest available healthy
generation in each slot is retained as a rollback option. The
[`generationRetention` reference](../reference/cluster-config.md#generation-retention)
has the field rules, and
[boot generations and retention](../operations/configure-nodes.md#boot-generations-and-retention)
covers listing, selecting, and removing generations:

```sh
katlctl node generations list cp-1 --config cluster.yaml
katlctl node generations select GENERATION cp-1 --config cluster.yaml
```

Selection does not reboot the node and accepts only available healthy
generations. After a Kubernetes upgrade crosses its control-plane mutation
boundary, Katl refuses to select a generation with an older Kubernetes bundle.

## Rollback is not a backup

Host rollback restores the selected root, UKI, system extensions, and compiled
host configuration. Everything else lives on persistent state shared by every
generation: the writable `/var` with machine and SSH identity, containerd,
kubelet, kubeadm, etcd, persistent-volume, and workload data, plus the EFI
System Partition and firmware boot state.

Selecting or falling back to an earlier generation therefore does not undo
kubeadm commands, etcd membership or data, Kubernetes API objects, CNI state,
volumes, or external infrastructure, and it cannot recover a damaged or full
`/var`, a lost EFI System Partition, invalid firmware state, or disk failure.
Once kubeadm has created PKI, kubelet state, static Pod manifests, or etcd data,
returning to generation 0 is not a clean cluster reset; use the
[wipe and reinstall](../operations/wipe-reinstall.md) workflow instead.

Keep independent etcd, workload, and data backups. When an operation reports
`recoveryRequired: true`, stop and inspect the node rather than assuming that a
host rollback restored the cluster. The
[Kubernetes identity](../operations/kubernetes-identity.md) preserves kubeadm CA
and signing keys across whole-cluster reprovisioning; it does not preserve etcd
contents or Kubernetes resources.
