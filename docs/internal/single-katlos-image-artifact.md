# Single KatlOS Image Artifact Contract

Status: current decision.

This document defines the user-facing KatlOS payload image used for install and
upgrade. It replaces the current loose install-time references to runtime root,
runtime UKI, Kubernetes sysexts, checksums, and metadata with one payload image.
Internal builds may still produce those components as intermediates.

The existing v1alpha1 install manifest shape with `artifacts.runtimeRoot`,
`artifacts.uki`, and `artifacts.sysexts` is a legacy scaffold. User-facing
install input must move to one `katlosImage` reference before the installer
consumption path is considered complete.

Installer boot artifacts are separate. A UKI, kernel/initrd pair, ISO, or USB
wrapper starts `katlos-install`; the KatlOS image is the payload that
`katlos-install` verifies and applies.

## Initial Format

Initial format: a read-only SquashFS filesystem image.

Rejected for the first implementation:

```text
tar or tar.zst archive
  easy to build, but not an image and requires extraction policy before
  component verification

OCI image or registry artifact
  useful later for distribution, but adds registry/client semantics before the
  local install and VM loop need them

whole-disk image
  conflicts with Katl's installer boundary because katlos-install owns disk
  partitioning, root slot selection, state filesystems, and boot metadata
```

A SquashFS payload is one file, can be mounted read-only by the installer, and
can contain the exact component bytes plus an embedded typed index. The top-level
image has an adjacent checksum and, later, an adjacent signature. Component
digests are recorded inside the embedded index and are verified after mount.

## Naming

Build outputs use role, version, architecture, and format in the name:

```text
katlos-install-<version>-<arch>.squashfs
katlos-install-<version>-<arch>.squashfs.sha256
katlos-install-<version>-<arch>.squashfs.json

katlos-upgrade-<version>-<arch>.squashfs
katlos-upgrade-<version>-<arch>.squashfs.sha256
katlos-upgrade-<version>-<arch>.squashfs.json
```

The adjacent JSON is publication metadata for humans, scripts, and catalogs. The
authoritative component index lives inside the SquashFS image and is verified
from the mounted image bytes. The adjacent JSON must not be the only source of
component metadata used by `katlos-install`.

## Image Layout

Required top-level layout:

```text
/katlos/image.json
/components/runtime/root.squashfs
/components/boot/katl.efi
/components/sysext/katl-kube-<version>.sysext
/components/metadata/
```

`/katlos/image.json` is the component index. Paths in the index are relative to
the mounted image root and must be normal relative paths. They must not be
absolute paths, contain `..`, or depend on host-specific locations.

The `components/metadata` directory may contain rendered boot entry templates,
component metadata copied from build intermediates, catalog snippets, and other
static inputs needed to create generation spec/status. It must not contain
node-specific identity, network addresses, kubeadm init/join secrets, bootstrap
tokens, SSH host keys, or generated node confext output.

## Index Schema

The first index schema is:

```text
apiVersion: katl.dev/v1alpha1
kind: KatlOSImage
imageRole: install | upgrade
format: squashfs
version
buildID
architecture
runtimeInterface
createdAt
components[]
```

Each component records:

```text
name
role
path
format
sizeBytes
sha256
version or artifactVersion
payloadVersion, when the payload has its own version such as Kubernetes
architecture
compatibility metadata
install target metadata, when needed by the installer
```

Required component roles for install images and combined upgrade images:

```text
runtime-root
  the immutable runtime root filesystem image written byte-for-byte into a root
  slot

runtime-uki
  the runtime UKI or boot asset installed into the ESP or XBOOTLDR area for the
  selected generation

kubernetes-sysext
  a bundled Kubernetes sysext available for exact manifest selection, including
  payload version,
  architecture, artifact version, source metadata, and supported runtime
  interfaces
```

The runtime-root component metadata must include the runtime version,
architecture, runtime interface, root artifact digest, minimum root slot size,
filesystem format, and any required root filesystem feature metadata.

The Kubernetes sysext component metadata uses the same compatibility vocabulary
as generation specs: artifact version, payload version, Kubernetes minor,
architecture, digest, size, source repo metadata, package versions when
available, and supported runtime interfaces.

The index may include one or more exact-version Kubernetes sysext artifacts. A
day-one install manifest requests an exact Kubernetes version such as `1.36.1`;
Katl resolves that request only against bundled image components with matching
payload version, for example `katl-kube-1.36.1.sysext`. Missing matches,
duplicate matches, version ranges, and catalog references fail validation. A
broader version catalog and compatibility matrix are day-2 update planning work.

The first implementation should model images as complete generation payloads:
install images and combined upgrade images carry all required roles. Partial
upgrade images may omit roles that are explicitly preserved from the current
generation only after the upgrade apply path defines and tests that preservation
behavior.

## Install Image

An install image contains the complete payload needed to create the first
runtime generation on a node:

```text
runtime root component
runtime UKI or boot assets
bundled Kubernetes sysext candidates for exact manifest-selected versions
static metadata needed to write generation spec/status and boot entries
component digests and compatibility metadata
```

Node-specific configuration remains outside the image. The install manifest,
PXE/preseed input, USB/local handoff, or VM harness supplies identity, disk
selection, network configuration, kubeadm config references, systemRole, the
exact Kubernetes payload version, and other supported day-one node
configuration. Capability overlays are a day-2 design item. Reusing the same
install image for multiple nodes must not require rebuilding the image.

Generation 0 uses the runtime root, boot assets, baseline metadata, and baseline
configuration needed to boot KatlOS. Bundled Kubernetes sysexts are available
image components and generation 1 inputs; they are not generation 0 selected or
active state. `katlctl cluster bootstrap` later asks `katlc` to create the first
Kubernetes-capable generation and select the bundled Kubernetes sysext whose
payload version exactly matches the manifest version.

