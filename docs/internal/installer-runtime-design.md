# Installer Image, katlos-install, and Runtime OS Design

Status: working design.

This document defines the first concrete split between the temporary installer
environment, the installer application, and the installed runtime OS.

## Naming

Use these names consistently:

```text
Katl
  The project.

katlc
  The user-facing KatlOS state/configuration command. It accepts user-supplied
  Katl YAML or configuration, validates it, compiles it into generation-scoped
  sysext/confext payloads and metadata, and applies, stages, reports, or rolls
  back runtime state.

installer-image
  The temporary boot environment built with mkosi. It bootstraps a target node
  and runs katlos-install.

katlos-install
  The Go installer application that runs inside installer-image.

KatlOS runtime
  The installed, persistent OS composition that boots from the target disk.
  It is a pared down, tightly configured Linux system prepared for kubeadm and
  kubelet through the selected Kubernetes sysext, not a bespoke distribution.
```

`installer-image` is built by mkosi. It should not normally contain mkosi. mkosi
is a build-time tool used by developers, CI, or a build container. The installer
consumes prebuilt runtime artifacts instead of building the OS on the target
node.

The runtime OS is not an upstream distribution, a separate package universe, a
Talos-style custom appliance, or a custom userspace from scratch. It is a
Fedora-derived system image that Katl trims and configures: kernel and initramfs,
systemd userspace, basic networking and storage tools, SSH, a container runtime,
Katl-owned units/agents, and the host components needed before kubelet and
kubeadm are provided by the selected Kubernetes sysext.

KatlOS exists after Katl installs a generation. The initial implementation
should think of the runtime root as a Fedora-derived image assembled from
artifacts, with KatlOS state managed by generated sysext/confext generations,
not as a user-facing OS generator or a new package repository.

## Component Boundaries

```text
katlc
  validates user-supplied Katl YAML/configuration, compiles generation-scoped
  sysext/confext payloads and metadata, and applies or stages runtime state

mkosi
  builds installer-image, runtime root artifacts, UKIs, and sysexts

installer-image
  boots the target node into a controlled live environment

katlos-install
  applies the install manifest to the target disk and writes the runtime OS

KatlOS runtime
  runs the node after install, reaches generation 0 installed-runtime health, and
  later supports explicit bootstrap, upgrade, repair, and recovery operations
```

The installer path must not become a whole-disk image clone. `katlos-install`
owns the target disk layout, formats filesystems on the real device, writes
bounded immutable artifacts into root slots, and creates boot metadata.

## installer-image

`installer-image` is a minimal live OS. For the initial implementation it should
be Fedora-derived because Fedora packages modern systemd, mkosi already supports
it well, and keeping the installer and runtime OS on the same base reduces early
compatibility problems.

The image is allowed to be more convenient than the runtime OS, but it should
still be purpose-built. Its default job is to boot, configure enough network and
storage, start SSH for debugging when configured, and run `katlos-install`.

For the current installer path, the shipped installer boot artifact should be a
single installer UKI. That UKI should contain the installer kernel, initrd,
embedded command line, and the installer userspace needed to run
`katlos-install`. Local VM tests, PXE chains, and later ISO/USB wrappers should
all be able to consume that same UKI artifact. Signing and additional wrapper
artifacts can come later; the current step should prove one bootable UKI with the
installer inside it.

The default installer should include:

```text
kernel and initramfs
systemd, udev, journald
systemd-networkd and systemd-resolved
installer input discovery and local handoff support
openssh-server
ca-certificates
curl
util-linux, lsblk, blkid, wipefs, findmnt, mount, umount
systemd-repart
dosfstools
e2fsprogs
squashfs-tools, only if needed for validation/debugging
bootctl and systemd-boot assets
katlos-install
artifact verification tool or embedded verifier support
small debugging tools useful during bring-up
```

The default installer should not include:

```text
mkosi
dnf as an operational install dependency
Kubernetes components
container runtime
large debug suites
build toolchains
project logic in shell hooks
```

Extra debug packages can be added to a debug installer profile later. They
should not be required for the normal install path.

## Installer Materials

The installer can receive materials in three ways:

```text
network install
  user-managed PXE/iPXE or another network boot path boots installer-image and
  passes install manifest data or enough metadata for katlos-install to fetch it.

offline install
  ISO/USB boots installer-image with a bundled material set.

local handoff install
  ISO/USB or VM boots installer-image without a preseeded manifest; katlos-install
  starts a small HTTP server and waits for the initial install manifest to be
  supplied by an operator or local test harness.
```

The material set contains:

```text
install manifest
installer input metadata
runtime root artifact
runtime UKI or kernel/initramfs assets
systemd-boot entry templates or generation metadata
sysext artifacts
Katl configuration domain inputs
checksums and signatures for fetched artifacts
optional artifact trust policy once signing is introduced
```

For network installs, the material set may be fetched from any user-managed
HTTP source. For ISO installs, the material set should be embedded in the image
or adjacent on the boot media.

## Local Config Handoff

Local testing and hands-on bare-metal installs need a path that does not depend
on PXE, matchbox, or pre-rendered kernel parameters. This also supports workflows
where a remote KVM device boots a mounted installer ISO and the operator applies
machine configuration afterwards.

If no install manifest is supplied by kernel command line,
embedded media, or a known local path, `katlos-install` should enter a waiting
mode instead of failing immediately or mutating disks.

