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
systemRole, kubeadm config, bootstrap token, or KatlOS image URL.

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

Katl emits two first-class installer boot variants from the same installer
content:

```text
installer UKI
  EFI executable containing the installer kernel, initrd, and embedded default
  command line

split installer kernel/initrd
  Linux kernel plus initrd files suitable for PXE, iPXE, matchbox, or direct
  kernel/initrd VM boot through the supported VM runner
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
katl.input.url=<InstallManifest URL>
katl.input.sha256=<sha256>
katl.install.mode=auto
```

The referenced install manifest contains node-specific configuration and one
`katlosImage` reference. This keeps target disk policy, node identity, network
configuration, systemRole, kubeadm config references, and KatlOS payload
selection in one typed document. Capability overlays are deferred to day-2
design.

Split reference input is allowed only as a convenience when a provisioning
system stores node config and payload refs separately:

```text
katl.node-config.url=<node config URL>
katl.node-config.sha256=<sha256>
katl.katlos-image.url=<KatlOS install image URL>
katl.katlos-image.sha256=<sha256>
katl.install.mode=auto
```

`katlos-install` must normalize split references into the same validated install
request shape used by the canonical install manifest path before it mutates any
disk. Split references must not create a second durable policy API.

## Kernel Arguments

Safe kernel arguments:

```text
katl.input.url
katl.input.sha256
katl.node-config.url
katl.node-config.sha256
katl.katlos-image.url
katl.katlos-image.sha256
katl.install.mode=auto|wait|debug
katl.debug.ssh=0|1
katl.debug.shell=0|1
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
handoff with an operator token, offline media with an explicit trust boundary,
or a later dedicated secret channel.

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

Both variants must report the selected input mode, input source, boot artifact
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
