# Upgrade a KatlOS host

KatlOS host upgrades are one-node-at-a-time operations. The normal command
resolves a release, stages its root and UKI into the inactive slot, reboots into
a bounded trial, and waits for both boot health and recovery of any existing
Kubernetes role. It does not upgrade Kubernetes, drain the node, or orchestrate
availability across several hosts.

## Preconditions

- the node is healthy on a known-good generation;
- no other mutating node operation is active;
- the selected upgrade SquashFS is from the intended KatlOS release;
- the upgrade declares a compatible architecture and runtime interface;
- the node can fetch release artifacts from GitHub;
- the installation `ClusterConfig` contains the node and its current management
  address, or `--endpoint` supplies an override;
- the command is run during the intended reboot window; and
- Kubernetes and workload availability have been handled outside Katl.

## Plan the host upgrade

Replace the version in this example with the target KatlOS release:

```sh
katlctl node upgrade cp-1 --config ./cluster.yaml --version 2026.9.0-beta.18 --plan
```

A plan response has no durable mutation and does not reboot the node.

During staging, the node downloads or opens the image, calculates its SHA-256
and size, records that resolved identity in the operation, and checks the image's
boot components before changing the inactive slot. On nodes that support target
preparation, the target release also prepares the complete candidate generation
in an isolated state view. A planning failure leaves the active root and boot
selection untouched.

## Upgrade the host

Run the reviewed command without `--plan`:

```sh
katlctl node upgrade cp-1 --config ./cluster.yaml --version 2026.9.0-beta.18
```

For repeated day-two commands, `katlctl context save --config ./cluster.yaml`
can save this topology locally. That context is optional; it is not a second
cluster configuration operators must maintain.

`katlctl` follows staging progress, asks the node agent to reboot, waits for the
agent to restart, and requires the selected generation to be committed, booted,
and healthy. On a bootstrapped node it also waits for kubelet, Node Ready, local
control-plane components where applicable, and the managed API and route
exchange paths. The default result is concise text; use `--output json` when
automation needs the structured `rebooted`, `bootHealth`, and `kubernetes`
fields. Check workload availability before upgrading another host.

During the reboot, the console may show a containerd stop-job
countdown after the containerd daemon has exited. Containerd deliberately keeps
its shim processes across ordinary daemon restarts; a full host shutdown lets
systemd finish the remaining workload and shim cleanup. Allow that shutdown to
complete rather than forcing power off, which can increase the risk of
workload data loss. If it repeatedly reaches the systemd timeout, preserve the
previous-boot journal before retrying the upgrade.

## Failure boundary

Boot health may select the previous known-good host generation. A failed trial
keeps the source generation as the persistent EFI default. If the target loses
management networking, preserve its console evidence, then reboot it from the
console or out-of-band management to return to that source. `katlctl` reports
the failure when it can reconnect. Host rollback does not
undo Kubernetes, etcd, workload, or external-infrastructure changes. If the
operation record says `recoveryRequired: true`, or the node fails to return,
stop the rollout and collect the evidence in [Troubleshoot KatlOS](troubleshoot.md).
If KatlOS returns but Kubernetes does not recover before the timeout, do not
schedule workloads on that node. `katlctl node status` reports whether kubelet,
Node Ready, local control-plane components, or managed routing is still waiting.

## Move a beta.14 node across the image metadata change

The beta.14 agent rejects the `extensionRelease` field in newer upgrade
artifact metadata. Use the beta.14 `katlctl` binary and a local copy of the
intended target image for this one-time bridge. Preserve the image bytes and
their checksum; remove only that field from the adjacent `.json` file. First
verify the target image and beta.14 CLI against their release checksums.

```sh
image=./katlos-upgrade-TARGET-x86_64.squashfs
bridge=./beta14-bridge
mkdir -p "$bridge"
cp "$image" "$bridge/$(basename "$image")"
jq 'del(.extensionRelease)' "$image.json" > "$bridge/$(basename "$image").json"
sha256sum "$bridge/$(basename "$image")"
./katlctl-2026.9.0-beta.14-linux-amd64 node upgrade cp-1 \
  --config ./cluster.yaml --artifact "$bridge/$(basename "$image")" --plan
./katlctl-2026.9.0-beta.14-linux-amd64 node upgrade cp-1 \
  --config ./cluster.yaml --artifact "$bridge/$(basename "$image")"
```

Compare the printed SHA-256 with the published image checksum before running
either upgrade command. The bridge image must be a regular file because the
beta.14 client does not accept a symlink as `--artifact`. After the upgrade,
use the target release's `katlctl` and confirm `katlctl node status cp-1 --config
./cluster.yaml` reports a healthy node. This procedure is limited to the
beta.14 metadata incompatibility; do not use it to bypass a failed target
preparation or compatibility check. Keep the beta.14 CLI for this bridge only.

## Kernel flavours

KatlOS has two kernel tracks, released together with the same KatlOS version:

- **standard** uses Fedora's current stable kernel packages.
- **lts** uses the maintained [kwizart 6.18 LTS kernel RPMs](https://copr.fedorainfracloud.org/coprs/kwizart/kernel-longterm-6.18/) for the same Fedora release.

LTS refers to the kernel series, not extended support for Fedora userspace.
Patch updates within 6.18 enter new KatlOS builds through the package repository;
changing the LTS series is a deliberate Katl build-policy change. Nodes only
change kernels when you upgrade KatlOS, not through background RPM updates.

Omitting `--flavour` selects standard, including on an LTS node. To select a track,
including at the same KatlOS version:

```sh
katlctl node upgrade cp-1 --config cluster.yaml --version VERSION --flavour lts
katlctl node upgrade cp-1 --config cluster.yaml --version VERSION --flavour standard
```

Both commands stage a complete OS image and reboot. Kubernetes extensions remain
independent of the kernel flavour. `--artifact` must match the selected flavour.
Pass `--flavour lts` for a local LTS image; omitting the flag selects standard.

Nodes running a release from before flavour support must first upgrade to the
standard image of a release with flavour support, then switch to LTS. The CLI
reports this before uploading or staging an LTS image.

For a fresh LTS installation, use the `katl-installer-lts` boot assets and
`katlos-lts-install-VERSION-x86_64.squashfs` from the same release. The LTS ISO
includes the matching install image. Standard assets retain their existing
names. LTS upgrade images are named
`katlos-lts-upgrade-VERSION-x86_64.squashfs`; `katlctl` chooses that name for you.
