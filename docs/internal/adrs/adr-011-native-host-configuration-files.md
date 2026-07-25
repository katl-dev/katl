# ADR-011: Native host configuration is carried as managed file sets

Status: accepted; amended by ADR-012 for confext payloads coupled to
user-owned system extension bundles.

Date: 2026-07-24.

## Context

KatlOS is an immutable, systemd-native Kubernetes node OS. Operators still need
to use ordinary Linux configuration interfaces that are already stable and
well documented outside Katl, including:

```text
/etc/sysctl.d/*.conf
/etc/udev/rules.d/*.rules
/etc/modules-load.d/*.conf
/etc/modprobe.d/*.conf
service-specific configuration and systemd drop-ins
```

Adding a typed Katl field for every Linux subsystem would make Katl a second
host configuration language. It would also make an otherwise native Linux
feature depend on a Katl schema, renderer, and release before an operator could
use it.

Katl nevertheless owns lifecycle behavior that an ordinary mutable host does
not provide:

```text
trusted configuration input
generation-scoped materialization
Katl-owned path protection
live versus next-boot planning
atomic activation with root and sysext selection
status, rollback, and upgrade collision checks
```

ADR-001 selected generated systemd confext as the internal mechanism for
generation-scoped `/etc`. ADR-002 selected `auto`, `live`, and `next-boot`
runtime apply modes. ADR-007 selected a self-contained Katl config bundle
compiled from operator-authored ClusterConfig. This decision adds a generic
native-file surface to those existing contracts without exposing confext
artifacts or activation paths to operators.

The current typed `sysctl.settings` input is the wrong abstraction boundary.
Sysctl configuration, udev rules, and module loading should all be expressible
as native files. Katl may understand selected native paths well enough to offer
safe online activation, but that understanding must not be a prerequisite for
carrying the file and applying it at boot.

## Decision

ClusterConfig supports named native host configuration sets under
`hostConfiguration.sets`.

Each set owns:

```text
a stable operator-selected name
one or more regular files below /etc
optional bounded systemd notifications
present or absent desired state
```

Katl compiles the selected sets into the same generation-scoped generated
confext as Katl-owned configuration. Users do not build, upload, name, order,
or activate confexts. Katl continues to select exactly one generated node
configuration layer with the root, UKI, sysext set, and generation metadata.

This is a bounded native-file interface, not an arbitrary host mutation
interface. It writes only validated regular files beneath `/etc`; it does not
run shell, accept arbitrary commands, write mutable state directly, install
packages, or select extension artifacts.

## Operator Story

As a home-lab operator, I want to place native Linux configuration on all or
selected KatlOS nodes so that I can use documented Linux and systemd facilities
without waiting for Katl to add a bespoke schema, while Katl preserves the
configuration across immutable upgrades and prevents it from corrupting
Katl-managed state.

The representative journey is:

```text
Given
  cluster defaults containing a sysctl.d file, a udev rules file, and a
  modules-load.d file

When
  the operator runs katlctl cluster apply with the complete ClusterConfig

Then
  Katl validates every path and reports the native effect of each set
  sysctl assignments are eligible for live apply when rollback can be proven
  udev rules are eligible for live reload without retriggering existing devices
  the kernel module list is classified as next-boot-only
  auto stages the complete mixed change as one next-boot generation
  the current boot remains unchanged
  the candidate boot consumes all three files through their native interfaces
  boot health promotes the generation only after required services succeed
  repeating the same apply produces no mutation
  a later KatlOS upgrade preserves the files or rejects a new ownership
    collision before activation
```

A later sysctl-only change may apply live and must prove the observed kernel
value. A udev-only change may reload the rule database and must say that
existing devices were not retriggered. A protected Katl path is rejected before
the candidate generation is written.

## User-Facing Configuration

The source ClusterConfig shape is:

```yaml
apiVersion: katl.dev/v1alpha1
kind: ClusterConfig

spec:
  defaults:
    hostConfiguration:
      sets:
        kernel-forwarding:
          files:
            - path: /etc/sysctl.d/80-home-lab-forwarding.conf
              content: |
                net.ipv4.ip_forward = 1
                net.ipv6.conf.all.forwarding = 1

        ups-device:
          files:
            - path: /etc/udev/rules.d/80-home-lab-ups.rules
              content: |
                SUBSYSTEM=="usb", ATTR{idVendor}=="051d", MODE="0660", GROUP="dialout"

        storage-modules:
          files:
            - path: /etc/modules-load.d/80-home-lab-storage.conf
              content: |
                br_netfilter
                vfio_pci

        journal-limits:
          files:
            - path: /etc/systemd/journald.conf.d/80-home-lab.conf
              content: |
                [Journal]
                SystemMaxUse=2G
          notify:
            systemd:
              - unit: systemd-journald.service
                action: try-reload-or-restart
```

`hostConfiguration` may appear under `spec.defaults` or on a concrete entry in
`spec.nodes`. It does not add node classes, selectors, or another merge layer.

Each file provides exactly one of:

```text
content
  Inline UTF-8 content.

source
  A path relative to the ClusterConfig source root. katlctl reads it while
  compiling the config bundle and embeds the bytes into that self-contained
  artifact.
```

The source reference is authoring convenience, not runtime indirection.
Installers and node agents never resolve an external file reference. Users do
not provide content digests as routine ceremony; Katl calculates and records
them internally.

File mode is optional and defaults to `0644`. The initial accepted modes are
`0600`, `0640`, and `0644`. All files are owned by `root:root`, are
non-executable, and are materialized as regular files.

Set state defaults to `present`. A node override may use `state: absent` to
remove a set inherited from cluster defaults. An absent set may not also
declare files or notifications.

## Merge And Desired-State Semantics

Cluster defaults are merged before the flat per-node configuration.

Sets are keyed by name:

```text
a node set with a new name is added
a node set with an inherited name replaces that complete inherited set
state: absent removes the inherited set
omitting a previously desired non-inherited set removes it from the next
  desired generation
```

Files are keyed by their normalized absolute path after sets are merged.
Different selected sets may not own the same path, even when their bytes are
identical. Rejecting duplicate ownership keeps deletion, activation, status,
and rollback unambiguous.

The whole selected node configuration is desired state. Katl does not append
files indefinitely or treat a submitted set as an imperative patch.

## Katl Ownership Boundary

Katl must distinguish Katl-owned configuration from ordinary local
administrator configuration.

Every KatlOS release carries an internal ownership policy containing:

```text
exact files rendered by Katl domains
protected Katl configuration prefixes
protected systemd units and their drop-in namespaces
host identity, mount, boot, lifecycle, and Kubernetes state paths
```

Katl renders and inventories its own candidate output before accepting native
host files. A host file is rejected when it collides with either the ownership
policy or a concrete Katl-rendered output.

Path ownership does not imply that Katl interprets precedence inside every
native format. An operator file can intentionally override a distribution or
Katl default through the subsystem's normal filename or directive precedence.
Katl validates a bounded semantic invariant separately only when that invariant
is required for boot health, operator access, management reachability, or
another supported KatlOS journey. The rejection must name that invariant rather
than claiming an unrelated path collision.

The ownership policy is not a list of every distribution default. Operators may
use normal `/etc` precedence to configure software supplied by the immutable
root or a selected sysext unless Katl relies on and protects that path.
Distribution defaults normally belong under `/usr/lib`; local configuration
under `/etc` remains the operator surface.

The initial hard-denied paths and prefixes include:

```text
/etc/extension-release.d
/etc/katl
/etc/kubernetes
/etc/fstab and Katl-owned mount units
/etc/hostname and other Katl-owned identity output
/etc/passwd, /etc/shadow, /etc/group, and /etc/gshadow
/etc/pam.d and /etc/security
/etc/sudoers and /etc/sudoers.d
/etc/subuid and /etc/subgid
/etc/sysusers.d
/etc/ssh/sshd_config and /etc/ssh/sshd_config.d
Katl-owned authorized_keys files
Katl lifecycle, generation, boot-health, installer, katlc, kubelet, containerd,
  and other release-critical unit files and drop-in directories
```

