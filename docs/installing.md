# Installing KatlOS

Status: early user-facing guide. KatlOS is experimental alpha software; read
the [support boundary](support.md) before installing it.

This document is the installation reference for ISO and PXE paths. After
generation 0 boots, continue with the task-oriented
[KatlOS Operator Guide](operations/README.md) for management access, Kubernetes
bootstrap, configuration, upgrades, recovery, and troubleshooting.

Katl publishes a versioned installer ISO containing the matching KatlOS payload.
Write or attach that one artifact, then provide node-specific install input at
boot time. Loose boot and payload artifacts remain available for PXE. Katl does
not manage DHCP, TFTP, iPXE, matchbox, firmware boot order, USB imaging, GitOps
repositories, CNI add-ons, Flux, or Kubernetes workloads.

The install boundary is:

```text
Katl build output
  one self-contained installer ISO
  loose installer and KatlOS payload artifacts for PXE

User-managed provisioning
  PXE, iPXE, matchbox, virtual media, USB, or another boot path
  one compiled cluster config bundle and a selected node name

katlos-install
  reads the bundle and selects one node's compiled install plan
  binds that plan to the embedded or explicitly supplied KatlOS image
  mutates the selected disk only after validation
  installs generation 0 and records installed-node handoff state

katlctl cluster bootstrap
  later asks node-local katlc agents to create generation 1 and run kubeadm
```

## Artifacts

Download the operator CLI for the workstation that will compile configuration
and bootstrap the cluster:

```text
katlctl-<version>-linux-amd64
katlctl-<version>-linux-amd64.sha256
katlctl-<version>-linux-amd64.json
```

Install it under the stable command name and confirm that its embedded release
identity matches the KatlOS release:

```sh
VERSION=2026.7.0-alpha.2
install -m 0755 "katlctl-$VERSION-linux-amd64" ~/.local/bin/katlctl
katlctl version
```

For USB, optical, or virtual media, use the primary release artifact:

```text
katl-installer.iso
katl-installer.iso.sha256
katl-installer.iso.json
```

The ISO metadata records the release version and architecture. The ISO contains
the temporary installer and exactly one matching KatlOS install image. It never
contains node identity or credentials.

For PXE, publish the loose installer boot artifacts that match your boot path:

```text
katl-installer.efi
katl-installer.efi.sha256
katl-installer.efi.json

katl-installer.vmlinuz
katl-installer.vmlinuz.sha256
katl-installer.vmlinuz.json

katl-installer.initrd
katl-installer.initrd.sha256
katl-installer.initrd.json
```

and one KatlOS install payload:

```text
katlos-install-<version>-<arch>.squashfs
katlos-install-<version>-<arch>.squashfs.sha256
katlos-install-<version>-<arch>.squashfs.json
```

The installer boot artifacts start the temporary installer environment. The
KatlOS image is the payload that `katlos-install` verifies and writes into the
installed system. Do not rebuild either artifact for each node. Put node
identity, disk selection, networkd snippets, SSH authorized keys, system role,
and bootstrap intent in one `ClusterConfig`, then compile one bundle for all
nodes.

### Optional: authenticate release artifacts

The ISO and matching `katlctl` binary are sufficient for the normal trusted
home-lab path. Each KatlOS tag also includes `SHA256SUMS`, adjacent checksum
files, `PROVENANCE.md`, and a resolved package inventory for operators who want
to authenticate the downloaded bytes. Those checks are optional and do not
change how KatlOS operates.

To check transport integrity, download the checksum manifest alongside the
artifacts and run:

```sh
sha256sum --ignore-missing --check SHA256SUMS
```

Then authenticate each asset against the keyless GitHub attestation issued to
the Katl release workflow. Pin the expected tag in the verification policy:

```sh
TAG=v2026.7.0-alpha.2
gh attestation verify katl-installer.iso \
  --repo katl-dev/katl \
  --signer-workflow katl-dev/katl/.github/workflows/release-artifacts.yml \
  --source-ref "refs/tags/$TAG"
```

