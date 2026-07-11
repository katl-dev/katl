# Installing KatlOS

Status: early user-facing guide.

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
  per-node install manifests and credentials

katlos-install
  validates one install manifest and one KatlOS image
  mutates the selected disk only after validation
  installs generation 0 and records installed-node handoff state

katlctl cluster bootstrap
  later asks node-local katlc agents to create generation 1 and run kubeadm
```

## Artifacts

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
katl-installer-<version>-<arch>.efi
katl-installer-<version>-<arch>.efi.sha256
katl-installer-<version>-<arch>.efi.json

katl-installer-<version>-<arch>.vmlinuz
katl-installer-<version>-<arch>.vmlinuz.sha256
katl-installer-<version>-<arch>.vmlinuz.json

katl-installer-<version>-<arch>.initrd
katl-installer-<version>-<arch>.initrd.sha256
katl-installer-<version>-<arch>.initrd.json
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
and bootstrap intent in the install manifest.

## Install Manifest

Each node needs an `install.katl.dev/v1alpha1` manifest. YAML is the preferred
operator-facing format because it keeps native systemd and kubeadm snippets
readable. JSON manifests are accepted for tooling compatibility.

This example is a control-plane node that uses DHCP and the KatlOS image embedded
in its installer ISO:

```yaml
apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
node:
  identity:
    hostname: cp-1
    ssh:
      authorizedKeys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  systemRole: control-plane
  networkd:
    files:
      - name: 10-lan.network
        content: |
          [Match]
          Name=enp1s0

          [Network]
          DHCP=yes
  kubernetes:
    kubeadm:
      configRef: control-plane
  bootstrap:
    clusterName: katl-lab
    inventoryNodeName: cp-1
    nodeAddress: 192.0.2.11
    controlPlaneEndpoint: api.katl.test:6443
    bootstrapProfileRef: control-plane
    kubernetesBundleSource: https://ghcr.io/v2/katl/kubernetes-payloads
    kubernetesBundleRef: v1.36.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
install:
  wipeTarget: true
  targetDisk:
    byID: /dev/disk/by-id/ata-KATL_EXAMPLE_ROOT_DISK
    minSizeMiB: 32768
```

When booted from the versioned ISO, omit `katlosImage`; the installer binds the
manifest to the embedded image metadata and verifies the image before disk
mutation. PXE and other loose-artifact flows must provide an explicit
`katlosImage.url` or `katlosImage.localRef` with its digest and identity.

Worker nodes use the same schema with `systemRole: worker` and a worker
bootstrap profile reference when you want to preserve that intent for later
`katlctl cluster bootstrap`.

Worker node fields differ only where the node identity, role, disk selector, and
bootstrap profile differ:

```yaml
node:
  identity:
    hostname: worker-1
    ssh:
      authorizedKeys:
        - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  systemRole: worker
  kubernetes:
    kubeadm:
      configRef: worker
  bootstrap:
    clusterName: katl-lab
    inventoryNodeName: worker-1
    nodeAddress: 192.0.2.21
    controlPlaneEndpoint: api.katl.test:6443
    bootstrapProfileRef: worker
    kubernetesBundleSource: https://ghcr.io/v2/katl/kubernetes-payloads
    kubernetesBundleRef: v1.36.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
