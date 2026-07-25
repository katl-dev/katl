# ADR-013: KatlOS includes common firmware and CPU microcode

Status: proposed.

Date: 2026-07-25.

## Context

KatlOS is an immutable, generation-managed Kubernetes node OS. The existing
[v0.1 kernel and firmware support policy](../v0.1-kernel-firmware-support-policy.md)
retains broad in-tree kernel support in the installed runtime while
deliberately limiting the ephemeral installer to the modules needed for boot,
storage, wired networking and installation.

Kernel modules alone do not provide the corresponding hardware support. Modern
processors and devices also require redistributable firmware:

```text
Intel and AMD CPU microcode loaded before normal kernel initialization
Intel i915 and xe GPU firmware
AMD amdgpu firmware
general device firmware used by supported storage and network hardware
```

Leaving firmware to runtime package installation would conflict with the
immutable-host model. Making users select firmware packages in ClusterConfig
would expose Fedora packaging details as a Katl API and would produce
hardware-specific image variants which are difficult to test, update and
roll back.

Firmware and CPU microcode participate in the same compatibility relationship
as the selected kernel. A generation which combines a new kernel with firmware
from a prior generation, or restores a kernel without restoring its associated
firmware, is not a coherent rollback unit.

This decision applies initially to generic x86-64 KatlOS artifacts. It defines
packaged support, not hardware certification.

If accepted, this ADR supersedes the firmware and microcode portions of the
v0.1 policy. The earlier module-retention and installer-pruning boundaries
remain in effect.

## Decision

KatlOS includes common CPU microcode, redistributable device firmware and
in-tree kernel drivers in its immutable boot and runtime artifacts.

Generic x86-64 artifacts include support for both Intel and AMD systems. Katl
does not produce separate vendor images and does not expose firmware selection
through ClusterConfig.

Katl owns:

```text
the selected kernel and module set
the resolved redistributable firmware package set
early CPU microcode placement
runtime firmware placement
artifact compatibility and generation identity
release verification and rollback behavior
```

The kernel selects the processor microcode and device firmware appropriate for
the hardware present at boot.

### Goals

This decision provides:

```text
early Intel and AMD CPU microcode loading
runtime support for in-tree i915, xe and amdgpu drivers
redistributable firmware for claimed common hardware classes
kernel, initrd, microcode and firmware rollback as one generation
release failures when claimed firmware coverage is missing
generic x86-64 artifacts without hardware-specific operator configuration
```

### Non-goals

This decision does not provide:

```text
proprietary or out-of-tree drivers
hardware certification
user-supplied firmware
firmware-specific KatlOS flavors
runtime package installation
a general firmware sysext mechanism
specialized device firmware without a separate support decision
```

## Package Policy

The initial Fedora runtime package selection includes:

```text
kernel-core
kernel-modules
linux-firmware
microcode_ctl
amd-ucode-firmware
intel-gpu-firmware
amd-gpu-firmware
```

`microcode_ctl` supplies the Fedora-packaged Intel CPU microcode. The
`amd-ucode-firmware` package supplies AMD CPU microcode.

The installer package selection includes:

```text
linux-firmware
microcode_ctl
amd-ucode-firmware
```

GPU firmware is not required in the installer while GPU drivers remain outside
the installer module policy. If Katl later claims installer GPU support, the
corresponding driver and firmware must be added and verified together.

Package names are Fedora build inputs, not stable Katl APIs. Fedora may rename,
split or merge these packages. A package reorganization may change the concrete
selection without changing this decision when the resulting artifacts provide
the same claimed capabilities and redistribution remains permitted.

Resolved package identities and versions remain recorded as release evidence.
Changing the selected kernel or resolved firmware packages invalidates the
corresponding artifact build inputs.

## Artifact Placement

### CPU microcode

Intel and AMD CPU microcode is included in the early initrd embedded in every
installer and runtime UKI.

The microcode CPIO archive must precede the normal initramfs content in the
UKI's initrd payload. Finding microcode files somewhere in an extracted combined
initrd is insufficient: verification must prove that the kernel can consume
the archive during early microcode loading before normal driver and userspace
initialization.

Both vendor firmware sets may be present in one generic x86-64 UKI. The kernel
selects the update matching the current processor.

A change to selected CPU microcode creates a new boot generation and requires
a reboot. Microcode is never updated independently in the active generation.

### Device and GPU firmware

Device firmware is stored below `/usr/lib/firmware` in the immutable runtime
root.

The installed runtime retains the Fedora-provided kernel modules for:

```text
i915
xe
amdgpu
```

It also retains the redistributable Intel and AMD firmware families required
by those modules. Verification is based on package ownership and firmware
families rather than an exhaustive list of individual blob filenames.

If any device driver is included in an initrd, the firmware required for that
driver's claimed boot-time operation must also be included in that initrd.
GPU drivers remain excluded from the current ephemeral installer module set,
so installer GPU firmware is not presently a release requirement.

## Flavor Model

Stable-kernel and LTS-kernel KatlOS flavors use the same firmware policy:

```text
katlos-stable-x86_64
katlos-lts-x86_64
```

Katl does not produce separate Intel and AMD variants. Each flavor is rebuilt
and verified against its selected kernel, resolved module set and resolved
firmware packages.

