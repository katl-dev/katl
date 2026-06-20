# Katl Current Design

Status: current architecture snapshot as of 2026-06-11.

This document is the short orientation guide for the active Katl design. Focused
details live in the companion documents under `docs/internal/` and accepted ADRs
under `docs/internal/adrs/`.

## North Star

The durable product direction lives in `docs/internal/north-star.md`.

Katl produces and maintains KatlOS: an installable, upgradeable,
systemd-native Kubernetes node OS. Users customize KatlOS by supplying Katl YAML
or configuration, which `katlc` validates and compiles into sysext/confext
generations. Those generations are activated with rollback-aware runtime state
while fitting a user-owned GitOps cluster workflow.

The long-term user workflow is:

```text
KatlOS source
  -> mkosi builds generic installer/runtime/sysext artifacts
  -> artifacts are published by the user's chosen release process
  -> machines boot installer-image through user-managed boot infrastructure
  -> katlos-install installs KatlOS generation 0 with stored cluster intent
  -> katlctl cluster bootstrap asks katlc to create the first
     Kubernetes-capable generation from that intent
  -> katlctl sequences explicit node requests while katlc runs kubeadm bootstrap
     or join operations and commits the generation after kubeadm and health
     checks succeed
  -> later explicit kubeadm-aware operations upgrade, repair, or recover nodes
  -> KatlOS activates, stages, reports, or rolls back host generations
  -> after bootstrap, the user installs and owns cluster add-ons, GitOps, and
     workloads
```

GitHub Actions is a useful north-star publishing environment, but it is not an
early implementation target. The current local focus is to build an installer
UKI, boot it in the VM runner, install a Fedora-derived runtime root artifact to a target
disk, and boot that installed runtime.

## Active Product Boundary

Katl-owned surfaces:

```text
katlc KatlOS state/configuration command
katlctl control client with only connection and known-node configuration
installer-image build inputs
katlos-install
runtime root artifact build inputs
artifact metadata and verification
Katl-owned root disk layout
generated confext content
systemd boot/update/mount/health wiring
local VM validation harness
```

User-owned surfaces:

```text
DHCP, TFTP, PXE, iPXE, matchbox, or firmware boot order management
end-user GitHub Actions workflows
Kubernetes add-ons, GitOps, workloads, and ongoing cluster lifecycle after
cluster bootstrap
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
  User-facing KatlOS state/configuration command. It accepts user-supplied Katl
  YAML or configuration, validates supported domains, compiles them into
  generation-scoped sysext/confext payloads and metadata, and applies, stages,
  reports, or rolls back runtime state.

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

KatlOS runtime
  Installed Fedora-derived node runtime. It is a pared down Linux system for
  systemd, SSH, the container runtime, and Katl-owned wiring. Kubernetes tools
  such as kubeadm and kubelet come from the selected Kubernetes sysext. It is not
  a bespoke distribution or a Talos-style appliance.
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
5. katlos-install verifies the KatlOS image and trust material. It does not
   bundle or fetch a Kubernetes sysext.
6. katlos-install partitions and formats the target root disk using Katl-owned
   policy.
7. katlos-install writes the prebuilt runtime root artifact to root-a.
8. katlos-install installs UKIs, systemd-boot entries, generation spec/status,
   generated confext, mount units, identity, SSH policy, and writable state.
9. The node reboots from the installed disk.
10. The runtime reaches a local Katl boot-complete target for generation 0 and
    waits for an explicit `katlctl cluster bootstrap` operation or other
    post-install host operations.
```

The installer does not build Fedora packages on the target node. It consumes
prebuilt artifacts and writes them into the installed layout.

## Configuration Model

Katl uses a Katl-native install manifest, user-supplied Katl YAML/configuration,
and generated sysext/confext generations.

