# Upgrade a KatlOS host

Use `katlctl node upgrade` to move one node to another KatlOS release or kernel
flavour. The command resolves a release, stages its root and UKI into the
inactive slot, reboots into a bounded trial, and waits for both boot health and
recovery of any existing Kubernetes role. It does not upgrade Kubernetes, drain
the node, or orchestrate availability across several nodes, so upgrade one
node at a time.

## Before you start

- Check that the node is running a
  [healthy generation](../concepts/generations.md#staged-trial-and-committed)
  and that no other mutating node operation is active.
- Choose the intended KatlOS release, and use that release's upgrade
  SquashFS if you supply a local image. The node must be able to fetch release
  artifacts from GitHub.
- Check that the installed CNI supports the target kernel. For Cilium, check
  the [tested version guidance](cilium.md#before-you-start).
- Check that `cluster.yaml` contains the node and its current management
  address, or supply an override with `--endpoint`. The node must also have been
  saved with `katlctl context save --config ./cluster.yaml`, because the
  upgrade checks the node's saved identity.
- Handle Kubernetes and workload availability outside Katl; see
  [Prepare a Kubernetes node for host maintenance](maintain-node.md). Host
  upgrade does not require a drain or uncordon a node that you left cordoned.
  For nodes that use Rook-Ceph volumes, follow the
  [Rook-Ceph guidance](maintain-node.md#rook-ceph) and verify the storage
  teardown required by your deployment.
- Keep independent etcd and workload backups and ensure that you have console
  or out-of-band access. Preserve the source release assets and CLI until you
  have accepted the target and no longer need its one-step rollback path.
- Run the upgrade during the intended reboot window.

Katl checks the image itself before it changes the inactive slot, including
its declared architecture and runtime interface, its boot components, and
whether it fits on writable state.

## Plan the host upgrade

Replace the version in this example with the target KatlOS release:

```sh
katlctl node upgrade cp-1 --config ./cluster.yaml --version 2026.11.0 --plan
```

A plan makes no durable change and does not reboot the node. The node
downloads or opens the image, records its SHA-256 and size in the operation,
and checks its boot components. On nodes that support target preparation, the
target release also prepares the complete candidate generation in an isolated
state view. A planning failure leaves the active root and boot selection
untouched.

The node rejects an upgrade before changing its inactive root slot if writable
state cannot hold the bounded image, preparation inputs, and transfer copies
while retaining 512 MiB for the running node. Free space under
`/var/lib/katl` or remove operator-owned data, then repeat the plan. Katl
removes completed and abandoned host-upgrade scratch automatically; do not
manually delete generation directories to make space because a retained
generation can own rollback assets that another generation references.

After a plan-only call, an uploaded local image remains available for up to
one hour so you can submit the reviewed plan without another upload. Katl
removes the other plan-time copies, mounts, and preparation snapshots after the
call, and keeps the operation record as durable success or failure evidence.

### Combine supported host configuration

Use `--apply-config` when a target release and a host configuration must become
active as one generation. For example, an extension selected in
`cluster.yaml` might first exist in the target release:

```sh
katlctl node upgrade cp-1 --config ./cluster.yaml \
  --version 2026.11.0 --apply-config --plan
```

The target release validates and prepares the configuration in isolation. The
source node does not interpret target extension metadata, and it does not apply
any subset of the configuration before reboot. Run the command without
`--plan` only after reviewing the target-prepared plan.

Combined upgrades support generation retention, SSH access, system extensions,
native host settings, module loading, temporary-file rules, resolver settings,
and API proxy settings. They do not support kernel command-line changes,
Kubernetes or kubeadm changes, node role or membership transitions, volume or
mount changes, destructive storage changes, or arbitrary unsafe `/etc` files.
If the plan rejects one of these domains, upgrade without `--apply-config`,
then use the dedicated configuration, Kubernetes, or storage workflow. A
rejected plan leaves the inactive root, EFI selection, and current generation
unchanged.

## Upgrade the host

Run the reviewed command without `--plan`:

```sh
katlctl node upgrade cp-1 --config ./cluster.yaml --version 2026.11.0
```

For repeated day-two commands, `--context NAME` can select the saved context
instead of `--config`. The context only shortens commands: `cluster.yaml`
remains the desired state, and `--apply-config` reads it from `--config`.

`katlctl` follows staging progress, asks the `katlc` management agent to reboot
the node, waits for the agent to restart, and requires the selected generation
to be committed, booted, and healthy. On a bootstrapped node it also waits
for kubelet, Node Ready, local control-plane components where applicable,
and the managed API and route exchange paths. The default result is concise
text; use `--output json` when automation needs the structured `rebooted`,
`bootHealth`, and `kubernetes` fields. These fields report independently
observed outcomes. For example, if KatlOS becomes healthy but Kubernetes does
not recover before the timeout, the command fails overall while reporting
`bootHealth` as `healthy` and `kubernetes` as its last observed waiting
state. An unreachable node reports `unknown` health instead of treating an
unobserved boot as a failed boot. Check workload availability before upgrading
another node.

During the reboot, the console may show a containerd stop-job
countdown after the containerd daemon has exited. Containerd deliberately keeps
its shim processes across ordinary daemon restarts; a full host shutdown lets
systemd finish the remaining workload and shim cleanup. Allow that shutdown to
complete rather than forcing power off, which can increase the risk of
workload data loss. If it repeatedly reaches the systemd timeout, preserve the
previous-boot journal before retrying the upgrade.

## Understand rollback retention

KatlOS keeps two root slots, so after an upgrade only the previous runtime
remains bootable as a rollback target. The generation list can retain more
metadata and configuration history, but staging the next host upgrade
overwrites the inactive slot and makes any older generation that used it
unavailable for boot. A host rollback changes the selected root, UKI, and system
and configuration extensions; it does not restore Kubernetes, etcd, or workload
data, and it cannot recover a damaged `/var`, ESP, firmware state, or disk. See
[retention](../concepts/generations.md#retention) and
[rollback is not a backup](../concepts/generations.md#rollback-is-not-a-backup).

## Failure boundary

Automatic fallback is available only once activation confirms that the node is
booting the pending trial and that a healthy previous generation remains to fall
back to. The source generation stays the persistent EFI default throughout the
trial. See [boot health and fallback](../concepts/generations.md#boot-health-and-fallback)
for the lifecycle.

| Failure | What Katl does | What you do |
| --- | --- | --- |
| An armed trial loses management networking, fails required activation, or misses the 10-minute boot deadline | Selects the previous healthy generation and performs one automatic reboot. `katlctl` reports that the candidate was rejected after the node reconnects on the source generation. | Read the previous-boot journal to find why boot health failed before retrying. |
| No healthy fallback exists, required state or EFI storage is unavailable, or the failure occurs before activation arms boot recovery | Does not reboot automatically. | Use the console or out-of-band management to preserve the failed boot's journal and inspect boot-selection state. |
| The fallback also fails | Stops instead of boot-looping, because the fallback does not re-arm the switch. | Preserve the journal and inspect boot-selection state from the console. |
| The operation record says `recoveryRequired: true`, or the node fails to return | Stops. | Stop the rollout and collect the evidence in [Troubleshoot KatlOS](troubleshoot.md). |
| KatlOS returns but Kubernetes does not recover before the timeout | Fails the command while reporting `bootHealth` as `healthy`. | Do not schedule workloads on that node. `katlctl node status` reports whether kubelet, Node Ready, local control-plane components, or managed routing is still waiting. |

Host rollback does not undo Kubernetes, etcd, workload, or
external-infrastructure changes.

## Kernel flavours

KatlOS has two kernel tracks, released together with the same KatlOS version:

- **standard** uses Fedora's current stable kernel packages.
- **lts** uses the maintained [kwizart 6.18 LTS kernel RPMs](https://copr.fedorainfracloud.org/coprs/kwizart/kernel-longterm-6.18/) for the same Fedora release.

LTS refers to the kernel series, not extended support for Fedora userspace.
Kernel versions enter KatlOS builds through reviewed, retained RPM selections;
changing the LTS series is a deliberate Katl build-policy change. Nodes only
change kernels when you upgrade KatlOS, not through background RPM updates.

Omitting `--flavour` selects standard, including on an LTS node. To select a track,
including at the same KatlOS version:

```sh
katlctl node upgrade cp-1 --config cluster.yaml --version VERSION --flavour lts
katlctl node upgrade cp-1 --config cluster.yaml --version VERSION --flavour standard
```

Changing flavour stages a complete OS image and reboots. Repeating an upgrade
to the identical active image without `--apply-config` returns unchanged
without rebooting. The Kubernetes bundle is independent of the kernel
flavour. `--artifact` must match the selected flavour. Pass `--flavour lts` for
a local LTS image; omitting the flag selects standard.

Nodes running a release from before flavour support must first upgrade to the
standard image of a release with flavour support, then switch to LTS. The CLI
reports this before uploading or staging an LTS image.

For a fresh LTS installation, use the `katl-installer-lts` boot assets and
`katlos-lts-install-VERSION-x86_64.squashfs` from the same release. The LTS ISO
includes the matching install image. Standard assets retain their existing
names. LTS upgrade images are named
`katlos-lts-upgrade-VERSION-x86_64.squashfs`; `katlctl` chooses that name for you.