```

The destructive install guard is intentionally duplicated: the manifest must set
`install.wipeTarget` to `true`, and the boot path must select an install mode
that allows mutation. Prefer stable disk selectors such as
`/dev/disk/by-id/...`, WWN, or serial. Do not use short kernel-assigned device
names in manifests.

## PXE Or Matchbox

For network boot, publish the split installer kernel/initrd and each node's
install manifest through your own HTTP infrastructure. Your PXE, iPXE, or
matchbox config passes only enough input for `katlos-install` to find and verify
the manifest.

Bootstrap-ready manifests that set `node.kubernetes.kubeadm.configRef` also need
the referenced kubeadm sidecars in the installer material set:

```text
kubeadm-configs/<configRef>.yaml
kubeadm/<configRef>.yaml
```

The current URL boot path downloads the manifest itself. It does not yet fetch
those sidecars over the network. If the node must preserve kubeadm bootstrap
intent for later `katlctl cluster bootstrap`, provide the manifest and sidecars
through local preseed or USB media, or ensure your installer wrapper places
those directories beside the downloaded manifest before `katlos-install` runs.

Current installer kernel arguments:

```text
katl.manifest.url=<InstallManifest URL>
katl.manifest.sha256=<InstallManifest SHA-256>
katl.manifest=<local manifest path>
katl.node=<node name>
katl.install.mode=auto
katl.artifact-base-url=<artifact base URL>
katl.wait-for-config=1
katl.hold-for-debug=1
console=...
ip=...
```

Use `katl.install.mode=auto` only when the manifest is correct and the target
disk selector has been checked. URL manifests require `katl.manifest.sha256`; a
URL without a digest fails before disk mutation. Without any manifest, or with
`katl.wait-for-config=1`, the installer waits for local handoff input instead of
mutating disks. `katl.hold-for-debug=1` keeps the installer in debug mode.

Illustrative iPXE entry:

```ipxe
#!ipxe
set base https://boot.example.invalid/katl/2026.06.04
set node cp-1
set manifest_sha bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
kernel ${base}/katl-installer-2026.06.04-x86_64.vmlinuz initrd=katl-installer-2026.06.04-x86_64.initrd console=ttyS0,115200n8 katl.node=${node} katl.manifest.url=https://boot.example.invalid/katl/manifests/${node}.yaml katl.manifest.sha256=${manifest_sha} katl.install.mode=auto
initrd ${base}/katl-installer-2026.06.04-x86_64.initrd
boot
```

Illustrative matchbox profile:

```json
{
  "id": "katlos-installer-x86_64",
  "name": "KatlOS installer",
  "boot": {
    "kernel": "https://boot.example.invalid/katl/2026.06.04/katl-installer-2026.06.04-x86_64.vmlinuz",
    "initrd": [
      "https://boot.example.invalid/katl/2026.06.04/katl-installer-2026.06.04-x86_64.initrd"
    ],
    "args": [
      "console=ttyS0,115200n8",
      "katl.node=${mac:hexhyp}",
      "katl.manifest.url=https://boot.example.invalid/katl/manifests/${mac:hexhyp}.yaml",
      "katl.manifest.sha256=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "katl.install.mode=auto"
    ]
  }
}
```

Illustrative matchbox group:

```json
{
  "id": "katlos-cp-1",
  "name": "KatlOS cp-1",
  "profile": "katlos-installer-x86_64",
  "selector": {
    "mac": "52:54:00:12:34:56"
  },
  "metadata": {
    "node": "cp-1"
  }
}
```

These snippets are examples of the boot input contract only. Katl does not
create matchbox profiles or groups for you.

## USB Or Local Handoff

For hands-on installs, boot the versioned KatlOS ISO without a preseeded
manifest. `katlos-install` mounts its payload read-only, enters local handoff mode,
prints a URL and one-time token to the console and journal, and waits for one
valid install manifest.

Operator flow:

```text
1. Boot the installer artifact.
2. Read the console line:
   katlos-install waiting for config at http://<installer-ip>:8080/v1/install token=<token>
3. Check status:
   curl http://<installer-ip>:8080/v1/status
4. Submit the same install manifest used by PXE:
   curl -X POST \
     -H "Authorization: Bearer <token>" \
     -H "Content-Type: application/yaml" \
     --data-binary @cp-1.install.yaml \
     http://<installer-ip>:8080/v1/install
```

After a manifest is accepted, the handoff endpoint refuses later submissions.
Invalid manifests keep the installer waiting and do not authorize disk mutation.

The release ISO is already offline-capable, so the submitted node manifest omits
`katlosImage`. Separate seed media with the `KATLSEED` label or the
`virtio-katl-seed` disk ID is only needed for preseeded node configuration and
credentials. Advanced custom media can still use `katlosImage.localRef`:

```yaml
katlosImage:
  localRef: images/katlos-install-2026.06.04-x86_64.squashfs
  sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  sizeBytes: 1073741824
  version: "2026.06.04"
  architecture: x86_64
  runtimeInterface: katl-runtime-1
  role: install
