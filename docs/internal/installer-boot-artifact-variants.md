# Installer Boot Artifact Variants

Status: current decision.

This document defines the boot wrappers that start the Katl installer. These
artifacts are not KatlOS payload images. They boot a live installer environment
that runs `katlos-install`, discovers installer input, verifies one KatlOS image
payload, and applies that payload to the target disk.

The single KatlOS payload contract is defined in
`docs/internal/single-katlos-image-artifact.md`.

## Boundary

Installer boot artifacts are generic per Katl build, architecture, and installer
version. They must not be rebuilt for each node, cluster, address, target disk,
systemRole, bootstrap profile, bootstrap token, or KatlOS image URL.

They may contain:

```text
installer kernel
installer initrd
installer userspace
katlos-install
installer units and input discovery logic
CA trust and debugging tools selected for the installer profile
```

They must not contain:

```text
node identity
node addresses
SSH host keys
authorized user secrets
kubeadm tokens or certificate keys
cluster control-plane endpoint
target disk policy
KatlOS payload image bytes, except in later ISO/USB wrapper media
```

ISO and USB media may bundle a generic boot artifact plus a KatlOS payload image
for offline use, but the boot artifact itself remains a wrapper.

## Output Variants

Katl emits three first-class installer boot variants from the same installer
content:

```text
installer UKI
  EFI executable containing the installer kernel, initrd, and embedded default
  command line

split installer kernel/initrd
  Linux kernel plus initrd files suitable for PXE, iPXE, matchbox, or direct
  kernel/initrd VM boot through the supported VM runner

installer ISO
  UEFI El Torito wrapper around the installer UKI for optical media and
  ISO-backed virtual machines
```

Initial output names:

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

katl-installer-<version>-<arch>.iso
katl-installer-<version>-<arch>.iso.sha256
katl-installer-<version>-<arch>.iso.json
```

Local development may continue writing stable convenience paths under
`_build/mkosi/`, but published metadata and test artifacts should use the
versioned names.

## Metadata

Each boot artifact has adjacent metadata:

```text
apiVersion: katl.dev/v1alpha1
kind: InstallerBootArtifact
artifactRole: installer-uki | installer-kernel | installer-initrd
format: uki | linux-kernel | initrd
version
buildID
architecture
path or URL
sizeBytes
sha256
createdAt
installerInterface
defaultKernelCommandLine[]
supportedInputModes[]
```

The artifact index used by build scripts and VM tests must include these roles:

```text
installer-uki
installer-kernel
installer-initrd
installer-iso
```

Checksums cover the exact artifact bytes. Metadata must not contain host-local
Nix store paths, user home paths, or other machine-specific locations. Local
indexes may use repo-relative paths for development artifacts.

## Installer Input

All boot variants converge on the same validated install request before
destructive disk mutation. PXE/matchbox input, local handoff, and offline media
are delivery mechanisms, not separate install policy models.

Preferred network input:

```text
katl.manifest.url=<InstallManifest URL>
katl.manifest.sha256=<sha256>
katl.install.mode=auto
```

The referenced install manifest contains node-specific configuration and one
`katlosImage` reference. This keeps target disk policy, node identity, network
configuration, systemRole, bootstrap profile references, and KatlOS payload
selection in one typed document. Capability overlays are deferred to day-2
design. Split references for separate node configuration and payload URLs are
not part of the current installer input contract.

## Kernel Arguments

Safe kernel arguments:

```text
katl.manifest.url
katl.manifest.sha256
katl.manifest
katl.node
katl.artifact-base-url
katl.install.mode=auto|manual
katl.wait-for-config=0|1
katl.hold-for-debug=0|1
console=...
ip=...
```

Kernel arguments may include public URLs, digests, non-secret mode flags, and
standard Linux console/network boot parameters. They should not include inline
large manifests or arbitrary user configuration.

Forbidden kernel argument contents:

```text
private image credentials
SSH private keys
SSH authorized keys, unless explicitly accepted as public test-only material
kubeadm tokens
certificate keys
bearer tokens
target disk destructive policy as an action
inline node config containing secrets
```

Secret-bearing material must arrive through protected fetched input, local
handoff on a trusted provisioning network, offline media with an explicit trust
boundary, or a later dedicated secret channel.

## Variant Behavior

UKI behavior:

```text
boots directly from EFI, the supported VM runner, or an ISO/USB wrapper
uses embedded default command line for local handoff unless overridden
starts katlos-install in wait mode when no input URL/ref is supplied
```

Split kernel/initrd behavior:

```text
boots through PXE, iPXE, matchbox, or kernel/initrd VM boot
receives input refs and mode flags through kernel arguments
starts the same katlos-install units and state machine as the UKI variant
does not require rebuilding kernel or initrd when node config changes
```

ISO behavior:

```text
boots the same generic installer UKI through a UEFI El Torito boot image
uses the same manifest and KatlOS image input contract as the UKI
does not embed node-specific configuration or a KatlOS payload image
```

All variants must report the selected input mode, input source, boot artifact
versions, and redacted command line in diagnostics.

## Matchbox And PXE Boundary

Katl does not own DHCP, TFTP, iPXE hosting, matchbox groups, firmware boot order,
or site provisioning workflow. Katl owns only the boot artifacts and the
installer input contract.

Examples for matchbox or iPXE should show how to pass documented kernel
arguments and fetch the generic artifacts. They must not turn Katl into a
provisioning server or require checked-in site-specific paths.

## Follow-Up Work

Existing follow-up work covers the implementation path:

```text
build split installer kernel and initrd artifacts

verify PXE boot can supply config without image rebuild

update install manifest schema for one KatlOS image reference

consume single KatlOS install image in installer

define node install-to-bootstrap state machine after boot and payload input
contracts are stable
```

These are sufficient follow-ups for this decision.