In waiting mode, `katlos-install` should:

```text
bring up installer networking
start a small HTTP server
print the installer IP address, URL, and one-time token to console and journal
serve a read-only status endpoint
accept exactly one install manifest submission
validate the submitted manifest before any destructive action
stop accepting config once installation starts
```

Initial HTTP API shape:

```text
GET  /healthz
GET  /v1/status
POST /v1/install
```

`POST /v1/install` should accept the same install manifest used by preseeded
network installs. The API must not introduce a separate configuration model.

The handoff mode should require a one-time token by default. For local VM tests
tests, the token can be captured from the serial log. A deliberately insecure
test-only mode may exist, but it must be explicit.

This mode is only for supplying initial installer input. It is not a long-lived
runtime management API and it must not continue running after install begins.

## Network Boot Input Contract

Katl should not own how a machine is provisioned into the installer. Users may
use matchbox, hand-written iPXE, DHCP/TFTP tooling, a USB stick that chainloads
network assets, or another local boot process.

Katl owns:

```text
installer-image artifacts
installer input contract
install manifest schema
runtime artifacts
artifact metadata and verification inputs
```

Katl does not own:

```text
DHCP configuration
TFTP services
iPXE hosting
matchbox groups or profiles
firmware boot order management
site provisioning workflow
```

The first network boot flow should therefore be generic:

```text
1. User-managed boot infrastructure boots installer-image.
2. Boot configuration passes an install manifest URL, embedded manifest, or
   enough Katl boot metadata for katlos-install to discover install input.
3. installer-image boots the installer UKI and starts katlos-install.
4. katlos-install configures the live installer environment enough to fetch or
   accept install input.
5. katlos-install downloads/verifies artifacts, prepares the target disk, lays
   out the runtime OS, installs boot metadata, and reboots.
6. The machine boots from the installed disk, assuming firmware boot order is
   already correct or katlos-install successfully creates an EFI boot entry.
```

Any PXE/iPXE/matchbox examples should stay documentation-only unless the project
explicitly expands scope. Examples should show the boot input contract, not make
Katl responsible for provisioning. A boot configuration may pass:

```text
katl.manifest.url=<install manifest URL>
katl.install.mode=auto
console=...
```

The durable install policy should be in the install manifest consumed by
`katlos-install`, not embedded in PXE/iPXE/matchbox logic.

## Installer Input Role

The installer input contract is owned by `katlos-install` and the install
manifest schema. Katl uses its own typed input model rather than delegating
installer or runtime configuration to an external first-boot configurator.

Installer input may configure:

```text
hostname for the live installer environment
networking needed to fetch install materials, including static networkd config
DNS, NTP, proxy, and CA trust needed by the live installer
installer manifest URL or embedded install manifest
artifact base URLs or mirror hints, if not already in the manifest
autoinstall, hold-for-debug, or wait-for-config flags
temporary files consumed by katlos-install under /etc/katl or /run/katl
non-target device discovery or site-specific installer environment setup
```

Installer input must not bypass `katlos-install` ownership of durable install
actions:

```text
root disk partitioning policy
formatting or mounting persistent node disks as an action
runtime artifact installation
bootloader installation
steady-state runtime configuration
```

The manifest may select the target disk and authorize destructive install. Once
selected, the root disk belongs to Katl. Root disk partitioning, root slot
filesystems, state filesystem, labels, alignment, and sizing policy are Katl
implementation details, not user-configurable install policy. Boot metadata,
embedded media, or local handoff may deliver the manifest or write it to disk
for `katlos-install`, but `katlos-install` is the component that validates and
applies it. The user-facing install policy includes:

```text
target root disk selector
destructive install permission
extra data disk selectors
extra data disk filesystems
extra data disk mount points
```

## Install Manifest

The install manifest is the durable user input consumed by `katlos-install`.
Kernel arguments, embedded media, user-managed network boot metadata, and local
handoff mode may all deliver the manifest, but they must not define a separate
install policy model.

The initial schema is versioned as:

```text
apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
```

The schema lives at:

```text
docs/internal/schemas/install-manifest-v1alpha1.schema.json
```

A minimal manifest example lives at:

```text
docs/internal/examples/minimal-install-manifest.json
```

The v1alpha1 manifest contains these top-level sections:

```text
node
  hostname and `katl` runtime SSH authorized keys
  exact Kubernetes payload version and optional kubeadm config reference

install
  destructive install guard, target root disk selector, and optional extra data
  disks

katlosImage
  one KatlOS install image reference with required digest, size, version,
  architecture, and role metadata
```

`node.kubernetes.version` is an exact Kubernetes payload version such as
`1.36.1`. It is not a version range, resolver expression, catalog reference, or
compatibility policy. The first implementation resolves it only against bundled
Kubernetes sysext components inside the verified KatlOS image.

The current manifest deliberately does not expose a separate manifest name,
metadata labels, user-chosen generation IDs, node matching selectors, SSH
enable/disable policy, installer SSH overrides, artifact trust roots, bootloader
policy, loader entry names, kernel arguments, or extra disk mount options. The
hostname under `node.identity.hostname` is the only per-node identity field.
Those can be added later through explicit design when there is a concrete
implementation need.

The manifest is intentionally explicit about destructive installation:

```text
install.allowDestructiveInstall: true
```