Validation also rejects:

```text
paths outside /etc
relative paths, path traversal, NUL bytes, and non-normalized ambiguity
duplicate normalized paths
symlinks, hard links, devices, sockets, FIFOs, and directories as file entries
setuid, setgid, sticky, executable, group-writable, or world-writable modes
content and source supplied together
missing, non-regular, or escaping source files
content that exceeds configured request and bundle size limits
```

User-authored secrets are not supported in the initial file-set API. Config
bundles, generation artifacts, plans, and retained source input are not a
general secret store. A later secret reference and materialization contract may
write a permitted service configuration path without placing secret bytes in
ClusterConfig, but it is a separate decision.

## Upgrade Collision Handling

KatlOS upgrades validate the desired host file tree against the target
release's ownership policy before selecting a candidate root.

If a newer KatlOS release claims a path currently owned by a user set, the
upgrade is rejected before root, sysext, or confext activation. The current
generation remains selected. The diagnostic names:

```text
the conflicting /etc path
the user set that currently owns it
the target KatlOS release and Katl component that claims it
the required rename, removal, or supported Katl field
```

Katl does not silently discard the user file, let extension ordering decide the
winner, or overwrite Katl output.

## Apply Planning

Native host files form one public `host-configuration` domain. Plans and status
also retain the set name and normalized changed paths so operators can
understand which configuration caused each action.

File support and live support are deliberately separate:

```text
permitted file with a proven live adapter
  May be accepted live when the specific diff passes adapter preflight.

permitted file with an explicit bounded systemd notification
  May be accepted live when the unit, action, and rollback preflight pass.

permitted file without a proven live plan
  Accepted as next-boot by auto. Strict live rejects it with an actionable
  staged-only diagnostic.

unsafe, protected, ambiguous, or unsupported file
  Rejected before a candidate generation is written.
```

Unknown native configuration does not require a new Katl schema or renderer.
It remains useful at next boot through the underlying Linux subsystem. Adding a
future live adapter improves application behavior; it does not unlock the
file's basic availability.

The existing request-wide apply rule remains: a mixed request uses the most
conservative supported mode. If any changed set is next-boot-only, `auto`
stages the complete node configuration and leaves the current boot unchanged.
Strict `live` rejects the whole request before activation.

## Built-In Activation Adapters

Built-in activation adapters are internal Katl policy. They are not public
sysctl, udev, or module configuration schemas.

### sysctl.d

Ordinary concrete assignments in `/etc/sysctl.d/*.conf` may apply live when
Katl can:

```text
parse every changed assignment
resolve the affected concrete kernel parameters
snapshot their current runtime values before mutation
apply only the selected candidate files
verify the requested runtime values
restore the snapshot after any partial or later apply failure
```

Globs, exclusions, conditional failures, ambiguous precedence, deletions whose
effective previous value cannot be determined, or parameters that do not yet
exist are staged for next boot in the initial implementation. Katl must not
claim rollback merely because it restored the old file; live kernel values also
have to be restored.

This adapter replaces the need for a typed public `sysctl.settings` domain.

### udev rules

Changed `/etc/udev/rules.d/*.rules` files may apply live after supported syntax
verification. Katl refreshes the generated confext and asks the running udev
manager to reload its rules.

Katl does not automatically retrigger devices. Reloaded rules affect later
events; existing devices are unchanged. Status says this explicitly. An
operator who needs devices to be reprobed uses a separate explicit and bounded
operation because a broad trigger can rename interfaces, change permissions,
or disrupt storage and device consumers.

Rollback restores the previous confext selection and reloads the previous rule
set. Katl does not claim that effects from device events which occurred while a
candidate rule set was active have been reversed.

### modules-load.d and modprobe.d

Files under `/etc/modules-load.d` and `/etc/modprobe.d` are next-boot-only in
the initial implementation.

Katl does not automatically load, unload, blacklist, or rebind kernel modules
during normal live configuration apply. Module state can affect storage,
networking, device ownership, and workloads in ways that cannot be generically
rolled back.

