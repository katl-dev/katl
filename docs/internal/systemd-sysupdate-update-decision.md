# systemd-sysupdate Update Decision

Status: current decision.

This decision defines how Katl uses `systemd-sysupdate` for KatlOS runtime
updates while keeping Katl generation spec, status, and boot selection state
authoritative for the complete bootable runtime state.

Primary local references for this decision are `systemd-sysupdate(8)`,
`sysupdate.d(5)`, `systemd-boot(7)`, `bootctl(1)`,
`systemd-bless-boot.service(8)`, `systemd-sysupdated.service(8)`, and
`org.freedesktop.sysupdate1(5)`.

## Decision

Use `systemd-sysupdate` as the default transport and staging primitive for
KatlOS runtime root and runtime UKI updates, but not as Katl's update policy
engine.

Katl owns:

```text
KatlOS image verification and component compatibility checks
generation spec/status creation
root, UKI, sysext, and confext selection as one generation
boot health promotion and rollback selection
node configuration validation and generated confext rendering
Kubernetes and etcd safety policy
cluster rollout policy
```

`systemd-sysupdate` owns only bounded resource transfer into pre-created slots or
boot files after Katl has selected and verified a candidate update source. This
keeps the update path systemd-native without letting sysupdate become the
installer, config compiler, Kubernetes operator, or generation database.

## Resource Mapping

Katl's first sysupdate target is the host runtime payload. Transfers in one
target must share one version identifier so the runtime root and UKI are staged
together.

Do not split the root, UKI, and future verity resources into separate
`--component=` targets for the first implementation. The local
`systemd-sysupdate(8)` documentation reserves components for independently
updated sets; Katl's root and UKI must update synchronously.

Initial transfer set:

```text
50-katl-root.transfer
  source: url-file or regular-file runtime root image
  target: partition
  target path: auto
  target partition type: root
  target match pattern: katl_@v
  read-only: yes

70-katl-uki.transfer
  source: url-file or regular-file runtime UKI
  target: regular-file under $BOOT
  target path: /EFI/Linux
  target path relative to: boot
  target match pattern: katl_@v+@l-@d.efi, katl_@v+@l.efi, katl_@v.efi
  mode: 0644
```

The root transfer writes the inactive root slot. `systemd-sysupdate` requires
the target partitions to already exist, so first install and disk layout remain
the responsibility of `katlos-install` and `systemd-repart`.

Katl currently models logical slots as `root-a` and `root-b` and uses fixed GPT
labels such as `KATL_ROOT_A` and `KATL_ROOT_B` during install planning.
Sysupdate partition targets instead discover available slots by partition type
and use partition labels for version state, including the special `_empty`
label. The prototype must resolve this friction explicitly. Katl generation
metadata should keep the logical slot and selected PARTUUID as authority; the
sysupdate-facing partition label may become version-bearing implementation
state after first install.

The UKI transfer is deliberately ordered after the root transfer. `sysupdate.d`
finalizes transfers alphabetically, and the boot entry is the runtime entry
point; writing it last avoids exposing a bootable entry before the matching root
slot has been staged.

Optional verity support adds one earlier transfer:

```text
40-katl-root-verity.transfer
  source: url-file or regular-file verity image
  target: partition
  target partition type: root-verity
  target match pattern: katl_@v_verity
  read-only: yes
```

When verity is used, the UKI or loader entry must carry the selected root
identity and verity metadata needed to boot the exact root and verity pair. The
Katl generation spec stores those fields with the root artifact digest.

## Versions And Slots

Use a single KatlOS image version as the sysupdate `@v` value for the root,
verity, and UKI transfers in a combined KatlOS update. This version must be
stable, sortable by sysupdate, and identical across all resources in the target.

`InstancesMax=2` is the initial policy for root and UKI transfers. Katl starts
with two root slots and keeps the current known-good generation plus one
candidate. More slots can be added later, but the generation model and VM
rollback tests should prove two slots first.

Set `ProtectVersion=` to the running KatlOS image version when updating online.
The `%A` specifier reads `IMAGE_VERSION=` from `/etc/os-release`; Katl's runtime
root must therefore expose the KatlOS image version there before sysupdate is
used online. This protects the booted OS resources from being vacuumed or
overwritten while they are in use.

For UKIs, use `TriesLeft=` and `TriesDone=` with `@l` and `@d` in the target
match patterns so new candidate UKIs participate in systemd-boot automatic boot
assessment. The first implementation may set one try to match Katl's current
rollback policy, or use more tries only after VM tests cover repeated failed
boots.

Boot counting renames the booted UKI after a successful boot. Katl generation
spec should record the canonical final UKI path. Mutable trial boot path state,
including boot-counted `+@l-@d` filenames, belongs in
`/var/lib/katl/boot/selection.json` while a generation is trying. The sysupdate
version and boot-counted filename are not the generation identity.

## Single KatlOS Image Publication

The user-facing artifact remains the single KatlOS image defined in
`docs/internal/single-katlos-image-artifact.md`. Sysupdate transfer files do not
consume that embedded index directly; they transfer individual resources.

Publication must therefore provide one of these views for the same release:

```text
single KatlOS SquashFS image
  durable user-facing install and upgrade artifact

sysupdate component view
  runtime root image
  runtime UKI
  optional verity image
  SHA256SUMS
  SHA256SUMS.gpg when using remote url-file sources
```

The component view must be generated from the same verified component bytes and
metadata as the single image. The single image remains the contract for
installers and offline media. The component view is an implementation view for
sysupdate transport.

For local or offline updates, Katl may mount a verified KatlOS image and run
sysupdate with `regular-file` sources and `--definitions=` pointing at
Katl-generated transfer definitions for the mounted component directory. Local
regular-file sources do not provide sysupdate's remote signature verification,
so Katl must verify the top-level image digest and embedded component digests
before invoking sysupdate.

Because sysupdate source `MatchPattern=` values must contain `@v`, the mounted
image layout must either include a sysupdate-compatible component directory or
Katl must create a temporary source directory with versioned names such as
`katl_<version>.root.squashfs` and `katl_<version>.efi` before invoking
sysupdate.

For remote updates, the published component view should use `url-file` sources.
`SHA256SUMS` provides payload hashes, and `SHA256SUMS.gpg` provides the detached
signature expected by sysupdate when verification is enabled. Katl publication
metadata must define the signing key distribution story before remote automatic
updates are enabled by default.

## Generation Metadata Authority

Sysupdate's newest installed version is not the Katl source of truth. A Katl
candidate is complete only after Katl writes:

```text
/var/lib/katl/generations/<generation-id>/spec.json
/var/lib/katl/generations/<generation-id>/status.json
```

The generation spec stores the selected root slot, root partition identity,
runtime artifact digest, UKI path, sysext set, generated confext set, kernel
command line, and previous generation. Generation status stores `commitState`,
`bootState`, and `healthState` bound to the spec by `specDigest`. A sysupdate
transfer result is only an input to those records.

Rollback selects a generation spec, not a sysupdate version. Rolling back
must restore:

```text
root slot
UKI path
kernel command line
systemd-sysext activation set
systemd-confext activation set
```

Katl must not ask sysupdate to independently update Kubernetes sysexts or
generated confexts as part of the first host runtime target. Sysexts and
confexts may eventually get their own transport, but Katl generation spec/status
must still select their activation with the root and UKI as one validated set.

## Boot Health And Activation

Systemd-boot provides the boot entry and automatic boot assessment mechanism.
New candidate UKIs should be installed with boot-counting names. The booted
runtime reaches `katl-boot-complete.target`, then Katl marks the generation
healthy and may allow `systemd-bless-boot.service` or `systemd-bless-boot good`
to mark the booted UKI as good.

Katl should not persistently default a candidate generation before health
passes. Initial activation should either use systemd-boot one-shot selection or
boot-counting entries with the previous known-good generation still available.

A successful sysupdate transfer is staging, not activation or commit. Commit
order is: transfer resources, write candidate metadata, arm bounded boot
selection, boot candidate, reach `katl-boot-complete.target`, mark
`good/healthy`, then make the candidate the persistent default.

After health passes, Katl updates generation status to:

```text
bootState: good
healthState: healthy
```

Only `status.json` is updated by health promotion. Only then may the candidate
become the persistent default in boot selection state and the previous healthy
generation be marked `commitState: superseded`. Making a candidate the
persistent default is bootloader and boot-selection state, not mutation of
generation selection fields.

If boot health fails, Katl marks the candidate failed/unhealthy and selects the
previous known-good generation. Sysupdate may later vacuum obsolete transferred
resources, but only after Katl no longer needs them for rollback.

## What sysupdate Must Not Own

Sysupdate must not own or infer:

```text
node config schema validation
generated confext rendering
live vs next-boot apply mode decisions
kubeadm desired input safety
kubeadm init or join execution
etcd membership and quorum policy
CNI readiness
cluster workload convergence
multi-node rollout ordering
rollback target selection
generation status mutation
secrets, bootstrap tokens, or node identity
```

Those decisions stay in Katl product logic and tests. Sysupdate should see only
validated bytes and transfer definitions.

## D-Bus Service

Defer `systemd-sysupdated` and the `org.freedesktop.sysupdate1` D-Bus API for
the first implementation.

The local manpage for `org.freedesktop.sysupdate1` marks the API unstable and
subject to breaking changes. Katl should first prototype direct
`systemd-sysupdate` CLI invocation with explicit transfer definitions and VM
rollback tests. D-Bus integration can be revisited after the resource mapping,
signing model, and boot-health flow are proven.

## Consequences

This decision preserves the one-image install and upgrade contract while using
systemd's native A/B transfer machinery where it fits. It adds a publication
requirement: releases need either a generated sysupdate component view or a
local/offline flow that mounts the verified KatlOS image and presents its
components as regular-file sources.

The next implementation step is a small prototype that stages a root partition
and UKI into an installed VM, creates Katl generation spec/status from the
staged resources, boots the candidate, and proves both health promotion and
failed boot rollback in VM tests. The prototype must also prove the
partition-label transition
from Katl's current `KATL_ROOT_A`/`KATL_ROOT_B` labels to sysupdate's
version-bearing or `_empty` labels, and it must cover boot-counted UKI path
recording while the candidate is still trying.