Missing, false, or null values must fail validation before disk mutation. This
guard is separate from any `katl.install.mode=auto` boot hint; autoinstall may
start the state machine, but the manifest must still authorize destructive disk
changes.

Target disk selectors must prefer stable hardware identity such as
`/dev/disk/by-id`, WWN, or serial number. Short kernel device names such as
`/dev/sda` are not valid manifest selectors because they are not stable enough
for destructive operations.

The schema can validate required fields, value shape, safe enum values, exact
duplicate arrays, and reserved mount path syntax. `katlos-install` must add
semantic validation for facts that need hardware discovery or normalized path
comparison:

```text
target disk
  selector must resolve to exactly one whole disk, must not be read-only, and
  must satisfy install.targetDisk.minSizeMiB when set

destructive guard
  install.allowDestructiveInstall must be true before wipefs, repartitioning,
  formatting, or root slot writes

root slots
  Katl's selected root-a and root-b sizes must both fit the runtime root
  artifact, must be large enough for update headroom, and must leave enough room
  for ESP, optional XBOOTLDR, optional etcd, and minimum state partition size

state partition
  Katl's state partition policy consumes remaining disk after fixed partitions;
  minimum state size must be satisfied after planning

katlosImage
  the manifest references one KatlOS install image rather than loose runtime
  root, UKI, or sysext artifacts; the image reference must have a URL or local
  ref, SHA-256 digest, size, version, architecture, and role; digest mismatches
  fail before mutation where possible and before boot metadata is installed;
  signing and external trust-root policy are deferred

Kubernetes payload version
  node.kubernetes.version must be an exact version such as 1.36.1; the verified
  KatlOS image must contain exactly one bundled Kubernetes sysext component for
  that payload version, such as katl-kube-1.36.1.sysext; missing, duplicate, or
  incompatible matches fail validation

SSH and identity
  at least one `katl` authorized key is required; SSH disablement and installer
  SSH overrides are deferred; machine-id is not user supplied in the current
  manifest;
  katlos-install generates a random machine-id during install and records it in
  persistent state and boot settings

extra disks
  selectors must not resolve to the target root disk or its partitions; mount
  paths must normalize under /srv or /var/lib/katl/extra; duplicate normalized
  mount paths, parent/child mount conflicts, and reserved paths such as /,
  /boot, /efi, /usr, /etc, /run, /tmp, /var, /var/lib/kubelet,
  /var/lib/containerd, and /var/lib/etcd must be rejected; custom mount options
  are deferred
```

Runtime first-boot seed material may configure:

```text
node identity seed
initial hostname
initial SSH access
activation pointers for generation 0 baseline confext and sysext artifacts,
when selected
first-boot marker files
```

Long-lived `/etc` configuration should come from generated confext generations.
The first-boot path is bootstrap material, not the steady-state configuration
manager.

## katlos-install

`katlos-install` is a Go application because the installer needs typed plans,
idempotent state transitions, testable validation, and clear command
boundaries.

It is responsible for:

```text
reading kernel command line and installer environment
starting local config handoff when no manifest is preseeded
loading the install manifest
selecting the node config
collecting hardware facts
validating target disk identity and size
validating extra disk identity, filesystem, and mount requests
verifying artifact signatures and digests
building an install plan
persisting install progress when the state partition exists
wiping signatures when explicitly allowed
creating the GPT partition table
creating or validating partitions
formatting writable filesystems
writing immutable runtime artifacts into root slots
installing systemd-boot
installing UKIs and loader entries
installing or caching bundled sysext artifacts without activating Kubernetes for
  generation 0
materializing generated confext from trusted manifest input
generating runtime mount units for /var, /etc/kubernetes, and extra disks
writing runtime seed data
writing install records under /var/lib/katl
verifying the final mounted layout
rebooting into the runtime OS
```

It is not responsible for:

```text
building the runtime OS with mkosi
running Kubernetes
running kubeadm
joining the cluster
long-term node updates
general configuration management
```

### State Machine

The installer should be structured as idempotent states:

```text
DiscoverInstallerInput
WaitForLocalConfig, when no manifest is preseeded
LoadManifest
SelectNode
CollectHardwareFacts
VerifyTrust
PlanInstall
PrepareDisk
CreatePartitions
FormatFilesystems
MountTarget
InstallRootSlot
InstallBootArtifacts
InstallExtensions
InstallSeed
InstallMountUnits
WriteInstallRecord
VerifyTarget
Reboot
```

Each state should have typed inputs and outputs. Command execution should sit at
explicit boundaries so unit tests can cover planning and validation without
booting a VM.

Before the state partition exists, progress is recoverable by inspecting the
target disk. After it exists, `katlos-install` should persist checkpoints under:

```text
/mnt/target/var/lib/katl/install/state.json
/mnt/target/var/lib/katl/install/manifest.json
/mnt/target/var/lib/katl/install/logs/
```

The installer should be safe to re-run against a partially installed target. It
may continue, repair, or refuse, but it must not silently destroy data unless
the manifest explicitly allows destructive install.

## Does katlos-install Use mkosi?

No. mkosi builds the artifacts before install time:

```text
runtime root artifact
runtime UKI
sysext artifacts
installer-image
```

`katlos-install` consumes those artifacts. It may call system tools such as
`systemd-repart`, `mkfs.*`, `mount`, `bootctl`, and `sfdisk` where appropriate,
but the installer logic and decisions live in Go.