Repeat the attestation check for the KatlOS SquashFS or loose PXE artifact you
will use. The attestation binds those bytes to the repository, workflow, tag,
and source commit. It does not make the build vulnerability-free and is not a
UEFI Secure Boot signature; production boot-key policy and node-side signature
enforcement remain separate work.

## Author One ClusterConfig

Normal installation starts from one `config.katl.dev/v1alpha1` `ClusterConfig`.
It describes operator choices: the Kubernetes version and the desired identity,
role, networking, and install target of each node. Katl selects release
artifacts and role-dependent Kubernetes bootstrap state and handles bundle and operation metadata
internally.

`katlctl config init` generates this minimal form. It uses DHCP and the KatlOS
image embedded in the release ISO. Replace the SSH key, node addresses, and
stable disk IDs:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: katl-lab
spec:
  # Stable Kubernetes API endpoint for multi-control-plane clusters.
  # controlPlaneEndpoint: api.home.arpa:6443
  # Set controlPlane: true on nodes that join the Kubernetes control plane.
  # Omission means worker.
  # Nodes use DHCP by default; native systemd-networkd files can be set under defaults or a node.
  kubernetes:
    version: v1.36.1
  defaults:
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-KATL_CP_1_ROOT
      bootstrap:
        address: 192.0.2.11
    - name: worker-1
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-KATL_WORKER_1_ROOT
      bootstrap:
        address: 192.0.2.21
```

The release ISO supplies the KatlOS image. ClusterConfig never contains image
URLs, bundle references, credentials, named kubeadm profiles, node classes, or
other compiler mechanisms.

Advanced users can keep native kubeadm settings without waiting for Katl to add
a typed field for each upstream option:

```yaml
spec:
  kubernetes:
    version: v1.36.1
    kubeadm:
      configFile: ./kubeadm.yaml
      patchesDir: ./kubeadm-patches
```

Both paths are relative to `cluster.yaml`. Katl validates the native document
kinds and API versions, rejects unsafe host paths and filesystem escapes,
supplies its required role defaults, and embeds the resulting files into the
compiled `.katlcfg`. It selects control-plane and worker documents internally
and injects bootstrap tokens and certificate material only when the explicit
bootstrap operation runs. Omit the entire `kubeadm` block for Katl's complete
defaults.

The ISO flow consumes this source directly: `katlctl install apply` and
`katlctl cluster bootstrap` compile the internal bundle automatically. Produce
an explicit bundle only for PXE or offline provisioning, as shown in the next
section.

Katl maintains the bundle's internal consistency metadata itself. Operators do
not need to retain it, pass it on the ISO path, or handle its digests.

Disk installation is destructive. Always inspect each resolved target disk
before enabling automatic install. Use `byID`, WWN, or serial selectors, never
`/dev/sda`-style names.

## PXE Or Matchbox

Publish the loose installer kernel and initrd, the KatlOS SquashFS and metadata,
and the single `.katlcfg` archive through your own HTTP infrastructure. Keep the
image selection out of ClusterConfig and supply the published release artifact
when compiling the PXE bundle:

```sh
katlctl config bundle ./cluster.yaml \
  --output ./katl-lab.katlcfg \
  --katlos-image-url https://boot.example.invalid/katl/2026.7.0/katlos-install-2026.7.0-x86_64.squashfs \
  --katlos-image-metadata ./katlos-install-2026.7.0-x86_64.squashfs.json
