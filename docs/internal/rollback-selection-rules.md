# Rollback Selection Rules

This decision defines how Katl returns to the previous known-good generation
after a failed boot or explicit rollback request.

## Decision

Rollback selects a generation spec and validates its status, not an individual
root partition. The selected spec determines:

```text
root slot
UKI path
kernel command line
sysext activation set
confext activation set
```

Katl must never roll back only the root slot while leaving sysext or confext
activation pointed at the failed generation.

Rollback should use native systemd mechanisms where they fit. Boot selection
should use systemd-boot one-shot, boot counting, or loader-entry behavior before
custom boot selectors. Extension rollback should regenerate the selected
systemd-sysext and systemd-confext activation inputs from generation spec
rather than maintaining a separate mutable extension state.

## Known-Good Rule

A generation becomes known-good only after it reaches the configured boot health
signal, its status validates against immutable generation spec, and its status is
updated to:

```text
commitState: committed | superseded
bootState: good
healthState: healthy
```

The previous known-good generation is the newest generation with
valid `status.json.specDigest`, `healthState: healthy`, `bootState: good`, and
`commitState: committed` or `commitState: superseded` that is not the currently
failed or currently tried generation. Superseded means a healthy generation is
no longer the active default, not that it is unsafe for rollback.
`config-apply-status.json` health is not sufficient for known-good rollback
selection.

## Failed Boot Rollback

When a tried generation fails its boot attempt:

```text
mark tried generation failed/unhealthy
select previous known-good generation by validated status and spec
set boot entry for previous known-good UKI/root slot from spec
regenerate /run extension activation links from previous known-good spec
boot previous known-good generation
```

If there is no previous known-good generation, automatic rollback is not
available. That is the first-install failure case and requires reinstall or
repair tooling.

Failed boot rollback marks only the failed tried generation `failed/unhealthy`.
The rollback target keeps its existing `good/healthy` boot health and its
existing commit state. `/run` activation links are regenerated from the rollback
target spec after that target is selected for the running boot. Rollback must
not mutate the target spec or any other selection fields.

## Explicit Rollback

An explicit rollback request uses the same selection path as failed boot
rollback, but the triggering generation does not need to be marked failed. It
may be marked superseded or left good depending on the operator action.

The first implementation should support rolling back to the immediate previous
known-good generation. Arbitrary generation selection can be added later once
repair tooling exists.

If the rollback target fails to boot, Katl must report rollback failure rather
than marking the abandoned generation as repaired.

## Rollback And Kubeadm Mutation Boundary

Rollback is host generation selection, not repair. It is allowed to select a
previous known-good generation, restore the root slot, UKI, kernel command line,
sysext activation set, confext activation set, and regenerate `/run` activation
links from that generation spec.

Rollback must not:

```text
run kubeadm or kubectl
edit, replace, sanitize, or delete /etc/kubernetes
edit, replace, sanitize, or delete /var/lib/kubelet
restore, rewrite, delete, or reinterpret /var/lib/etcd
remove or replace etcd members
restore an etcd snapshot
mutate Kubernetes API objects
replace install input
clean partial bootstrap, join, upgrade, or repair state
```

After kubeadm has mutated node or cluster state, host rollback may make the node
bootable again, but it must not report Kubernetes state repaired. The associated
operation record must keep `recoveryRequired` until an explicit kubeadm-aware or
etcd-aware repair operation succeeds. Mutation scopes should be recorded with
names such as `etc-kubernetes`, `kubelet-state`, `etcd-state`, and
`cluster-objects`.

Boot-time operation reconciliation may finish host rollback bookkeeping, rebuild
operation snapshots from valid journals, or mark interrupted host-only work as
requiring explicit repair. It must preserve `recoveryRequired` for
stale-post-mutation and stale-ambiguous records until an explicit retry or repair
operation clears it.

When rollback cannot select a previous known-good generation, Katl reports
recovery-required and records diagnostics for a deferred recovery operation. It
must not invent a rollback target.

## First Install Seed

`katlos-install` must seed enough metadata for future rollback:

```text
generation spec under /var/lib/katl/generations/<id>/spec.json
generation status under /var/lib/katl/generations/<id>/status.json
root-a PARTUUID and runtime artifact digest
UKI path and kernel command line
generated confext path, digest, compatibility, and activation path
sysext paths, activation paths, and digests
commitState committed
bootState pending
healthState unknown
```

After the first runtime reaches the boot health target, it becomes the first
known-good generation.

## Boot Entry Selection

Boot entries must identify the generation they boot. A generation-specific UKI
or loader entry should point to the selected root PARTUUID and include enough
metadata for the runtime to find its generation spec and status.

Each bootable entry must include:

```text
katl.generation=<generation-id>
root=PARTUUID=<selected-root-slot-partuuid>
```

Durable boot selection state lives under:

```text
/var/lib/katl/boot/selection.json
```

The boot selection record stores `defaultGenerationID`, `trialGenerationID`,
`previousKnownGoodGenerationID`, `bootedGenerationID`, and `$BOOT`-relative
loader or UKI paths for the default, trial, previous known-good, and booted
entries. Boot-counted trial filenames and recovery flags belong in this boot
selection record, not in immutable generation spec.

The first implementation should keep the previous known-good generation as the
default boot entry and try a candidate with systemd-boot one-shot selection. A
candidate must not become the default entry until it reaches the configured
boot health target. Explicit boot counting can be layered on later; the
generation spec remains the source of truth for root and extension selection.
If `bootedGenerationID` is missing, unknown, or disagrees with the root PARTUUID
or generation spec, Katl must not promote or bless the boot.

## Validation

Rollback validation must ensure:

```text
selected generation spec exists and parses
selected generation status validates against selected spec digest
selected root slot PARTUUID exists
selected UKI path exists
selected sysext/confext paths exist under the selected generation
activation links under /run point only to the selected generation
failed generation is not left partially active
```

VM update tests should eventually prove that a failed generation returns to
the previous known-good root slot and matching sysext/confext set.