The normal install flow is:

```text
build runtime artifact with mkosi
boot installer-image
verify runtime artifact
partition and format the real target disk
write the runtime artifact into a root slot
write boot and state metadata
reboot into the runtime OS
```

The installer should not install packages onto the target with `dnf`, and it
should not build a root filesystem on the target and then squash it.

## Runtime Root Artifact Contract

For the current install path, the runtime root artifact is produced on the build
side, before the machine boots the installer:

```text
mkosi builds the Fedora-derived runtime root tree
the build packages that tree as a SquashFS filesystem image
the build emits metadata next to the image
installer-image receives or fetches the image and metadata
katlos-install verifies and writes the image into root-a or root-b
```

The build-side mkosi profile should produce the runtime root from declared
packages and generated Katl units. The final SquashFS packaging step is also a
build-side operation. The artifact bytes and their digest must be fixed before a
target node begins installation. User-supplied Katl YAML/configuration is
compiled by `katlc` into sysext/confext generation state on KatlOS, not baked
into generic runtime artifact bytes.

The target machine must not assemble the Fedora runtime from packages during
install. It consumes a prebuilt, hashed artifact.

The first artifact format should be a SquashFS filesystem image. The root slot
partition contains the SquashFS image bytes directly, starting at byte offset 0.
It is not an ext4 filesystem containing a SquashFS file, and it is not an
extracted tree that gets squashed on the target.

Artifact metadata should record at least:

```text
format: squashfs
path or URL
size in bytes
sha256 digest of exactly the SquashFS artifact bytes
compression
generation id
target architecture
runtime version or build id
root filesystem feature requirements, if any
compatible boot artifact or UKI digest and command-line metadata
compatible sysext artifact digests and generated confext generation metadata
minimum root slot size
created timestamp
```

The install manifest should copy or reference this metadata. `katlos-install`
must treat manifest artifact metadata and fetched artifact metadata as the same
contract: mismatched URL, digest, size, generation id, architecture, or
compatible boot artifact metadata is a manifest/artifact mismatch and fails
before boot metadata changes.

`katlos-install` should write a root slot as a byte-for-byte artifact install:

```text
verify artifact digest before destructive actions
select root-a for first install, or the inactive slot for updates
verify the artifact size fits the selected partition
stream artifact bytes to the selected block device
flush the block device
verify the first artifact-size bytes on disk, or mount the slot read-only and
  validate filesystem metadata
render boot metadata only after the slot is verified
```

Verification must hash the artifact byte range, not the whole partition. Root
slot partitions are intentionally larger than many artifacts, and trailing bytes
after the SquashFS image are ignored. The preferred verification is:

```text
hash the fetched artifact while downloading or before mutation
write exactly size-bytes to the selected root slot
flush and reopen the block device
read exactly size-bytes from offset 0
hash that byte range and compare with metadata.sha256
optionally mount the slot read-only with rootfstype=squashfs and inspect
  filesystem metadata as an additional validation, not as a replacement for the
  byte-range digest check
```

Trailing partition bytes are not part of the artifact digest. `katlos-install`
may leave old trailing bytes in place after writing a smaller artifact, but boot
metadata and verification must never depend on those bytes. A later hardening
step can zero the remainder of the partition if tests show that it is useful for
forensics or reproducibility.

This layout works across 512e and 4Kn disks as long as Katl creates aligned GPT
partitions, for example on 1 MiB boundaries. SquashFS is a filesystem image at
the start of the partition, and the kernel block layer handles the device
logical sector size. The installer should use normal buffered file IO or
otherwise respect block-device alignment; it should not make correctness depend
on fragile `O_DIRECT` assumptions.

The boot entry for a generation should point at the selected slot explicitly:

```text
root=PARTUUID=<selected-root-slot-partuuid> rootfstype=squashfs ro
```

The boot metadata points at the root slot partition, not at an artifact URL or
file path. Generation metadata under `/var/lib/katl/generations/` records the
artifact URL, digest, selected slot, selected slot PARTUUID, UKI path, kernel
command line, and extension set that were verified together.

The target node explicitly does not:

```text
run mkosi
install Fedora packages with dnf
assemble a runtime root tree from packages
create the SquashFS runtime image
modify the SquashFS contents after verification
derive boot metadata from an unverified artifact
```

Open questions before implementation proceeds:

```text
Should the build emit artifact metadata as a standalone JSON document, as part
  of the install manifest, or both?

Should slot write verification always perform both byte-range hashing and a
  read-only SquashFS mount, or should the mount check be a debug/integration
  gate only?

Should the installer zero unused trailing bytes in root slots for
  reproducibility, or leave them alone to keep writes bounded to artifact size?

Should UKI compatibility be represented as a direct UKI digest, a boot metadata
  digest, or a generation record digest once update signing is introduced?
```

## Generated Confext Contract

Users should not supply sysext/confext images directly in the default
configuration path. Users supply Katl install manifests and Katl
YAML/configuration in known domains. Katl materializes that input into
generation-scoped sysext/confext state.