```

The full manifest still carries node identity, disk selection, role, kubeadm
config reference, and bootstrap intent. An explicit image reference overrides
media selection and changes only where the payload is read from.

## Installer Safety And Status

`katlos-install` validates before destructive disk mutation:

```text
install manifest schema
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

The Kubernetes bundle reference is exact. For example,
`node.bootstrap.kubernetesBundleRef: v1.36.0@sha256:<digest>` means bootstrap
must select the compatible Kubernetes payload bundle whose manifest digest
matches that pin. During the explicit bootstrap operation, `katlc` fetches that
bundle from `node.bootstrap.kubernetesBundleSource`, verifies the Katl bundle
metadata and payload digests, stages the sysext locally, and selects it for
generation 1. To bootstrap a fresh cluster on `v1.36.1`, keep the KatlOS install
image when runtime compatibility permits it and supply a source/ref pair that
resolves to the `v1.36.1` bundle.

After all nodes are installed and reachable through their node-local `katlc`
management endpoints, run bootstrap from an operator workstation:

```text
katlctl cluster bootstrap \
  --inventory cluster.yaml \
  --init-node cp-1 \
  --control-plane-endpoint api.katl.test:6443 \
  --kubeconfig-out kubeconfig \
  --overwrite-kubeconfig
```

`katlctl` is a bounded client. Node-local `katlc` validates and records the
authoritative bootstrap operations, creates generation 1, runs `kubeadm`, and
records evidence. `katlctl` output and kubeconfig files are operator artifacts,
not node recovery state.

Katl hands off the cluster layer after kubeadm bootstrap. Users provide and
operate CNI, CoreDNS policy, Flux or another GitOps tool, workloads, ingress,
storage classes, and cluster add-ons. Test fixtures may apply a small CNI or
workload manifest to prove handoff behavior, but Katl is not a Kubernetes
distribution or add-on manager.

## Upgrades

KatlOS upgrades also consume one KatlOS image artifact. The image records
runtime root, boot assets, versions, architecture, runtime
interface, and digests. An upgrade operation creates a candidate generation from
that image and validates component compatibility before selection.

Kubernetes version upgrades remain separate from day-one install. They require a
kubeadm-aware operation that can make the target `kubeadm` available before the
target kubelet starts. Until that path is implemented and tested, treat
Kubernetes upgrades as unsupported operational work. Producing and publishing a
new sysext such as `v1.36.1` is useful immediately for fresh installs and
future upgrade planning, but it does not by itself make `v1.36.0` to `v1.36.1`
mutation safe on an existing cluster.

## Troubleshooting

Collect these first:

```text
installer console output
journalctl -b
journalctl -b -u katlos-install.service
/var/lib/katl/install status and copied manifest files
the install manifest used for this node
the KatlOS image metadata and SHA-256 file
systemctl status katlc-agent.service
systemctl status katl-boot-complete.target
```

Common failures:

```text
manifest rejected
  Check apiVersion, kind, required node.identity.ssh.authorizedKeys,
  node.systemRole, optional node.kubernetes.kubeadm.configRef, and targetDisk.
  PXE manifests must also include the katlosImage fields; ISO installs derive
  them from the embedded media descriptor.

destructive install refused
  Confirm install.wipeTarget is true and boot input selected
  katl.install.mode=auto. For URL manifests, confirm katl.manifest.sha256 is
  present and matches the published manifest bytes.

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
Kubernetes version upgrade execution
optional node application sysexts such as BIRD, gVisor, or Kata
hardware extension catalogs or per-node installer artifact rebuilds
secret distribution beyond protected install input or local handoff
production signing, revocation, and private artifact distribution policy
```

Those areas need separate design and tests before they become supported
operator workflows.