The installer material model therefore changes from:

```text
install manifest -> runtimeRoot URL + UKI URL + sysext URLs
```

to:

```text
install manifest -> KatlOS install image URL/ref/digest + Kubernetes version 1.36.1
KatlOS install image -> generation 0 runtime payload + bundled Kubernetes sysext candidates
katlctl cluster bootstrap -> generation 1 selects katl-kube-1.36.1.sysext
```

## Upgrade Image

An upgrade image uses the same `KatlOSImage` schema with `imageRole: upgrade`.
It contains the payload needed to create a candidate generation from one
user-facing artifact.

Supported upgrade shapes:

```text
KatlOS-only
  replace runtime root and boot assets, and preserve the current Kubernetes
  sysext only when compatibility metadata validates that pair

Kubernetes-only
  replace the Kubernetes sysext, and preserve the current runtime root and boot
  assets only when compatibility metadata validates that pair

combined
  replace runtime root, boot assets, and Kubernetes sysexts from the image
```

Preserved components come from the currently installed generation, not from
loose files supplied by the operator. A candidate generation spec records
exactly which embedded and preserved component digests were validated together.

The first implementation may restrict upgrade images to the combined shape if
that keeps implementation and tests smaller. The schema must not prevent
KatlOS-only and Kubernetes-only updates later.

A Kubernetes upgrade image may still contain a single Kubernetes sysext for the
target steady state. The upgrade operation, not the image schema, must define
how target kubeadm is made available before target kubelet activation. Splitting
kubeadm into a separate tool component is an implementation option, not a
current image contract.

This is an explicit upgrade sequencing requirement, not an image-schema feature
and not a kubeadm replacement. If the target Kubernetes sysext contains both
`kubeadm` and `kubelet`, the upgrade path must provide a pre-kubelet gate: the
target `kubeadm` binary must be available for the operator, test harness, or a
future kubeadm-aware `katlctl` step before target `kubelet.service` is started or
restarted from that same payload. The mechanism is intentionally deferred. Valid
directions include service ordering, a held kubelet start condition, temporary
target-kubeadm exposure, or a split kubeadm tool payload. The image contract only
requires that the ordering be representable and testable.

## Node Configuration References

PXE/preseed, USB/local handoff, and VM tests all use the same node configuration
model after input discovery. That configuration points to one KatlOS image:

```text
katlosImage.url or katlosImage.localRef
katlosImage.sha256
katlosImage.sizeBytes, when known
katlosImage.role: install
```

Network boot wrappers may pass enough kernel arguments for `katlos-install` to
fetch the install manifest or payload reference, but durable install policy stays
in the install manifest. Kernel arguments must not carry node secrets or inline
large manifests.

Offline media may place the KatlOS image on the same ISO/USB filesystem as the
installer wrapper. Local handoff may post a manifest that references that local
image or a network image. In all cases, the node-specific manifest/config is
separate from the reusable KatlOS image.

## Verification

`katlos-install` validates the image before destructive disk mutation.

Required order:

```text
resolve the KatlOS image from the manifest or local media
verify the top-level image SHA-256 against the manifest or trusted local metadata
mount the image read-only
load /katlos/image.json with unknown fields rejected
validate apiVersion, kind, imageRole, architecture, and runtimeInterface
verify each required component exists
hash each component byte range from the mounted image and compare size/SHA-256
resolve the manifest Kubernetes version to exactly one bundled sysext component
validate runtime root, boot asset, bundled sysext candidate, and compatibility
  metadata as one set
only then repartition, write root slots, install boot assets, and create the
  generation 0 record
```

Failure behavior:

```text
missing image, unreadable image, or top-level digest mismatch
  fail before mounting or mutating disks

invalid index, unknown required role, missing component, or component digest
  mismatch
  fail before mutating disks

missing, duplicate, or malformed bundled sysext for the manifest Kubernetes
  version
  fail before generation 1 preparation; when detected during install-image
  verification, fail before mutating disks

architecture or runtime/sysext compatibility mismatch
  fail before mutating disks

write or post-write verification failure
  leave the system unbootselected for the failed generation and report the
  failed phase, selected disk, component role, and digest context
```

The installer must never derive generation spec/status from unverified component
metadata. The generation spec records the validated component digests and
compatibility fields, not the original download URL as authority.

## Trust And Secrets

The first implementation requires SHA-256 verification. Signing and encryption
are deferred but the schema leaves room for them through adjacent publication
metadata and future index fields.

Deferred trust questions:

```text
signature envelope format for published KatlOS images
trust root distribution for installers and updaters
whether encrypted payloads are needed for private node application bundles
how revocation metadata is distributed
whether OCI distribution should wrap the same SquashFS payload later
```

Secret rules:

```text
node identity, SSH host keys, kubeadm tokens, certificate keys, and cluster
  bootstrap secrets are not stored in KatlOS images

node-specific network addresses and systemRole choices are not stored in KatlOS
  images

credentials for fetching a private image are supplied by installer input or a
  protected local channel, not embedded in the reusable image
```

## Follow-Up Work

Existing follow-up work covers the implementation path:

```text
build single KatlOS install image

update install manifest schema for one KatlOS image reference

consume single KatlOS install image in installer

define and implement single KatlOS upgrade image apply path

verify install and upgrade consume one image

define installer boot artifact variants that point at the KatlOS image

verify PXE boot can supply config without image rebuild
```

These are sufficient follow-ups for this decision. No new tracked work is
required for the contract itself.