Configuration apply is node-local. The input handed to the installer or runtime
state path is Katl YAML/configuration; KatlOS validates that input and renders
the generation-scoped extension state itself. Generated confext is built locally
for that generation. Sysext payloads are prebuilt artifacts, but their selected
activation set is recorded with the same generation as the rendered confext.
Bundled Kubernetes sysexts from the install image are available payloads until a
generation selects one; they are not active merely because the installer verified
or cached them.
`katlc` and KatlOS runtime services must reject unknown or unsupported
configuration before rendering anything. Unsupported domains, fields, sysext
selections, apply modes, or raw extension paths are validation failures, not
ignored input.

The configuration API should be small and explicit. It is not an arbitrary
`/etc` passthrough. Each supported domain defines:

```text
user-facing input shape
native file syntax, when passthrough is useful
rendered target path under /etc
validation rules
runtime apply, reload, or restart behavior
```

For example, a `networkd` domain can allow native `.network`, `.netdev`, and
`.link` file content while Katl owns rendering those files under
`/etc/systemd/network/` and applying changes with `systemd-networkd` and
`networkctl`.

For the initial install, `katlos-install` owns this conversion:

```text
read validated install manifest
validate known configuration domains and their output paths
render domain configuration into a generation-scoped confext tree or image
write extension-release metadata
stage the confext under /var/lib/katl/generations/<generation-id>/
select that confext with the same generation metadata as the root slot and any
  generation 0 sysext set
```

Generated confext must be switched with the selected generation. It must not
drift independently from the selected root slot, UKI, boot metadata, or sysext
set.

`katlc` performs the same logical generation-build operation for an already
installed node, but the first Kubernetes sysext activation is part of explicit
cluster bootstrap. The first Kubernetes-capable generation flow is:

```text
katlctl cluster bootstrap asks katlc to validate stored install intent
katlc selects the bundled Kubernetes sysext whose payload version exactly matches
  node.kubernetes.version, such as katl-kube-1.36.1.sysext
katlc renders known configuration domains into generation 1 confext
katlc writes candidate generation metadata and activates it for kubeadm readiness
katlctl runs kubeadm init or join
katlc commits the candidate generation only after kubeadm and health checks pass
```

Later host configuration changes can use normal `katlc` generation apply or
stage flows. Later Kubernetes sysext transitions require explicit upgrade
operations, because kubeadm must own the cluster mutation:

```text
katlc receives desired Katl YAML/configuration
katlc validates trust and policy
katlc renders known configuration domains into a new generated confext generation
katlc may preserve the current Kubernetes sysext for ordinary host config changes
katlc rejects Kubernetes sysext changes unless an explicit upgrade operation owns
  the kubeadm handoff
katlc records success, failure, diagnostics, and rollback metadata
```

`katlc` and KatlOS runtime services should not become a general-purpose
configuration management system. They apply Katl-generated configuration
generations while preserving native systemd/Linux file semantics.

## Runtime OS Composition

The runtime OS should also be Fedora-derived initially. The goal is not to
expose Fedora as the product surface, but Fedora gives Katl a practical package
source for a modern systemd-native runtime image.

The runtime should contain the smallest practical set of packages and generated
units needed for an explicit bootstrap operation to create a kubeadm-ready
generation:

```text
kernel and initramfs tooling needed for the target boot model
systemd
systemd-udev
systemd-networkd
systemd-resolved
systemd-timesyncd
systemd-sysext
systemd-confext
systemd-tmpfiles
systemd-sysctl
systemd-modules-load
ca-certificates
util-linux
iproute
kmod
nftables or other required packet filtering base
openssh-server
containerd
runc or crun
katlc
katl node/update services, when they exist
katlctl, when it exists
```

This is the core of the runtime OS: kernel, systemd, SSH access, and enough host
OS to run kubelet/kubeadm correctly and repeatably. It is not expected to look
like a general-purpose Fedora Server install, but it should remain recognizable
and debuggable as a normal Linux system.

SSH should be available on the installed runtime for this project audience. This
is an intentional part of the operating model, not just a recovery escape hatch.
The default policy should be key-only access, no password login, and generated
systemd/sshd configuration rather than an ad-hoc mutable setup.

Katl owns host user and SSH policy. Users may supply SSH public keys, but they
do not define Linux users, sudo policy, PAM policy, sysusers snippets, or sshd
policy files. The runtime host identity model is:

```text
root
  password locked; no SSH login

katl
  the only SSH login account; key-only authentication

package/system users
  created by required base packages

katl-agent
  optional later no-login service identity if KatlOS runtime services need one
```

Katl should render the `katl` account, authorized keys, and sshd policy. The
generated policy should include no password authentication, no root login, and
`AllowUsers katl` or an equivalent restriction. User-supplied generated confext
input must not write account or authentication control files such as
`/etc/passwd`, `/etc/shadow`, `/etc/group`, `/etc/gshadow`, `/etc/sudoers*`,
`/etc/pam.d/*`, `/etc/security/*`, `/etc/subuid`, `/etc/subgid`,
`/etc/sysusers.d/*`, or `/etc/ssh/sshd_config*`.

A booted generation is assembled from these layers:

```text
runtime root artifact
  read-only base root containing systemd, host plumbing, container runtime,
  Katl-owned units, SSH configuration, and mount/update scaffolding

UKI and boot metadata
  kernel, initramfs, command line, systemd-boot entry, and boot attempt policy

Kubernetes sysext
  kubelet, kubeadm, kubectl, and closely related binaries; versioned
  independently from the KatlOS runtime root

generated confext
  node and role configuration under /etc

writable state
  one writable state partition mounted at /var, plus explicit systemd bind
  mounts only for persistent paths that need to appear outside /var
```

