# Katl Current Design

Status: current architecture snapshot as of 2026-06-01.

This document is the short orientation guide for the active Katl design. Focused
details live in the companion documents under `docs/internal/` and accepted ADRs
under `docs/internal/adrs/`.

## North Star

Katl is a systemd-native Kubernetes node OS builder. It builds installer and
runtime assets for users who want reproducible, kubeadm-ready nodes without
turning Katl into a Kubernetes distribution or a site provisioning system.

The long-term user workflow is:

```text
Katl config repo
  -> katlc validates and compiles config
  -> mkosi builds installer/runtime artifacts
  -> artifacts are published by the user's chosen release process
  -> machines boot installer-image through user-managed boot infrastructure
  -> katlos-install installs the runtime OS
  -> kubeadm and user-managed GitOps take over
```

GitHub Actions is a useful north-star publishing environment, but it is not an
early implementation target. The current local focus is to build an installer
UKI, boot it in QEMU, install a Fedora-derived runtime root artifact to a target
disk, and boot that installed runtime.

## Active Product Boundary

Katl owns:

```text
katlc configuration compiler
installer-image build inputs
katlos-install
runtime root artifact build inputs
artifact metadata and verification
Katl-owned root disk layout
generated confext content
systemd boot/update/mount/health wiring
local QEMU validation harness
```

Katl does not own:

```text
DHCP, TFTP, PXE, iPXE, matchbox, or firmware boot order management
end-user GitHub Actions workflows
Kubernetes add-ons or cluster lifecycle after kubeadm handoff
Kubernetes distribution packaging
user-defined host accounts
user-defined root disk partitioning or filesystems
general configuration management
```

Users may choose any boot/provisioning stack that can boot the Katl installer
artifact and provide installer input. Katl can later provide examples, but those
examples must not make provisioning infrastructure part of Katl's core product
surface.

## Core Components

```text
katlc
  User-facing compiler. It validates Katl config and produces install manifests,
  mkosi inputs, artifact metadata, update artifacts, and later publish plans.

mkosi
  Build-time image tool. It is used by developers, build containers, and later
  CI to produce installer-image and runtime artifacts. It is not an install-time
  dependency inside installer-image or the runtime OS.

installer-image
  Temporary boot environment built with mkosi. The current shipped boot artifact
  is a single installer UKI containing the installer kernel, initrd, userspace,
  and katlos-install.

katlos-install
  Go installer application. It discovers installer input, validates the manifest,
  verifies artifacts, owns the target root disk layout, writes root slots,
  installs boot metadata, materializes initial generated confext, seeds writable
  state, and reboots.

runtime OS
  Installed Fedora-derived node runtime. It is a pared down Linux system for
  systemd, SSH, container runtime, kubeadm, and kubelet. It is not a bespoke
  distribution or a Talos-style appliance.
```

## Installer Flow

The active installer flow is Katl-native:

```text
1. User-managed boot infrastructure or local VM boots installer-image.
2. katlos-install reads kernel arguments, embedded media, local files, or enters
   local handoff mode.
3. If no manifest is present, local handoff starts a token-protected HTTP
   endpoint and waits for exactly one install manifest.
4. katlos-install validates the manifest before any destructive disk action.
5. katlos-install verifies runtime artifacts and trust material.
6. katlos-install partitions and formats the target root disk using Katl-owned
   policy.
7. katlos-install writes the prebuilt runtime root artifact to root-a.
8. katlos-install installs UKIs, systemd-boot entries, generation metadata,
   generated confext, mount units, identity, SSH policy, and writable state.
9. The node reboots from the installed disk.
10. The runtime reaches a local Katl boot-complete target and then the
    kubeadm-ready handoff point.
```

The installer does not build Fedora packages on the target node. It consumes
prebuilt artifacts and writes them into the installed layout.

## Configuration Model

Katl uses a Katl-native install manifest and generated confext.

Users supply:

```text
target root disk selector
destructive install authorization
node identity inputs such as hostname and SSH public keys
artifact references and digests
extra non-root data disk requests
```

The current install manifest should stay minimal: hostname, `katl` SSH
authorized keys, target root disk, destructive install guard, runtime artifacts,
optional sysext artifacts, and extra non-root data disks. Extra disks remain in
scope because they exercise real install/runtime disk handling.

Users do not supply:

```text
node matching selectors or hardware inventory policy
manifest names, metadata labels, or user-chosen generation IDs
root disk partition table
root slot sizes
root or state filesystem choices
host account definitions
sudo, PAM, passwd, shadow, or sysusers policy
machine-id values or machine-id policy
installer SSH override policy
artifact trust-root or signing policy
bootloader, loader entry, or kernel argument policy
extra disk mount options
prebuilt confext artifacts in the default path
arbitrary `/etc` file paths
Kubernetes-generated mutable state under /etc/kubernetes
```