The package set may resolve differently between flavors when their kernels or
Fedora inputs differ. The supported capability claim remains the same unless a
separate decision explicitly narrows a flavor.

## Update and Rollback

Firmware is versioned with the KatlOS runtime generation.

A selected generation records and activates a coherent set of:

```text
kernel
UKI and initrd
CPU microcode
immutable runtime root
device firmware
kernel modules
```

Changing the kernel, early-boot firmware or microcode rebuilds the UKI.
Changing runtime device firmware rebuilds the immutable runtime artifact. The
generation records the exact resulting boot and runtime resources even when a
particular change does not alter every resource.

Rolling back a generation restores the previous kernel, initrd, CPU microcode,
kernel modules and device firmware together. Firmware is not independently
updated in place and is not written into mutable state.

Build dependency tracking must invalidate and rebuild the runtime root and UKI
whenever their selected kernel, module or firmware inputs change. A release
must not reuse a boot artifact merely because the package name remained the
same while its resolved contents changed.

## Verification

Missing claimed firmware coverage is a release failure.

Verification proves artifact contents and supported packaging contracts. It
does not infer physical hardware certification from package presence.

### Package checks

The resolved runtime package inventory contains the required CPU microcode,
general device firmware and GPU firmware capabilities.

The installer inventory contains both vendor CPU microcode sources and the
general redistributable firmware required by its claimed module set.

Checks may name the current Fedora packages, but failure diagnostics must
describe the missing capability as well as the package which was expected to
provide it.

### Runtime-root checks

Artifact tests inspect the selected kernel and immutable runtime root and
verify:

```text
modinfo i915
modinfo xe
modinfo amdgpu
```

The commands must resolve the modules from the artifact under test, not from
the build host.

Tests also verify the Intel and AMD CPU and GPU firmware families below
`/usr/lib/firmware`. They use package ownership, representative family
directories or other stable capability evidence instead of freezing Fedora's
complete firmware blob inventory.

The check fails when a claimed driver is present but its required firmware
family is absent.

### UKI checks

UKI tests extract the embedded initrd and prove that early CPU microcode exists
for both Intel and AMD processors.

The verification must distinguish the leading early microcode archive from the
normal initramfs and prove its ordering. It must fail when:

```text
the packages exist only in the runtime root
microcode exists only in the normal initramfs
one vendor's early microcode archive is missing
the UKI was reused after its selected microcode input changed
```

Installer and runtime UKIs are checked independently.

### Driver and initrd checks

Tests compare the effective installer initrd module set with its firmware
content. When a retained early driver requires firmware for a claimed journey,
the corresponding firmware family must be present in the same initrd.

The runtime-root driver checks remain separate because most runtime GPU drivers
and firmware need not be embedded in the initrd.

### Hardware journeys

Artifact presence demonstrates packaged support, not hardware certification.

A specific firmware filename becomes a release requirement only when Katl adds
a tested hardware journey which depends on it. That journey must record the
relevant device, driver, firmware observation and failure diagnostics without
turning an incidental complete firmware inventory into a stable contract.

## Operator Experience

Users do not select firmware, package names or CPU vendors in Katl YAML.

Routine installation and upgrade use the same generic KatlOS artifact for
common Intel and AMD systems. Firmware updates appear as normal KatlOS
generation updates and report their reboot requirement through the existing
lifecycle interface.

Missing firmware detected during a claimed journey is a KatlOS release defect,
not an instruction for the operator to install a package into the immutable
host.

## Future Work

Specialized firmware may later be delivered through a bounded node-extension
mechanism.

Such a design must define:

```text
kernel compatibility
early-boot placement requirements
device reprobe or reboot semantics
licensing and redistribution
trust and artifact verification
generation identity and rollback behavior
status and failure diagnostics
```

Until that design exists, specialized and proprietary firmware remains
unsupported or operator-managed outside KatlOS.

## Consequences

Positive consequences:

```text
common Intel and AMD systems use one predictable x86-64 artifact
microcode is applied at the correct early-boot stage
in-tree GPU support includes its required redistributable firmware
firmware updates and rollback remain generation-consistent
operators do not need to understand Fedora firmware packaging
release checks make claimed coverage explicit
```

Costs and constraints:

```text
runtime roots and UKIs are larger
firmware package changes can force boot and runtime artifact rebuilds
Fedora package reorganizations require maintained capability checks
physical hardware remains necessary for certification-level confidence
specialized and proprietary hardware remains outside the supported boundary
```

## Alternatives Rejected

### User-selected firmware packages

This would expose Fedora package names through Katl configuration, create
unbounded combinations and make rollback compatibility the operator's
responsibility.

### Vendor-specific Intel and AMD images

Separate images increase release and test combinations without providing value
on generic x86-64 systems, where the kernel already selects the applicable
microcode and device firmware.

### Firmware sysexts

An independent firmware extension can be incompatible with the active kernel,
may be required before normal sysext activation and can break generation
rollback. A bounded future mechanism needs an explicit kernel and early-boot
contract before it is supported.

### Mutable runtime firmware installation

Installing firmware after boot violates the immutable-root model and can leave
the active kernel, initrd and runtime firmware at unrelated versions.