Kubelet service ownership needs to stay test-driven. The likely starting point
is that the base root carries the unit skeleton and ordering that Katl controls,
the Kubernetes sysext carries binaries, and confext carries node-specific
configuration and drop-ins.

The runtime image should not include:

```text
mkosi
dnf as a runtime management interface
build toolchain
Kubernetes add-ons
Helm
Flux
Cilium
CoreDNS
Rook
large debug tools
application workloads
```

Kubernetes binaries should initially be delivered as a sysext unless boot tests
show that kubelet ordering or operational simplicity is better with them in the
base root artifact.

## Kubeadm-Ready Runtime

This section describes the first Kubernetes-capable candidate generation created
when `katlctl cluster bootstrap` asks `katlc` to validate stored intent and
prepare the node for kubeadm. It is not generation 0 first boot. Kubeadm
readiness is produced by normal generation creation and activation that selects
the Kubernetes sysext and generated kubeadm input; it is not a separate
Kubernetes operation.

The next runtime step after the installer UKI can install and boot from disk is
kubeadm readiness, not a complete Kubernetes cluster. Katl should prove that an
installed node has the host OS, writable state, generated config, and Kubernetes
binaries required for an operator or later automation to run `kubeadm init`.

For the first implementation, kubeadm readiness means:

```text
runtime root provides systemd, networking, time sync, SSH, containerd, OCI runtime,
  sysctl/modules-load/tmpfiles scaffolding, and Katl-controlled units
Kubernetes sysext provides kubeadm, kubelet, kubectl, and closely related CLI
  or helper binaries needed for preflight and node bootstrap
selected generation metadata records the Kubernetes sysext artifact, digest,
  activation path, and compatibility metadata
systemd-sysext activates only the selected generation's Kubernetes sysext
generated confext renders selected native kubeadm input under /etc/katl/kubeadm
/etc/kubernetes is a writable bind mount backed by
  /var/lib/katl/kubernetes/etc-kubernetes
containerd is running before the kubeadm-ready target
kubelet binary and service wiring are present, with final start/enable policy
  kept test-driven
katl-kubeadm-ready.target is reached only after the required local prerequisites
  are active
```

The kubeadm input API is a thin reference to native kubeadm YAML, not an
init/join action embedded in node configuration. The focused decision is
recorded in `docs/internal/kubeadm-config-input-design.md`.

The Kubernetes sysext is a Katl artifact produced by mkosi from declared package
inputs. In early development it can be built locally. Later CI can publish the
same artifact shape for users to download, but CI publishing is not part of the
current local loop.

For first install, the KatlOS image bundles exact-version Kubernetes sysext
artifacts, for example `katl-kube-1.36.1.sysext`. The install manifest requests
the exact Kubernetes payload version with `node.kubernetes.version: "1.36.1"`.
`katlctl cluster bootstrap` asks `katlc` to select the matching bundled sysext
for generation 1 and record its path, digest, payload version, activation path,
and compatibility metadata in generation metadata. A day-one install does not use
a version range, remote Kubernetes catalog, or compatibility matrix resolver.
Those are day-2 update planning concerns.

Kubernetes sysext versioning must stay decoupled from the installed KatlOS
runtime root version. Users should be able to keep their current Kubernetes
minor version while upgrading KatlOS, or upgrade Kubernetes while keeping the
same KatlOS runtime root, when the selected artifact pair is supported. A booted
generation still activates one exact runtime root plus one exact Kubernetes
sysext set so boot health and rollback remain atomic.

The Kubernetes sysext artifact metadata should include:

```text
sysext name
sysext artifact version or build id
Kubernetes version carried by the artifact
architecture
digest and size
runtime compatibility metadata
```

The runtime root artifact metadata should expose enough compatibility identity
for sysext validation, such as a KatlOS version and a runtime interface or
extension ABI version. The compatibility check should happen before
`katlos-install` or the runtime update agent writes a candidate generation as
bootable. Unsupported runtime/sysext pairs fail validation rather than relying
on boot-time discovery.

Valid update shapes include:

```text
KatlOS-only update
  new runtime root and UKI, existing Kubernetes sysext if compatible

Kubernetes-only update
  existing runtime root and UKI, new Kubernetes sysext if compatible

combined update
  new runtime root, UKI, and Kubernetes sysext validated as one set
```

Kubernetes version upgrades have one additional local activation gate. A
candidate generation that selects a target Kubernetes sysext must not activate
the target kubelet until the upgrade workflow has had access to target `kubeadm`
and has recorded the explicit handoff for kubeadm upgrade work. Katl's role in
this gate is OS-side ordering and evidence; kubeadm remains the tool that
performs Kubernetes upgrade actions and mutates Kubernetes node or cluster state.

Tests should be layered rather than waiting for a full VM flow:

```text
package or artifact tests inspect the sysext for expected binaries,
  extension-release metadata, and excluded add-ons
unit or golden tests cover generated service ordering, mount units, and
  generation metadata for sysext selection
systemd-analyze verify checks generated units where practical
VM install-to-runtime tests prove generation 0 boots from disk, mounts /var,
  activates baseline extensions/config, exposes operator access, and reaches
  installed-runtime health
VM bootstrap tests prove `katlctl cluster bootstrap` asks `katlc` to create and
  activate generation 1, selects the manifest-requested Kubernetes sysext,
  exposes writable /etc/kubernetes, reaches katl-kubeadm-ready.target, runs
  kubeadm, and commits only after kubeadm and health checks succeed
```