Katl is not a general-purpose OS configuration system. The configuration surface
should be small, explicit, and domain-scoped, but thin enough that users are not
forced through a lossy abstraction. For example, a networkd domain may accept
native `.network`, `.netdev`, and `.link` content, but Katl owns the destination
under `/etc/systemd/network/` and owns the apply behavior with
`systemd-networkd`/`networkctl`.

Known configuration domains should be added only when implementation work needs
them. The first likely additions for kubeadm-readiness are:

```text
node identity and hostname
katl SSH authorized keys
networkd units
kubeadm config references that render native kubeadm YAML under /etc/katl
extra data disk mounts
```

Additional domains should be added deliberately when users need them. They must
define their render paths, validation rules, and runtime apply/restart behavior
before becoming part of the user-facing configuration API.

Kubeadm configuration is intentionally a thin reference to native kubeadm files,
not YAML embedded as a string and not an init/join action. Node configuration
selects a named kubeadm config, Katl validates and renders it under
`/etc/katl/kubeadm/`, and an operator or test harness decides when to run
`kubeadm init`, `kubeadm join`, or later kubeadm upgrade commands.

## Rejected Configuration Bootstrap

Katl does not use Ignition for installer or runtime configuration.

It was rejected because it would add a second configuration language and a
separate first-boot phase between `katlos-install` and the runtime agent. Katl
already needs typed validation, target disk ownership, artifact verification,
generated confext, and later runtime-generated configuration generations. Keeping
all of that in Katl avoids a three-phase installer/bootstrap/runtime model and
keeps the source of truth in the Katl manifest and generation metadata.

## Runtime OS Composition

The runtime OS should initially be Fedora-derived. Fedora gives Katl modern
systemd, current kernel/userspace integration, and practical package
availability without forcing Katl to become its own package distribution.

The base runtime root should include the smallest practical package set:

```text
kernel/initramfs support for the selected boot model
systemd, udev, journald
systemd-networkd, resolved, timesyncd
systemd-sysext and systemd-confext
systemd-tmpfiles, sysctl, modules-load
CA certificates
util-linux, iproute, kmod
nftables or the required host networking base
openssh-server
containerd
runc or crun
Katl-owned units and agents when they exist
```

Kubernetes binaries should initially be delivered as a sysext unless boot tests
show that kubelet ordering is simpler with them in the base root. Kubernetes
add-ons, Helm, Flux, Cilium, CoreDNS, Rook, and application workloads are outside
the runtime base. The Kubernetes sysext is versioned independently from the
KatlOS runtime root. KatlOS upgrades should be able to keep the current
Kubernetes sysext, and Kubernetes upgrades should be able to keep the current
KatlOS root, when the selected artifacts are compatible.

After the installer UKI and installed runtime boot path works, the next local
step is a kubeadm-ready installed runtime. That means the base runtime has
containerd and the host plumbing kubeadm expects, a Kubernetes sysext supplies
`kubeadm`, `kubelet`, `kubectl`, and tightly related binaries, generated config
places kubeadm input under `/etc/katl`, and writable kubeadm output is projected
at `/etc/kubernetes`.

The first proof should stay local: build or inspect the sysext artifact, install
it with the selected generation, boot the installed runtime in QEMU, reach
`katl-kubeadm-ready.target`, and run a bounded kubeadm preflight or dry-run check
that proves the node is prepared for `kubeadm init`. CI-built downloadable
artifacts are a later publishing concern, not a blocker for this local loop.
Artifact compatibility metadata, not matching product versions, decides whether
a runtime root and Kubernetes sysext can be selected together.

## Host Users And SSH

Katl defines host identities. Users do not define Linux users or host account
policy.

Required host users:

```text
root
  Exists, password locked, no SSH login.

katl
  The only SSH login account. Key-only authentication. No password login.

package/system users
  Only those required by Fedora/systemd/OpenSSH/container runtime packages.

katl-agent
  Optional later no-login service user if a runtime agent needs an unprivileged
  phase.
```

The runtime should not expose user-managed host accounts such as `admin`,
`kube`, or role-specific users. Kubernetes workload identities live inside
containers and pods; they are not host `/etc/passwd` entries.

Katl-generated configuration must own sshd policy. User-supplied generated
confext input must not write account or authentication control files such as:

```text
/etc/passwd
/etc/shadow
/etc/group
/etc/gshadow
/etc/sudoers
/etc/sudoers.d/*
/etc/pam.d/*
/etc/security/*
/etc/subuid
/etc/subgid
/etc/sysusers.d/*
/etc/ssh/sshd_config
/etc/ssh/sshd_config.d/*
```

Users provide SSH public keys through Katl config. Katl renders those keys for
the `katl` account and renders sshd policy such as key-only auth, no root login,
and `AllowUsers katl`.

