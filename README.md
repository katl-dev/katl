# Katl

[![Fast Checks](https://github.com/katl-dev/katl/actions/workflows/fast-checks.yml/badge.svg)](https://github.com/katl-dev/katl/actions/workflows/fast-checks.yml)
[![Release](https://img.shields.io/github/v/release/katl-dev/katl?include_prereleases&sort=semver)](https://github.com/katl-dev/katl/releases)
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
katlctl validate + bundle ───────► one verified .katlcfg for every node
                                            │
KatlOS installer ISO ── boot node ◄─────────┘ select node by name
        │
        ▼
immutable generation 0 + node-local katlc agent
        │
        ▼
katlctl cluster bootstrap ───────► verified Kubernetes OCI bundle
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

### 1. Download and verify a release

From the [Katl releases page](https://github.com/katl-dev/katl/releases),
download these files from the same release:

```text
katl-installer.iso
katl-installer.iso.sha256
katlctl-<version>-linux-amd64
katlctl-<version>-linux-amd64.sha256
SHA256SUMS
PROVENANCE.md
```

Verify transport integrity before using them:

```sh
sha256sum --ignore-missing --check SHA256SUMS
```

Authenticate the installer against the release workflow, replacing `<tag>`
with the exact tag you downloaded:

```sh
TAG=v2026.7.0-alpha.2
gh attestation verify katl-installer.iso \
  --repo katl-dev/katl \
  --signer-workflow katl-dev/katl/.github/workflows/release-artifacts.yml \
  --source-ref "refs/tags/$TAG"
```

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
    version: v1.36.0
    bundle: ghcr.io/katl-dev/kubernetes:<version>@sha256:<oci-manifest-digest>
  # defaults, kubeadmConfigs, and nodes omitted here; use the complete example
  # in docs/installing.md.
```

Digest pins are strongly recommended. A readable tag is accepted and works
with Renovate's Docker datasource, but a digest prevents the selected content
from changing before the operation resolves it.

Validate the source without writing output, then compile one content-addressed
bundle for all nodes:

```sh
katlctl config validate ./cluster.yaml
katlctl config bundle ./cluster.yaml --output ./katl-lab.katlcfg
sha256sum ./katl-lab.katlcfg
```

Keep both the printed `bundleDigest` and the archive SHA-256. They protect
different layers and are not interchangeable.

### 3. Install each node

Attach or write `katl-installer.iso` using your normal UEFI virtual-media or USB
workflow, then boot it. Without preseed input, the installer waits safely and
prints an HTTP handoff URL and one-time token. Store the token in a protected
temporary file, then submit the same bundle to every node while selecting that
node by name:

```sh
INSTALLER_ENDPOINT=http://192.0.2.10:8080
BUNDLE_DIGEST='sha256:...'
umask 077
read -rsp 'Installer token: ' INSTALL_TOKEN; printf '\n'
printf '%s\n' "$INSTALL_TOKEN" > ./installer.token
unset INSTALL_TOKEN

katlctl install apply \
  --endpoint "$INSTALLER_ENDPOINT" \
  --token-file ./installer.token \
  --config-bundle ./katl-lab.katlcfg \
  --config-bundle-digest "$BUNDLE_DIGEST" \
  --node cp-1
```

The command validates the bundle and selected node locally, confirms the
installer is waiting, submits once, and waits for reboot-ready or a classified
failure. Remove the temporary token file after the handoff.

Installation is destructive only when both the resolved node sets
`install.wipeTarget: true` and boot input authorizes automatic installation.
Use `/dev/disk/by-id`, WWN, or serial selectors; do not rely on `/dev/sda`-style
names. Inspect the resolved disk for every node before granting wipe consent.

After reboot, generation 0 is a healthy KatlOS node with no Kubernetes
activation. Kubernetes bootstrap is a separate operator action.

### 4. Bootstrap Kubernetes

Once all installed nodes are reachable through their `katlc` endpoints, use the
same config bundle and its internal digest. First retrieve each node's distinct
agent token over SSH and populate the per-node `file:` credential references as
described in [Access installed nodes](docs/operations/access.md):

```sh
BUNDLE_DIGEST='sha256:...'
katlctl cluster bootstrap \
  --config-bundle ./katl-lab.katlcfg \
  --config-bundle-digest "$BUNDLE_DIGEST" \
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
IMAGE_SHA256=$(sha256sum "$IMAGE" | awk '{print $1}')
IMAGE_SIZE=$(stat -c %s "$IMAGE")
katlctl host upgrade \
  --plan \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --candidate-generation "katlos-$VERSION" \
  --client-request-id "cp-1-katlos-$VERSION" \
  --image-url "https://github.com/katl-dev/katl/releases/download/$TAG/$IMAGE" \
  --image-sha256 "$IMAGE_SHA256" \
  --image-size-bytes "$IMAGE_SIZE"
```

Remove `--plan` only after reviewing the response. Automated fleet rollout and
Kubernetes version upgrade execution are not supported alpha workflows.

Every accepted config, host-upgrade, bootstrap, and destructive-reset response
includes an `operationId` and `requestDigest`. Query a snapshot or follow it to
terminal state through the same node agent:

```sh
katlctl operation status \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --operation-id "$OPERATION_ID" \
  --request-digest "$REQUEST_DIGEST" \
  --watch
```

The digest binds the query to the exact accepted request. Watch streams are an
optimization; `katlctl` falls back to the node's authoritative persisted status
if a stream is interrupted.

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