The readiness check should avoid implying that Katl owns cluster lifecycle. Katl
prepares the node for kubeadm; kubeadm and user-managed GitOps own the cluster
from that point.

## Disk Format

The default installed OS disk uses GPT and EFI-only boot.

Recommended initial layout:

```text
esp
  type: esp
  filesystem: vfat
  mount: /efi
  mutable by Katl install/update tooling only

xbootldr
  type: xbootldr, optional
  filesystem: vfat initially
  mount: /boot
  contains UKIs and systemd-boot entry metadata
  mutable by Katl install/update tooling only

root-a
  type: root-x86-64 or root for the target architecture
  filesystem: squashfs image written directly into the partition
  mount: /
  size: fixed by profile
  immutable

root-b
  type: root-x86-64 or root for the target architecture
  filesystem: squashfs image written directly into the partition
  size: fixed by profile
  inactive root slot
  immutable

state
  type: var
  filesystem: ext4 initially
  mount: /var
  size: remaining disk after boot and root slots
  mutable persistent node state

etcd, optional
  type: linux-generic
  filesystem: ext4 initially
  mount: /var/lib/etcd
  mutable persistent control-plane state
```

The default storage model is intentionally simple:

```text
root-a and root-b
  fixed-size, read-only runtime slots

state
  the rest of the disk, formatted writable and mounted at /var
```

Applications that already write under `/var` should use the state partition
directly. This includes the expected defaults for kubelet, containerd, journald,
Katl state, and most host services. Katl should avoid inventing bind mounts for
normal `/var` paths unless a separate partition or special projection is needed.

Systemd mount units and tmpfiles rules should provide the stable storage view:

```text
var.mount
  mounts KATL_STATE at /var

optional var-lib-etcd.mount
  mounts a dedicated control-plane etcd partition at /var/lib/etcd

etc-kubernetes.mount
  bind mounts persistent kubeadm-owned state from /var/lib/katl/kubernetes
  into /etc/kubernetes
```

This keeps the immutable runtime root small while making writable application
state normal and predictable.

The initial writable state directory layout is recorded in
`docs/internal/writable-state-layout.md`.

For the first implementation, placing UKIs and loader entries on the ESP is
acceptable and may be simpler. If Katl uses XBOOTLDR, it should use a firmware
and `systemd-boot` readable filesystem by default. Use vfat initially rather
than ext4 unless Katl explicitly ships and validates the required UEFI
filesystem driver path.

Use labels and partition UUIDs to disambiguate slots:

```text
KATL_ESP
KATL_XBOOTLDR
KATL_ROOT_A
KATL_ROOT_B
KATL_STATE
KATL_ETCD
```

Because there are two root slots, boot entries should point at the selected root
partition explicitly with its partition UUID. Do not rely on generic root
auto-discovery to choose between `root-a` and `root-b`.

The generated kernel command line for a generation should be explicit:

```text
root=PARTUUID=<selected-root-slot-partuuid> rootfstype=squashfs ro
```

Katl should also mark inactive root slots with GPT attributes or boot metadata
that prevent accidental auto-selection. This needs to be tested with
`systemd-gpt-auto-generator`; agents should not depend on default root discovery
while two root partitions exist.

The initial root artifact should be a SquashFS filesystem image written into
the selected root partition. This keeps the root naturally read-only and makes
updates a bounded partition write instead of a whole-disk rewrite. Later designs
can evaluate EROFS, dm-verity, or a root image with separate verity partitions.

## Mutability Model

Immutable by default:

```text
/
/usr
runtime root slots
sysext artifacts
generated confext artifacts
booted UKI
systemd-boot entries outside Katl update operations
```

Mutable persistent state:

```text
/var
/var/lib/katl
/var/lib/kubelet
/var/lib/containerd
/var/lib/etcd
/var/log/journal, if persistent journald is enabled
```

The detailed path inventory is recorded in
`docs/internal/persistent-state-inventory.md`.

The writable state partition is the default home for application and node state.
Prefer native paths under `/var` over bind mounts when the application already
uses `/var`. Use bind mounts for paths outside `/var` that must be persistent,
most notably kubeadm-owned files under `/etc/kubernetes`.

Mutable projected state:

```text
/etc/machine-id
/etc/kubernetes
```

`/etc/machine-id` needs special handling because it is persistent identity but
the root and steady-state `/etc` are otherwise immutable. The base root should
ship an empty `/etc/machine-id` placeholder, and Katl must establish a stable
machine ID before normal services depend on it.

Candidate mechanisms:

```text
installer writes a per-node boot entry with systemd.machine_id=
initrd establishes /etc/machine-id from persistent state before systemd proper
an early mount path binds /var/lib/katl/identity/machine-id onto /etc/machine-id
```

Initial recommendation: `katlos-install` generates the stable machine ID during
install, records it under `/var/lib/katl/identity/machine-id`, and renders
`systemd.machine_id=<id>` into the installed generation's boot entry. That gives
PID 1 the identity before normal mounts or services run. Later work can move
this into the initrd if kernel command line exposure becomes unacceptable.