For a generation containing `modules-load.d` files,
`systemd-modules-load.service` participates directly in boot health. Failure to
load a requested module prevents that generation from becoming known-good and
uses the normal bounded trial-boot rollback.

### systemd units and drop-ins

Refreshing the generated confext is followed by one systemd manager
`daemon-reload` when systemd unit files or drop-ins changed. A daemon reload
does not imply that a running service consumes its new configuration.

A set may declare bounded notification for an existing unprotected systemd
unit:

```yaml
notify:
  systemd:
    - unit: example.service
      action: reload
```

Accepted actions are:

```text
reload
try-reload-or-restart
try-restart
```

These actions never start an inactive unit merely because configuration was
added. `start`, `stop`, `enable`, `disable`, masks, arbitrary signals, transient
units, command arguments, and shell hooks are not accepted.

Katl validates that the selected root and sysext set provide the unit, the unit
is not protected by Katl, and the requested action is structurally supported.
Live execution records the unit and action, checks the command result, restores
the previous confext on failure, and repeats the same notification against the
previous configuration during rollback.

A notification runs only when its owning set changes. Identical notifications
from multiple changed sets are coalesced. Conflicting actions for the same unit
are rejected instead of relying on list order. Built-in adapters run before
declared notifications so a service observes the completed native subsystem
update.

Notification is an expert escape hatch for software already present in KatlOS
or a selected external sysext. Standard paths with a built-in adapter do not
require operators to know a systemd unit or Katl implementation detail.

### Other files

Other permitted `/etc` files without a notification are staged for next boot.
Katl performs structural validation and any available native syntax validation,
but it does not pretend to understand every consumer.

Boot health continues to assert KatlOS release-critical services directly.
Configuration for an unrelated non-critical service remains operator-owned;
Katl reports when no semantic validator or health assertion is available.

## Activation, Status, And Rollback

Accepted live application uses the existing generation transaction:

```text
validate and classify the complete desired node configuration
write the operation record before mutation
render the complete candidate confext
move the generation-scoped activation link
refresh systemd-confext
run one daemon-reload when required
run built-in adapters and declared notifications in deterministic order
verify adapter results and release-critical health
commit the generation only after all live checks pass
```

Failure after activation restores the previous generation link, refreshes
systemd-confext, replays the previous adapters and notifications, and verifies
the previous runtime state. If rollback cannot be proven, Katl selects the
previous generation for next boot and reports `repair-required`.

Accepted next-boot application renders and selects a bounded trial generation
without refreshing the current confext or running any adapter or notification.
The normal boot-health contract decides promotion or rollback.

Operator-visible planning and status report outcomes, not implementation
ceremony. A plan should read like:

```text
kernel-forwarding: live; 2 kernel settings will change
ups-device: live reload; existing devices will not be retriggered
storage-modules: next boot; kernel module state is not changed online
journal-limits: live; systemd-journald.service will be notified
```

Persisted records contain set names, paths, content digests, classifications,
actions, results, and rollback targets. Routine status does not echo file
content, source bytes, or low-level systemd invocation identifiers.

## Install And Update Behavior

The same `hostConfiguration` input is valid during first install and runtime
apply.

At install, Katl compiles selected sets into generation 0. There is no live
apply phase; native consumers see the files during the first installed boot.

Host upgrades preserve the complete desired native configuration while
rendering the candidate generation against the target release's ownership
policy. User files never drift through a mutable global `/etc` tree independent
of generation selection.

Removing a set creates a new complete generation without its files. Live
removal is allowed only when the responsible adapter or notification can prove
the running state transition and rollback. Otherwise removal is staged for next
boot.

## Sysext Boundary

This API does not build, fetch, verify, select, or activate arbitrary sysexts.

An externally built sysext may provide:

```text
executables and libraries
base systemd units
read-only application defaults
```

Katl `hostConfiguration` may provide the selected node's permitted `/etc`
configuration and notify an existing unit from that sysext. Sysext selection,
compatibility, provenance, and lifecycle remain separate Katl extension
contracts.

The file-set API also does not turn confext into a distribution format. Katl
generates its node confext locally from trusted configuration and records it by
digest. Users do not supply prebuilt confext images or raw activation paths.