Katl configuration is applied to nodes as Katl configuration. Users and external
automation should not have to prebuild sysext/confext generation content for a
node. On first install, `katlos-install` validates the manifest, bootstraps
generation 0, and stores cluster intent without activating Kubernetes. The first
Kubernetes-capable generation is created later when `katlctl cluster bootstrap`
asks `katlc` to validate that stored intent, fetch the exact Kubernetes payload
from a user-supplied HTTPS bundle source, and render the generated confext needed
for kubeadm. Later host configuration changes can use normal `katlc` generation
apply or stage flows. Sysext payloads are prebuilt artifacts; the node-local
generation spec records which compatible sysexts are selected with the rendered
confext after `katlc` has fetched and verified them.

`katlc` and KatlOS runtime services must fail closed. Unknown domains,
unsupported fields, unsupported sysext selections, unsupported apply modes, and
raw extension activation inputs are rejected before render, staging, live apply,
or boot selection.

Users supply:

```text
target root disk selector
destructive install authorization and exact data-loss acknowledgement
node identity inputs such as hostname and SSH public keys
KatlOS image reference and digest
exact Kubernetes payload bundle source/ref, such as a source URL plus
  `v1.36.0@sha256:<bundle-manifest-digest>`
extra non-root data disk requests
```

The current install manifest should stay minimal: hostname, `katl` SSH
authorized keys, target root disk, destructive install guard, one KatlOS image
reference, exact Kubernetes payload bundle source/ref, and extra non-root data
disks. Extra disks remain in scope because they exercise real install/runtime
disk handling.

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
bootstrap profile references that render native kubeadm YAML under /etc/katl
extra data disk mounts
```

Additional domains should be added deliberately when users need them. They must
define their render paths, validation rules, and runtime apply/restart behavior
before becoming part of the user-facing configuration API.

Kubeadm configuration is intentionally reached through a thin Katl bootstrap
profile reference, not YAML embedded as a string, not a kubeadm command line in
user intent, and not an init/join action. Node configuration selects a bootstrap
profile. During `katlctl cluster bootstrap`, `katlc` validates the stored intent
and renders the selected native kubeadm input under `/etc/katl/kubeadm/` as
part of the first Kubernetes-capable candidate generation. Later `katlc`
generation apply or stage flows can update desired kubeadm input, but explicit
kubeadm-aware operations decide when to run `kubeadm init`, `kubeadm join`, or
later kubeadm upgrade commands.

## Rejected Configuration Bootstrap

Katl does not use Ignition for installer or runtime configuration.

It was rejected because it would add a second configuration language and a
separate first-boot phase between `katlos-install` and KatlOS runtime state
management. Katl already needs typed validation, target disk ownership, artifact
verification, generated confext, and later `katlc`-generated configuration
generations. Keeping those responsibilities inside Katl gives one typed Katl
input path for install and host generation planning. It does not make the
manifest or generation status authoritative for accepted lifecycle attempts:
once install, bootstrap, join, upgrade, or repair accepts a request, the
node-local `OperationRecord` is authoritative for attempt state. Live Kubernetes
state remains authoritative in kubeadm output, kubelet state, etcd, and the
Kubernetes API.

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

Kubernetes binaries are delivered through the selected Kubernetes sysext for
v0.1. The base root keeps the container runtime and host prerequisites needed to
run that payload, but not kubeadm, kubelet, kubectl, or crictl. Kubernetes
add-ons, Helm, Flux, Cilium, CoreDNS, Rook, and application workloads are outside
the runtime base. The Kubernetes sysext is versioned independently from the
KatlOS runtime root. KatlOS upgrades should be able to keep the current
Kubernetes sysext, and Kubernetes upgrades should be able to keep the current
KatlOS root, when the selected artifacts are compatible. Day-one install records
an exact Kubernetes bundle source/ref such as
`v1.36.0@sha256:<bundle-manifest-digest>` as cluster intent. Cluster bootstrap
asks `katlc` to fetch the matching Kubernetes payload bundle from the
user-supplied HTTPS source, verify its Katl custom manifest, stage the sysext
locally, and select it for generation 1. The publication and catalog direction
for producing exact-version Kubernetes sysext payloads is defined in
`docs/internal/kubernetes-sysext-delivery.md`; already-bootstrapped Kubernetes
upgrades remain separate day-2 implementation work.

After the installer UKI and installed runtime boot path works, the next local
step is `katlctl cluster bootstrap`. That operation asks `katlc` to validate
stored intent, create and activate the first Kubernetes-capable candidate
generation, select the manifest-requested Kubernetes sysext, render kubeadm input
under `/etc/katl`, project writable kubeadm output at `/etc/kubernetes`, run
kubeadm through a node-local operation, and commit the generation only after
kubeadm and local health checks succeed.

The first proof should stay local: build or inspect a Kubernetes payload bundle,
serve it from a controlled HTTPS endpoint or equivalent test fixture, boot
generation 0 in the VM runner, run `katlctl cluster bootstrap`, verify that
`katlc` fetches and stages the manifest-selected Kubernetes sysext and generated
config, and prove kubeadm can initialize the control plane. CI-built
downloadable artifacts are a later publishing concern, not a blocker for this
local loop.
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
  Optional later no-login service user if KatlOS runtime services need an
  unprivileged phase.
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
renders it into the selected boot entry with `systemd.machine_id=` so PID 1 and
D-Bus consumers see the value early enough. The file should be write-protected
after install and stable across runtime boots and updates, but it does not need
to be deterministic or preserved across reinstalling the node.

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

Update and rollback code should prefer native systemd mechanisms over custom
replacement machinery: systemd-boot for boot selection and trial boots,
systemd-sysext and systemd-confext for extension activation, native mount units
for state projections, systemd-tmpfiles for Katl-owned state preparation, and
systemd health targets for local boot completion. Katl-owned agents coordinate
the generation spec/status, compatibility checks, operation records, and
rollback decisions.

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
boot installer-image in the VM runner with OVMF
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
build a Kubernetes payload bundle that contains katl-kubernetes-v1.36.0-x86_64.sysext.raw
boot generation 0
run katlctl cluster bootstrap
ask katlc to fetch, verify, and stage the manifest-selected sysext, then create
and activate generation 1
render kubeadm input under /etc/katl from known Katl config domains
project writable /etc/kubernetes from /var
start containerd and expose kubelet with Katl-controlled ordering
reach katl-kubeadm-ready.target before kubeadm runs
run kubeadm init or join through a node-local katlc operation
commit generation 1 only after kubeadm and operation health checks succeed;
boot health remains pending until a later boot
```

