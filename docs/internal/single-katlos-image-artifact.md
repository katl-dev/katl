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
```

The runtime-root component metadata must include the runtime version,
architecture, runtime interface, root artifact digest, minimum root slot size,
filesystem format, and any required root filesystem feature metadata.

Kubernetes sysext payloads are distributed separately as Katl Kubernetes payload
bundles. Their metadata uses the compatibility vocabulary defined in
`docs/internal/installer-runtime-design.md`: artifact identity, systemd
extension identity, runtime compatibility, Kubernetes tooling versions,
supported kubeadm config API families, upgrade constraints, host prerequisites,
and provenance. The install image index must not imply that Kubernetes sysext
candidates are embedded in the KatlOS image.

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
static metadata needed to write generation spec/status and boot entries
component digests and compatibility metadata
```

Node-specific configuration remains outside the image. The install manifest,
PXE/preseed input, USB/local handoff, or VM harness supplies identity, disk
selection, network configuration, bootstrap profile references, systemRole, the
exact Kubernetes payload version, and other supported day-one node
configuration. Capability overlays are a day-2 design item. Reusing the same
install image for multiple nodes must not require rebuilding the image.

Generation 0 uses the runtime root, boot assets, baseline metadata, and baseline
configuration needed to boot KatlOS. Kubernetes sysexts are not image components
and are not generation 0 selected or active state. `katlctl cluster bootstrap`
later asks `katlc` to fetch and verify a user-supplied HTTPS Kubernetes payload
bundle, stage the sysext locally, and create the first Kubernetes-capable
generation with the staged sysext whose payload version exactly matches the
manifest intent.

The installer material model therefore changes from:

```text
install manifest -> runtimeRoot URL + UKI URL + sysext URLs
```

to:

```text
install manifest -> KatlOS install image URL/ref/digest + Kubernetes bundle source/ref
KatlOS install image -> generation 0 runtime payload only
katlctl cluster bootstrap -> katlc fetches HTTPS payload bundle and selects staged katl-kubernetes-v1.36.0-x86_64.sysext.raw
```

## Upgrade Image

An upgrade image uses the same `KatlOSImage` schema with `imageRole: upgrade`.
It contains the payload needed to create a candidate generation from one
user-facing artifact.

Representable upgrade image shapes:

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
current image contract. These shapes are representable in the image model, but
Kubernetes upgrade execution remains disabled until the target kubeadm access
mode and kubelet activation gate are selected, implemented, and tested.

This is an explicit upgrade sequencing requirement, not an image-schema feature
and not a kubeadm replacement. If the target Kubernetes sysext contains both
`kubeadm` and `kubelet`, the upgrade path must provide a pre-kubelet gate: the
target `kubeadm` binary must be available for the operator, test harness, or a
future kubeadm-aware node-local `katlc` operation before target
`kubelet.service` is started or restarted from that same payload. The mechanism
is intentionally deferred. Valid directions include service ordering, a held
kubelet start condition, temporary target-kubeadm exposure, or a split kubeadm
tool payload. The image contract only requires that the ordering be representable
and testable.

## Node Configuration References

PXE/preseed, USB/local handoff, and VM tests all use the same node configuration
model after input discovery. Loose-artifact deployment points to one KatlOS image:

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

The versioned release ISO places exactly one KatlOS install image and its media
descriptor on the ISO filesystem. When `katlosImage` is absent, the installer
binds the node configuration to that embedded image. Local handoff may still
post an explicit local or network image reference. In all cases, node-specific
configuration remains separate from the reusable KatlOS image.

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
record the manifest Kubernetes version or catalog ref as bootstrap intent
validate runtime root, boot asset, and compatibility metadata as one set
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

missing or malformed Kubernetes payload source/ref
  does not block generation 0 image verification; fails later when `katlc`
  prepares generation 1 unless the bootstrap operation supplies a valid HTTPS
  source/ref

architecture or runtime interface compatibility mismatch
  fail before mutating disks

write or post-write verification failure
  leave the system unbootselected for the failed generation and report the
  failed phase, selected disk, component role, and digest context
```

The installer must never derive generation spec/status from unverified component
metadata. The generation spec records the validated component digests and
compatibility fields, not the original download URL as authority.

VM install and upgrade proof artifacts record a `KatlOSSingleImageProof`
report. The report names the one user-facing image path or reference, the
embedded `katlos/image.json` identity, and each verified component
role/path/digest. Upgrade smoke tests that use systemd-sysupdate must derive the
local/offline source files from the verified runtime-root and runtime-UKI image
components, verify those source digests against the image index, and record the
transfer definitions that consume them. Tests must fail when an install manifest
or upgrade proof relies on loose user-facing runtime-root, UKI, or sysext
component inputs instead of component metadata embedded in the KatlOS image.

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