## Relationship To Typed Katl Domains

Typed Katl configuration remains appropriate when Katl must understand intent
to protect the supported product journey. Examples include:

```text
stable node identity and operator access
management network safety and control-plane reachability
disk selection, formatting, mounts, and destructive guards
Kubernetes payload selection and kubeadm lifecycle
Katl-owned application capability contracts
```

Native host configuration is appropriate when Linux already defines the
durable file contract and Katl does not need a new operator-facing model.

Before the stable ClusterConfig contract, Katl removes the public typed
`sysctl.settings` shape in favor of a native `/etc/sysctl.d` file set. Katl does
not add public `udev`, `modulesLoad`, `modprobe`, or per-daemon configuration
domains merely to reproduce their native file formats.

## Non-Goals

This decision does not provide:

```text
arbitrary writes outside /etc
mutable in-place editing of the active /etc tree
shell hooks or a general command runner
package installation or an embedded package manager
host accounts, PAM, sudo, SSH daemon policy, or sysusers passthrough
secret storage in ClusterConfig
systemd unit enablement, disablement, masks, or arbitrary service lifecycle
automatic device retrigger, module unload, or module rebinding
prebuilt user confext ingestion
sysext building or unbounded extension activation
Talos patch compatibility or a general templating language
Kubernetes add-on or GitOps lifecycle
```

## Testing Contract

Unit tests cover:

```text
known-field decoding for hostConfiguration sets, files, state, and notifications
defaults and node replacement/removal semantics
inline content and source reference resolution into a self-contained bundle
deterministic normalized ordering and content digests
duplicate path ownership across sets
all protected path, mode, file type, traversal, and source escape rejections
collision with concrete Katl render output and target-release ownership policy
upgrade refusal diagnostics
live versus next-boot classification for every adapter and generic file fallback
systemd unit name, protected-unit, and action validation
no-op repeat apply and desired set removal
redacted plans, operation records, and status
rollback planning and replay
```

Golden tests cover the complete generated confext tree for shared defaults,
node replacement, node removal, and mixed Katl-owned and user-owned files.
Generated unit files and drop-ins are checked with `systemd-analyze verify`
where practical.

VM tests cover:

```text
first install with sysctl.d, udev rules, and modules-load.d files
simple sysctl-only live apply with observed /proc/sys state
sysctl live failure with restored file and runtime kernel value
udev rule reload with an explicit report that existing devices were not triggered
mixed sysctl and modules-load change selecting next boot without current mutation
successful module load during candidate boot and generation promotion
module-load failure causing boot-health rollback
bounded systemd notification success and failure rollback
protected Katl path and protected unit rejection before rendering
unknown permitted /etc configuration persisting across reboot and host upgrade
target-release ownership collision stopping upgrade before activation
repeat apply producing no mutation
```

The VM journey uses `katldev` and the public `katlctl cluster apply` surface. It
verifies useful progress, actionable errors, the actual native runtime outcome,
and generation rollback rather than merely checking that files were rendered.

## Consequences

Katl gains a broad Linux customization surface without taking ownership of
every Linux configuration language.

The public configuration API becomes smaller: one generic native file-set
contract replaces several prospective subsystem-specific fields. Katl still
needs carefully tested Go policy for paths, ownership, diff classification,
activation, status, and rollback.

Some valid native configuration applies only at next boot until a safe adapter
exists. That is an intentional least-disruptive fallback, not lack of file
support.

ADR-001 is amended: generated confext may contain bounded operator-authored
native host files, while raw user-supplied confext artifacts and activation
paths remain rejected.

ADR-002 is amended: native host file diffs are classified by adapter,
notification, or conservative next-boot fallback rather than requiring a
separate public Katl domain for every path family.

ADR-003 is amended: authenticated runtime input may contain validated
`hostConfiguration` paths; arbitrary unbounded `/etc` mutation remains
rejected.

The supported-node-config-domains decision is amended in the same way. Its
typed-domain rules continue to apply to Katl-owned product intent, while this
ADR governs operator-owned native host files.