```

Current bundle-oriented kernel arguments are:

```text
katl.bundle.url=<config bundle URL>
katl.bundle.sha256=<optional expected config bundle archive SHA-256>
katl.bundle=<local config bundle path>
katl.node=<node name>
katl.install.mode=auto
katl.wait-for-config=1
katl.hold-for-debug=1
console=...
ip=...
```

The installer calculates the downloaded archive identity and checks the bundle
structure itself. `katl.bundle.sha256` is an optional expert control when an
external expected checksum is already available. Without input, or with
`katl.wait-for-config=1`, the installer waits for handoff. Debug mode never
starts an install.

Illustrative iPXE entry for `cp-1`:

```ipxe
#!ipxe
set base https://boot.example.invalid/katl/2026.7.0
set node cp-1
kernel ${base}/katl-installer.vmlinuz initrd=katl-installer.initrd console=ttyS0,115200n8 systemd.getty_auto=no katl.node=${node} katl.bundle.url=${base}/katl-lab.katlcfg katl.install.mode=auto
initrd ${base}/katl-installer.initrd
boot
```

Matchbox profiles carry the same `katl.*` arguments. Groups should select only
`katl.node`; they do not need a different bundle URL per node. Katl does not
create or operate DHCP, iPXE, or matchbox configuration.

## ISO Or Local Handoff

Boot the same `katl-installer.iso` on each node without preseed input. The
installer mounts its embedded KatlOS image read-only and waits without mutating
disks.
The VGA console keeps a KatlOS dashboard on `tty1` showing installer media
version and state, active network addresses, the handoff URL, disk-mutation
status, and a live tail of the boot journal. Press `Ctrl+Alt+F2` for a local
recovery shell; the dashboard, serial journal, and SSH service operate
independently.

The handoff is intentionally unauthenticated HTTP for the trusted home-lab
path. Use only the provisioning network and never expose port 8080 to an
untrusted network. The installer accepts one valid submission and then closes
the handoff path.

Discover waiting installers and their disk inventory from the workstation:

```sh
katlctl install discover
```

If the installer addresses are already known, query them directly while
creating the config instead of scanning the local subnet:

```sh
katlctl config init ./cluster.yaml \
  --installer 192.0.2.11 \
  --installer 192.0.2.21
```

Bare addresses use HTTP port 8080. A hostname, `host:port`, or complete HTTP(S)
base URL is also accepted. The first supplied installer becomes `cp-1`; later
installers become workers.

The report marks a disk `selectable` only when it is a writable whole disk, is
not mounted, and has a stable by-id, WWN, or serial selector. To turn all waiting
installers into an editable cluster source directly, provide an output path:

```sh
katlctl install discover ./cluster.yaml
```

Config generation imports supported public keys from the active SSH agent,
including the 1Password SSH agent, and falls back to
`~/.ssh/id_ed25519.pub`. Pass `--ssh-authorized-key PATH` to choose a key file
explicitly. If neither source provides a key, discovery still writes the file
and warns that an authorized key must be added before `install apply`.

The first discovered endpoint becomes `cp-1`; subsequent endpoints become
workers. Katl writes a target selector only when an installer reports exactly
one selectable disk and refuses to guess when multiple disks are eligible.
Review and adjust the generated node names, roles, addresses, disk identities,
and Kubernetes selection before applying it. Never substitute a transient
`/dev/vda` or `/dev/sda` path.

Apply the cluster source directly:

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
```

`katlctl` selects the endpoint automatically when exactly one installer is
waiting. If multiple installers are present, choose the intended discovery
result with `--endpoint URL`.

For the worker, boot the same ISO and apply the same source with
`--node worker-1`. `katlctl install apply` compiles and validates the source and
selected node before contacting the installer. The installer
then validates the compiled install material and embedded KatlOS image before
it can mutate the selected disk. The endpoint refuses later submissions.

The command waits by default and returns structured status when installation is
reboot-ready or reaches a classified failure. Pass `--no-wait` only when
another operator process will monitor `katlctl install status` and the console.

The console advertises `/v1/config-bundle` as the preferred endpoint.
`/v1/install` remains available for advanced compiled-manifest integrations.

Separate seed media with the `KATLSEED` label or `virtio-katl-seed` disk ID is
only needed when provisioning input without the HTTP handoff.

## Advanced Compiled InstallManifest Boundary

