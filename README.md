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

Author one `config.katl.dev/v1alpha1` `ClusterConfig` containing shared
defaults and per-node overrides. It selects stable target-disk identities,
networkd configuration, SSH keys, system roles, kubeadm input, and a Kubernetes
bundle:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: katl-lab
spec:
  controlPlaneEndpoint: api.katl.test:6443
  kubernetes:
    version: v1.36.1
    bundle: ghcr.io/katl-dev/kubernetes:<version>
  # defaults, kubeadmConfigs, and nodes omitted here; use the complete example
  # in docs/installing.md.
```

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
safely and prints its HTTP handoff URL. On the trusted provisioning network,
apply the cluster source while selecting the node by name:

```sh
INSTALLER_ENDPOINT=http://192.0.2.10:8080
katlctl install apply ./cluster.yaml \
  --endpoint "$INSTALLER_ENDPOINT" \
  --node cp-1
```

The command validates and compiles the source locally, confirms the installer
is waiting, submits the selected configuration once, and waits for reboot-ready
or a classified failure.

Installation is destructive only when both the resolved node sets
`install.wipeTarget: true` and boot input authorizes automatic installation.
Use `/dev/disk/by-id`, WWN, or serial selectors; do not rely on `/dev/sda`-style
names. Inspect the resolved disk for every node before granting wipe consent.

After reboot, generation 0 is a healthy KatlOS node with no Kubernetes
activation. Kubernetes bootstrap is a separate operator action.

### 4. Bootstrap Kubernetes

Once all installed nodes are reachable through their `katlc` endpoints, use the
same cluster source. First retrieve each node's distinct
agent token over SSH and populate the per-node `file:` credential references as
described in [Access installed nodes](docs/operations/access.md):

```sh
katlctl cluster bootstrap ./cluster.yaml \
  --init-node cp-1 \
  --kubeconfig-out ./kubeconfig \
  --overwrite-kubeconfig
```

The node agent fetches the selected Kubernetes OCI bundle, verifies its
manifest and layer digests, stages the sysext, creates generation 1, and runs
the bounded kubeadm operation. Katl then hands the Kubernetes API and
kubeconfig to the operator.

## Configuration and upgrades

`ClusterConfig` remains the user-authored source after installation. Preview a
node-specific runtime request before submitting it:

```sh
katlctl config render-node \
  --source ./cluster.yaml \
  --node cp-1 \
  --desired-version 2 > cp-1.runtime.yaml
```

Use `katlctl config apply validate` to plan through the node agent, then submit
the identical inputs without `validate`. Supported changes compile into
generation-scoped confext/sysext state and are applied live or on next boot
according to the domain policy. See [Apply runtime configuration](docs/installing.md#apply-runtime-configuration).

Host upgrades consume the published `katlos-upgrade-<version>-<arch>.squashfs`
artifact. Plan before accepting a next-boot generation:

```sh
TAG=v2026.7.0-alpha.2
VERSION=${TAG#v}
IMAGE="katlos-upgrade-$VERSION-x86_64.squashfs"
katlctl host upgrade \
  --plan \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --candidate-generation "katlos-$VERSION" \
  --image-url "https://github.com/katl-dev/katl/releases/download/$TAG/$IMAGE"
```

The node calculates and records the downloaded image identity before changing
the inactive root slot.

Kubernetes upgrades use the workstation cluster context and a readable bundle
reference. Plan the whole control-plane-first rollout without supplying bundle
digests, artifact paths, snapshot metadata, generation IDs, or operation IDs:

```sh
katlctl cluster upgrade kubernetes \
  --bundle ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1 \
  --plan
```

Remove `--plan` to upgrade the next eligible node. Reboot that node, verify
cluster health, then rerun the same command to advance through control planes
and workers one at a time. Each node fetches and verifies the selected bundle
itself; control-plane nodes capture pre-mutation etcd snapshot evidence. See
[Upgrade Kubernetes](docs/operations/upgrade-kubernetes.md).

Remove `--plan` only after reviewing the response. Unattended fleet rollout is
not a supported alpha workflow.

Mutating commands follow the node's durable operation to completion by default.
Progress is written to stderr and the final structured result to stdout. A lost
watch automatically falls back to polling the durable node record.

Use `--no-wait` only for deliberately detached work. The detached response
includes an operation reference. Current and recent operations remain
discoverable afterward:

```sh
katlctl operations list \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token
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
