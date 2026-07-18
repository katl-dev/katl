# Katl

[![Fast Checks](https://github.com/katl-dev/katl/actions/workflows/fast-checks.yml/badge.svg)](https://github.com/katl-dev/katl/actions/workflows/fast-checks.yml)
[![Release](https://img.shields.io/github/v/release/katl-dev/katl?include_prereleases&sort=date)](https://github.com/katl-dev/katl/releases)
[![License](https://img.shields.io/github/license/katl-dev/katl)](LICENSE)

Katl produces and maintains **KatlOS**, an immutable, installable, upgradeable,
systemd-native operating system for Kubernetes nodes.

KatlOS installs a small host, keeps operating-system and node configuration in
versioned generations, and prepares nodes for an explicit kubeadm bootstrap. It
uses standard Linux interfaces—systemd-boot, UKIs, `systemd-sysext`,
`systemd-confext`, `systemd-repart`, networkd, and kubeadm—rather than hiding
them behind a new cluster API.

> [!WARNING]
> KatlOS is experimental alpha software. Do not use it for production,
> security-sensitive, regulated, or availability-critical clusters. Alpha
> formats and workflows may change incompatibly and reinstall may be required.
> Read the [support boundary](docs/support.md) before evaluating a release.

## What Katl provides

- A self-contained UEFI installer ISO with the matching KatlOS image embedded.
- Loose kernel, initrd, UKI, and KatlOS artifacts for user-managed PXE systems.
- `katlctl`, the workstation CLI for validating cluster intent, compiling
  install bundles, bootstrapping Kubernetes, applying node configuration, and
  staging host upgrades.
- `katlc`, the node-local configuration and lifecycle agent.
- Immutable Kubernetes sysext bundles published at
  [`ghcr.io/katl-dev/kubernetes`](https://github.com/orgs/katl-dev/packages/container/package/kubernetes).
- SHA-256 manifests and keyless GitHub build-provenance attestations for release
  artifacts and Kubernetes bundles.

Katl does **not** provide DHCP/PXE infrastructure, DNS, CNI, GitOps, ingress,
storage, monitoring, backups, workload policy, or application lifecycle. Katl
prepares kubeadm-ready nodes; users operate the cluster built on top.

## How it fits together

```text
ClusterConfig YAML
        │
        ▼
katlctl config bundle ───────────► one .katlcfg for every node
                                            │
KatlOS installer ISO ── boot node ◄─────────┘ select node by name
        │
        ▼
immutable generation 0 + node-local katlc agent
        │
        ▼
katlctl cluster bootstrap ───────► Kubernetes OCI bundle
        │
        ▼
kubeadm cluster handoff ─────────► user-managed CNI, GitOps, and workloads
```

Configuration changes and host upgrades create later generations. Boot health
and rollback operate on KatlOS host artifacts; they do not undo etcd, kubeadm,
Kubernetes objects, persistent volumes, or application data.

## Quick start

The complete procedure, including a working multi-node `ClusterConfig`, PXE
arguments, disk safety rules, and troubleshooting, is in the
[installation guide](docs/installing.md). The outline below shows the normal ISO
path.

### 1. Download a release

From the [Katl releases page](https://github.com/katl-dev/katl/releases),
download these files from the same release:

```text
katl-installer.iso
katlctl-<version>-linux-amd64
```

Checksums and GitHub build attestations are also published for operators who
want to authenticate downloaded artifacts. They are optional on the normal
trusted-home-network path; see [Verify release artifacts](docs/operations/verify-release.md).

Install the matching operator CLI and confirm its embedded identity:

```sh
VERSION=2026.7.0-alpha.2
install -m 0755 "katlctl-$VERSION-linux-amd64" ~/.local/bin/katlctl
katlctl version
```

The ISO already contains the matching KatlOS root image. Users do not need to
distribute a separate root image for ISO installs. Loose artifacts remain
available for PXE.

### 2. Describe the cluster once

Create one `config.katl.dev/v1alpha1` `ClusterConfig` containing shared
defaults and flat per-node intent. `katlctl config init` supplies a working DHCP
and kubeadm-ready starting point; repeat `--node` for the intended topology:

```sh
katlctl config init ./cluster.yaml \
  --node cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-KATL_CP_1_ROOT \
  --node worker-1=worker,192.0.2.21,/dev/disk/by-id/ata-KATL_WORKER_1_ROOT
```

By default, config generation imports supported public keys from the active SSH
agent, including the 1Password SSH agent, and then tries
`~/.ssh/id_ed25519.pub`. Use `--ssh-authorized-key PATH` to select one key
explicitly. If no key is available, Katl still writes the editable config with
a warning; add an authorized key before `install apply`.

Review the generated disk identities, addresses, Kubernetes selection, and
network configuration before installing.

When an installer address is already known, `config init` can fetch its live
status and disk inventory instead of requiring the full node tuple. Repeat the
flag to build a multi-node starter config in the supplied order:

```sh
katlctl config init ./cluster.yaml \
  --installer 192.0.2.11 \
  --installer 192.0.2.21
```

If the nodes are already booted into the installer, skip the manual `--node`
arguments and generate this same file from live discovery in the next step.

Use the readable version tag on the normal path. Katl resolves the selected
content once for the operation and checks what it downloads internally. An
immutable digest pin remains available as an optional reproducibility control.

Keep `cluster.yaml` as the operator-facing source. `katlctl` compiles the
internal bundle automatically when installing and bootstrapping. Explicit
bundle output remains available for PXE and offline provisioning:

```sh
katlctl config bundle ./cluster.yaml --output ./katl-lab.katlcfg
```

Operators do not need to produce this file for the normal ISO path or copy any
of its internal digests.

### 3. Install each node

Attach or write `katl-installer.iso` using your normal UEFI virtual-media or USB
workflow, then boot it. The installer reports progress on both a local VGA
console and a 115200-baud serial console. Secure Boot must remain disabled until
Katl publishes signed boot artifacts. Without preseed input, the installer waits
safely. From the trusted provisioning network, discover waiting installers and
their stable disk identities:

```sh
katlctl install discover
```

To create `cluster.yaml` directly from all waiting installers, give discovery
the output path:

```sh
katlctl install discover ./cluster.yaml
```

Discovery assigns the first endpoint to `cp-1` and subsequent endpoints to
`worker-1`, `worker-2`, and so on. It writes a disk selector only when each
installer reports exactly one safe selectable disk; ambiguous disks produce an
error instead of a guess. Review the generated node names, roles, addresses,
disk identities, and Kubernetes selection before applying it.

Select the node to install by its configured name or bootstrap address. A
single-node config infers the node, so `--node` can be omitted:

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
```

Katl contacts the selected node's configured bootstrap address. If the
installer is temporarily reachable somewhere else—for example, it booted with
DHCP before applying a static address—override only the connection target:

```sh
katlctl install apply --config ./cluster.yaml --node cp-1 --endpoint 192.0.2.42
```

`--endpoint` accepts an IP, hostname, host and port, or complete HTTP(S) base
URL. When the selected node has no bootstrap address and no override is given,
Katl discovers a unique waiting installer.

The command validates and compiles the source locally, confirms the installer
is waiting, submits the selected configuration once, and waits for reboot-ready
or a classified failure.

Installation is destructive only when the install operation authorizes disk
mutation; Katl carries the compiled wipe guard internally.
Use `/dev/disk/by-id`, WWN, or serial selectors; do not rely on `/dev/sda`-style
names. Inspect the resolved disk for every node before granting wipe consent.

After reboot, generation 0 is a healthy KatlOS node with no Kubernetes
activation. Kubernetes bootstrap is a separate operator action.

### 4. Bootstrap Kubernetes

Once all installed nodes are reachable on the trusted home network, bootstrap
directly from the same cluster source:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --init-node cp-1
```

The node agent fetches the selected Kubernetes OCI bundle, verifies its
manifest and layer digests, stages the sysext, creates generation 1, and runs
the bounded kubeadm operation. Katl reports phase changes and writes the
operator kubeconfig to `./kubeconfig`; rerunning the unchanged command resumes
an interrupted bootstrap.

## Configuration and upgrades

`ClusterConfig` remains the user-authored source after installation. An
optional plan contacts the selected node but does not accept an operation:

```sh
katlctl node apply cp-1 --config ./cluster.yaml --plan
```

Run the command without `--plan` to apply it. `katlctl` derives config versions,
generation names, validation, and operation tracking internally.
Supported changes compile into generation-scoped confext/sysext state and are
applied live or on next boot according to the domain policy. See
[Apply runtime configuration](docs/installing.md#apply-runtime-configuration).

Routine host management uses the same `ClusterConfig` as installation and
bootstrap. Pass a node name in a larger cluster:

```sh
katlctl node status cp-1 --config ./cluster.yaml
katlctl node reboot cp-1 --config ./cluster.yaml
```

An optional workstation context created with `katlctl context save --config
./cluster.yaml` shortens repeated commands; it is not a prerequisite.

Status and reboot results are concise text by default. Add `--output json` for
automation. Reboot honors any generation already staged for the next boot and
waits for a new agent instance to report healthy; `--no-wait` deliberately
detaches after the reboot is scheduled.

Host upgrades take a release version and select the node from `ClusterConfig`.
`katlctl` resolves the published image, stages it, reboots the node, and waits
for the new generation to pass boot health:

```sh
katlctl node upgrade v2026.7.0-alpha.9 cp-1 --config ./cluster.yaml
```

Add `--plan` to check the upgrade without changing or rebooting the node. The
node calculates and records image identity internally before changing the
inactive root slot.

Kubernetes upgrades use the retained `ClusterConfig` and an exact Kubernetes
version. Plan the whole control-plane-first rollout without supplying bundle
digests, artifact paths, snapshot metadata, generation IDs, or operation IDs:

```sh
katlctl kubernetes upgrade v1.36.1 --config ./cluster.yaml --plan
```

Remove `--plan` to run the complete control-plane-first, worker-second rollout.
Katl resolves the release-owned immutable bundle and runtime compatibility,
upgrades one node online at a time, and stops on the first failure. Each node
fetches the selected bundle itself; control-plane nodes capture pre-mutation
etcd snapshot evidence.
See [Upgrade Kubernetes](docs/operations/upgrade-kubernetes.md).

Mutating commands follow the node's durable operation to completion by default.
Progress is written to stderr and the final structured result to stdout. A lost
watch automatically falls back to polling the durable node record.

Use `--no-wait` only for deliberately detached work. The detached response
includes an operation reference. Current and recent operations remain
discoverable afterward:

```sh
katlctl operations list \
  --config ./cluster.yaml --node cp-1
```

## Release artifacts

| Artifact | Purpose |
| --- | --- |
| `katl-installer.iso` | Primary self-contained UEFI installation media |
| `katl-installer.efi` | Installer UKI for suitable UEFI/PXE flows |
| `katl-installer.vmlinuz` + `.initrd` | Loose kernel and initrd for PXE |
| `katlos-install-<version>-<arch>.squashfs` | KatlOS payload for loose/PXE installation |
| `katlos-upgrade-<version>-<arch>.squashfs` | KatlOS host-upgrade payload |
| `katlctl-<version>-linux-amd64` | Matching workstation operator CLI |
| `SHA256SUMS` and `PROVENANCE.md` | Integrity manifest and trust instructions |

Every release image is a production image. VM-test agents, test services, and
test debug-shell support are built only in an explicitly instrumented test
variant and are rejected by release verification if they appear in production
artifacts.

## Project status and documentation

The current supported evaluation surface is x86-64 UEFI, the published ISO or
matching loose artifacts, one explicitly selected disk per node, the matching
`katlctl`, and kubeadm bootstrap using a compatible published Kubernetes
bundle. Hardware claims extend only to retained release evidence.

- [Installing KatlOS](docs/installing.md) — complete ISO and PXE workflows.
- [Operating KatlOS](docs/operations/README.md) — task-oriented runbooks for
  access, bootstrap, configuration, upgrades, wipe/reinstall, and diagnosis.
- [Support boundary](docs/support.md) — compatibility, trust, recovery, and
  reporting expectations.
- [Developing Katl](docs/developing.md) — build, test, and contribution loop.
- [North-star architecture](docs/internal/north-star.md) — durable product
  direction and system boundaries.
- [GitHub issues](https://github.com/katl-dev/katl/issues) — bugs and feature
  tracking; use [private vulnerability reporting](https://github.com/katl-dev/katl/security/advisories/new)
  for security-sensitive reports.

## Development

Katl product logic is written in Go; mkosi builds the images. The baseline local
gate is:

```sh
go test ./...
scripts/check-fast origin/main...HEAD
```

Image and VM changes require the matching mkosi and libvirt-backed
`scripts/vmtest-run` gates on a capable host. See
[docs/developing.md](docs/developing.md) before sending a pull request.

## License

Katl is licensed under the [MIT License](LICENSE).