The bundle contains one compiled `install.katl.dev/v1alpha1` `InstallManifest`
per node. That schema and the legacy `katl.manifest.*` kernel arguments remain
an advanced integration boundary for installer tooling and debugging. They are
not the normal authoring API: a raw manifest omits cluster-wide validation,
embedded kubeadm sidecars, resolved inventory, and proof that every node was
compiled from the same source. Author `ClusterConfig` and distribute its bundle
unless you are deliberately integrating at that lower-level boundary.

## Installer Safety And Status

`katlos-install` validates before destructive disk mutation:

```text
config bundle archive integrity and content descriptors
selected node and compiled install manifest schema
destructive install guard
target disk selector and size
KatlOS image SHA-256 and size
embedded /katlos/image.json
required runtime root and runtime UKI components
component digests from the mounted KatlOS image
architecture and runtime interface compatibility
```

Failures before validation completes must not repartition or wipe the selected
disk. Failures after mutation are recorded in installer status and should be
debugged from the installer console, journal, and `/var/lib/katl/install` state.

After install and reboot, a healthy generation 0 node should:

```text
boot from the installed disk
start systemd-networkd and sshd when configured
start the node-local katlc agent
record runtime handoff status
reach katl-boot-complete.target
not activate Kubernetes or require katl-kubeadm-ready.target
remain ready for explicit Kubernetes bootstrap but not bootstrapped
```

Useful first checks on an installed node:

```text
systemctl status katl-runtime-handoff-status.service
systemctl status katl-boot-health.service
systemctl status katlc-agent.service
systemctl status katl-boot-complete.target
systemctl status katl-kubeadm-ready.target
journalctl -b -u katlos-install.service -u katl-runtime-handoff-status.service -u katlc-agent.service
```

## Bootstrap Handoff

Installation does not run `kubeadm`, fetch Kubernetes payloads, or bundle a
Kubernetes sysext. It stores the node role and bootstrap intent needed for a
later explicit operator action.

During explicit bootstrap, Katl resolves `spec.kubernetes.version` through the
release compatibility catalog to an immutable bundle digest and compatible
KatlOS runtime interface. It fails before mutation when the version is not
available for this Katl release. `katlc` then fetches the selected public
`ghcr.io/katl-dev/kubernetes` artifact, verifies every OCI layer and its runtime
compatibility, stages the sysext, and selects it for generation 1. Operators do
not supply a bundle tag or digest on the normal path.

After all nodes are installed and reachable through their node-local `katlc`
management endpoints, bootstrap directly from the same source:

```text
katlctl cluster bootstrap --config ./cluster.yaml \
  --init-node cp-1 \
  --kubeconfig-out kubeconfig \
  --overwrite-kubeconfig
```

Katl compiles the source internally to obtain the control-plane endpoint, node
topology, roles, kubeadm references, Kubernetes version, and OCI bundle
selection. `--node-address node=address` remains available for an
operator-observed address that differs from the compiled source.

Each freshly installed node generates a distinct agent token. Enroll the
installed nodes before bootstrap:

```text
katlctl cluster enroll --config ./cluster.yaml
```

`katlctl` retrieves the tokens over SSH, stores them at the `file:` credential
references with mode `0600`, verifies every management endpoint, and creates a
workstation context. Do not put token values in `ClusterConfig`.

`katlctl` is a bounded client. Node-local `katlc` validates and records the
authoritative bootstrap operations, creates generation 1, runs `kubeadm`, and
records evidence. `katlctl` output and kubeconfig files are operator artifacts,
not node recovery state.

Katl hands off the cluster layer after kubeadm bootstrap. Users provide and
operate CNI, CoreDNS policy, Flux or another GitOps tool, workloads, ingress,
storage classes, and cluster add-ons. Test fixtures may apply a small CNI or
workload manifest to prove handoff behavior, but Katl is not a Kubernetes
distribution or add-on manager.

## Apply Runtime Configuration

`ClusterConfig` is the user-authored source for installation and supported
node runtime configuration. `katlctl` compiles and submits the node-agent
request internally; operators do not maintain a second configuration schema.

Optionally plan the exact per-node runtime request through the node agent:

```text
katlctl node apply --config ./cluster.yaml --node cp-1 --plan
```