## Disk Format

The installed root disk is Katl-owned after the user selects it and authorizes
destructive install.

Initial layout:

```text
ESP
  vfat, EFI boot files, systemd-boot as needed

optional XBOOTLDR
  vfat if used, UKIs and loader entries

root-a
  SquashFS runtime root artifact written directly to the partition

root-b
  inactive SquashFS runtime root slot for later updates

state
  ext4 initially, mounted at /var, consumes remaining root disk space

optional etcd
  later profile option, mounted at /var/lib/etcd when used
```

Root slots are immutable and versioned. The state partition is the durable
writable surface. Normal application and node state should stay under native
`/var` paths. Persistent paths outside `/var` require explicit systemd mount or
early-boot identity handling.

## Mutable State

Persistent state lives under `/var`:

```text
/var/lib/katl
/var/lib/katl/generations
/var/lib/katl/identity
/var/lib/katl/ssh
/var/lib/katl/kubernetes
/var/lib/kubelet
/var/lib/containerd
/var/lib/etcd
/var/log/journal, when enabled
```

`/etc/kubernetes` is the main writable `/etc` exception. It is kubeadm/kubelet
output and must be projected from `/var/lib/katl/kubernetes/etc-kubernetes`
with a bind mount. Generated confext must not own `/etc/kubernetes`.

`/etc/machine-id` is also special. `katlos-install` generates a random machine
ID during install, persists it under `/var/lib/katl/identity/machine-id`, and
exposes it early enough for systemd and D-Bus consumers. The file should be
write-protected after install and stable across runtime boots and updates, but
it does not need to be deterministic or preserved across reinstalling the node.

`/run` is ephemeral. It may hold boot-local activation links and handoff state,
but it must not hold persistent node identity.

## Boot, Updates, And Rollback

Each installed generation selects a complete runtime state:

```text
root slot
root artifact digest
UKI path
kernel command line
sysext set
independent runtime and sysext artifact versions
generated confext set
boot/health status
```

Rollback switches the selected generation as a unit. Katl must never roll back
only the root slot while leaving sysext or confext activation pointed at a
different generation.

The first update implementation should use a bounded trial boot:

```text
previous known-good generation remains the default boot entry
candidate generation is tried via systemd-boot one-shot or boot counting
candidate becomes default only after katl-boot-complete.target succeeds
failed candidate returns to the previous known-good generation
```

The boot health target is local. It proves the OS generation booted, mounted
state, activated selected extensions, established identity, and started required
local services. It does not need to prove full Kubernetes control-plane
convergence.

## Current Local Step

The immediate step is intentionally narrow:

```text
split mkosi profiles for installer and runtime
build an installer UKI
build a prebuilt runtime SquashFS artifact
boot installer-image in QEMU/OVMF
deliver install config without PXE
write the runtime artifact to a target disk
install EFI boot metadata
reboot into the installed runtime
verify deterministic serial/journal signals
```

End-user publishing workflows, provisioning examples, signed update envelopes,
runtime update agents, and full kubeadm automation come after the local
boot/install loop works.

The next step after that loop is still local and test-driven:

```text
build a Kubernetes sysext with kubeadm, kubelet, kubectl, and related binaries
install and activate the sysext as part of the selected generation
render kubeadm input under /etc/katl from known Katl config domains
project writable /etc/kubernetes from /var
start containerd and expose kubelet with Katl-controlled ordering
reach katl-kubeadm-ready.target in QEMU
run a bounded kubeadm preflight or dry-run check for kubeadm init readiness
```

## Focused Design Documents

Use these files for implementation detail:

```text
docs/internal/installer-runtime-design.md
  Main component, disk, artifact, installer, runtime, and boot design.

docs/internal/generation-metadata-model.md
  Generation record shape and how a generation selects root/sysext/confext.

docs/internal/boot-health-semantics.md
  Local boot health contract and generation status transitions.

docs/internal/rollback-selection-rules.md
  Known-good generation selection and rollback rules.

docs/internal/persistent-state-inventory.md
  Persistent node and Kubernetes state inventory.

docs/internal/writable-state-layout.md
  First writable state partition layout.

docs/internal/etc-kubernetes-projection.md
  Persistent /etc/kubernetes projection.

docs/internal/kubeadm-config-input-design.md
  Native kubeadm config input API, validation boundary, and render paths.

docs/internal/go-vm-test-harness-design.md
  Go-authored VM scenario harness for install, update, rollback, and
  Kubernetes integration tests.

docs/internal/kubeadm-api-smoke-design.md
  Single-node kubeadm init proof that reaches a kubectl-responsive API server.

docs/internal/adrs/adr-001-generated-confext-configuration.md
  Accepted decision for Katl-native configuration and generated confext. The file
  includes the rejected bootstrap option note.
```