## Focused Design Documents

Use these files for implementation detail:

```text
docs/internal/north-star.md
  Durable product direction, user story, design principles, ownership
  boundaries, and document map.

docs/internal/installer-runtime-design.md
  Main component, disk, artifact, installer, runtime, and boot design.

docs/internal/generation-metadata-model.md
  Generation record shape and how a generation selects root/sysext/confext.

docs/internal/generations-and-operations.md
  Shared model separating normal generation apply from explicit, auditable
  operations such as bootstrap, join, and upgrade.

docs/internal/boot-health-semantics.md
  Local boot health contract and generation status transitions.

docs/internal/rollback-selection-rules.md
  Known-good generation selection and rollback rules.

docs/internal/boot-selection-transaction.md
  Transactional boot-selection state, systemd-boot arming, promotion, and
  recovery rules.

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

docs/internal/kubernetes-upgrade-operations.md
  Explicit kubeadm-backed Kubernetes upgrade operations selected and supervised
  through Katl generations.

docs/internal/kubernetes-sysext-delivery.md
  Kubernetes sysext artifact publication, catalog, and patch-version bump
  direction.

docs/internal/node-app-sysext-contract.md
  Generic contract for optional node applications delivered as generation-
  selected app sysexts.

docs/internal/adrs/adr-001-generated-confext-configuration.md
  Accepted decision for Katl-native configuration and generated confext. The file
  includes the rejected bootstrap option note.
```