`katlctl` derives the source version, candidate generation, authenticated
endpoint, validation request, and operation tracking. These are internal
replay and lifecycle details, not operator inputs.

If the source has already been compiled, use the bundle instead of
recompiling it:

```text
katlctl node apply \
  --config ./katl-lab.katlcfg \
  --node cp-1 \
  --plan
```

Remove `--plan` to submit the accepted plan. The default `--mode auto` uses
the agent's domain matrix to choose live apply or next boot; `--mode live` and
`--mode next-boot` request an explicit policy and are rejected when unsafe.
Keep the same configuration inputs when submitting. `katlctl` generates the
idempotency key and follows the accepted operation to its terminal result.

The renderer carries the node's desired identity and systemd-networkd files as
well as operation-only system role and role-dependent Kubernetes bootstrap state. The
planner applies runtime-safe changes and reports when a lifecycle operation is
required; it does not silently discard operation-only differences. Disk install
selection and Kubernetes version changes use their dedicated install and
upgrade workflows. A change to bounded native kubeadm input records that a
kubeadm-aware action is required; normal config apply does not mutate
kubeadm-owned cluster state. Use the dedicated operation for a supported live
change. The node-agent request envelope is an internal API and is documented
separately from the operator installation flow.

## Upgrades

KatlOS upgrades also consume one KatlOS image artifact. The image records
runtime root, boot assets, versions, architecture, runtime
interface, and digests. An upgrade operation creates a candidate generation from
that image and validates component compatibility before selection.

Kubernetes version upgrades remain separate from day-one install. Use
`katlctl kubernetes upgrade` to plan and execute the kubeadm-aware,
control-plane-first rollout from a published bundle. Katl resolves the bundle
identity, captures control-plane etcd snapshot evidence, stages the target
`kubeadm` privately before releasing the target kubelet, and reports recovery
requirements without asking operators for digests, artifact paths, snapshot
metadata, generation IDs, or operation IDs. Follow
[Upgrade Kubernetes](operations/upgrade-kubernetes.md); activating a published
sysext manually is not a supported upgrade.

## Troubleshooting

Collect these first:

```text
installer console output
journalctl -b
journalctl -b -u katlos-install.service
/var/lib/katl/install status and copied manifest files
the config bundle and selected node name
the KatlOS image metadata and SHA-256 file
systemctl status katlc-agent.service
systemctl status katl-boot-complete.target
```

Common failures:

```text
bundle or selected node rejected
  Run katlctl config validate again. Check the selected node name, SSH
  authorized keys, system role, kubeadm
  role, address, and target disk. PXE bundle compilation must receive the
  published image URL and metadata flags; ISO installs derive the image from
  the embedded media descriptor.

destructive install refused
  Confirm boot input selected katl.install.mode=auto. Confirm the URL is
  reachable and returns the intended .katlcfg archive.

target disk not found
  Prefer /dev/disk/by-id, WWN, or serial selectors. Confirm firmware and HBA
  expose the expected device identity in the installer environment.

KatlOS image digest mismatch
  Re-publish the image and .sha256 together. Do not edit the SquashFS after
  metadata is generated.

node boots but bootstrap fails
  Inspect katlc operation records, kubeadm output, kubelet journal, container
  runtime journal, and the Kubernetes API state. Host rollback does not erase
  kubeadm or Kubernetes partial state.
```

## Unsupported Or Day-2

Current day-one docs intentionally do not cover:

```text
Katl-managed DHCP, TFTP, iPXE, or matchbox services
automatic cluster reconciliation after bootstrap
control-plane join through operation-backed bootstrap
optional node application sysexts such as BIRD, gVisor, or Kata
hardware extension catalogs or per-node installer artifact rebuilds
secret distribution beyond protected install input or local handoff
production signing, revocation, and private artifact distribution policy
```

Those areas need separate design and tests before they become supported
operator workflows. The complete production, compatibility, trust, recovery,
hardware-evidence, and issue-reporting boundary is maintained in
[`support.md`](support.md).