The generated value is random per install. It is not derived from hardware,
hostname, manifest content, or build inputs, and Katl does not preserve it across
a destructive reinstall. After install the backing file should be root-owned and
write-protected. It remains stable across normal runtime boots, root slot
switches, and updates because it lives on the writable state partition.

This must be proven in VM tests. A late normal service is not sufficient because
systemd and D-Bus consumers read machine identity early in boot.

Kubeadm-owned state under `/etc/kubernetes` must be persistent and writable,
but it must not make all of `/etc` mutable. Confext should not own this
subtree. Project it from:

```text
/var/lib/katl/kubernetes/etc-kubernetes
```

onto:

```text
/etc/kubernetes
```

with a systemd bind mount unit ordered before kubelet and kubeadm automation.
If confext overlays `/etc`, this bind mount should be validated after confext is
active so the overlay does not hide the persistent Kubernetes subtree.
The focused projection decision is recorded in
`docs/internal/etc-kubernetes-projection.md`.

Ephemeral state:

```text
/run
/tmp
installer runtime state before target /var exists
recovery overlays unless explicitly committed by repair tooling
```

Do not store persistent node identity or Kubernetes identity in `/run`.

## Boot And Update Metadata

Katl should store generation metadata under:

```text
/var/lib/katl/generations/
/var/lib/katl/boot/
/var/lib/katl/artifacts/
```

Do not place every installed sysext or confext directly in the global systemd
search paths. `systemd-sysext` and `systemd-confext` activate all unmasked
extensions they find in their default directories, which would mix generations
and break rollback.

Katl should store artifacts generation-scoped, for example:

```text
/var/lib/katl/generations/<generation-id>/sysext/
/var/lib/katl/generations/<generation-id>/confext/
```

At boot, Katl should expose only the selected generation's extensions to systemd,
for example by creating symlinks under:

```text
/run/extensions/
/run/confexts/
```

or another explicit activation mechanism that is proven in VM tests. Rollback must
switch the active root slot, active sysext set, and active confext set together.
A small Katl generation selector unit or systemd generator should recreate these
`/run` links each boot after persistent state is available and before
`systemd-sysext.service` and `systemd-confext.service` run.

Each generation record should include:

```text
generation id
runtime version
root slot
root partition UUID
root artifact digest
UKI path
sysext set, activation paths, and digests
sysext artifact versions, payload versions, architecture, and compatibility metadata
confext set, activation paths, and digests
kernel command line
created timestamp
boot attempt state
health state
```

The focused generation metadata decision is recorded in
`docs/internal/generation-metadata-model.md`.

The first install writes `root-a` and marks it pending. Runtime health
completion marks it good. Updates later write `root-b`, set it as the next boot
candidate with a bounded trial mechanism, and rely on Katl health state to
decide whether to promote or roll back. The first trial mechanism should keep the
previous known-good generation as the default boot entry and try the candidate
with systemd-boot one-shot selection or explicit boot counting. A candidate must
not become the permanent default until it reaches the boot-complete target.
The focused boot health decision is recorded in
`docs/internal/boot-health-semantics.md`.

`katlos-install` must render final loader entries on the target node because the
entries need final partition UUIDs, the install-generated machine-id value, boot
attempt naming, and generation metadata. Build-time assets may provide templates,
but not final installed entries.

## Runtime Mount Ordering

Generation 0 first boot should reach local filesystem and baseline extension
activation in this shape:

```text
root slot mounted read-only
/var mounted from KATL_STATE
optional /var/lib/etcd mounted
stable machine-id established by the chosen early-boot mechanism
baseline systemd-sysext activated, when selected
baseline systemd-confext activated
network online
time synchronized
installed-runtime health reached
```

A kubeadm-ready generation additionally requires:

```text
Kubernetes sysext activated
kubeadm config rendered under /etc/katl/kubeadm
/etc/kubernetes bind mounted from /var/lib/katl/kubernetes/etc-kubernetes
containerd running
kubelet available and, for Kubernetes upgrade candidates, gated until target
  kubeadm handoff is complete
katl-kubeadm-ready.target reached
```

Generated mount units and service ordering must be tested in VM tests because this
is where immutable root, confext, kubeadm, and systemd boot ordering intersect.
The target should be treated as a local preflight boundary. It must not require
Kubernetes API availability, add-ons, workload scheduling, or GitOps convergence.

## Open Questions

1. Should Kubernetes binaries start in the base image or in a sysext?

   Initial recommendation: sysext, unless kubelet ordering makes this painful.

2. Should `systemd-repart` be the only partitioning backend?

   Initial recommendation: compile to repart definitions where possible, but let
   `katlos-install` fall back to explicit partitioning commands for cases where
   repart is not expressive enough.

3. Should `etcd` always get a separate partition on control-plane nodes?

   Initial recommendation: support it as a profile option early, but keep the
   default VM layout simple until install tests are reliable.

4. Should the runtime root use SquashFS long-term?

   Initial recommendation: use SquashFS root slots first. Revisit EROFS and
   dm-verity after the install and update loop works.

5. Should SSH be enabled by default?

   Current decision: key-only SSH should be available for installer, runtime,
   and recovery profiles because the target users need practical debugging
   access. The only runtime SSH login account is `katl`; users supply keys, not
   host account policy.
