# Rollback Selection Rules

This decision defines how Katl returns to the previous known-good generation
after a failed boot or explicit rollback request.

## Decision

Rollback selects a generation record, not an individual root partition. The
selected record determines:

```text
root slot
UKI path
kernel command line
sysext activation set
confext activation set
```

Katl must never roll back only the root slot while leaving sysext or confext
activation pointed at the failed generation.

## Known-Good Rule

A generation becomes known-good only after it reaches the configured boot health
signal and its health state is updated to:

```text
healthState: healthy
```

The previous known-good generation is the newest generation record with
`healthState: healthy` that is not the currently failed or currently tried
generation. Its `bootState` may be `good` or `superseded`; superseded means a
healthy generation is no longer the active default, not that it is unsafe for
rollback.

## Failed Boot Rollback

When a tried generation fails its boot attempt:

```text
mark tried generation failed/unhealthy
select previous known-good generation
set boot entry for previous known-good UKI/root slot
regenerate /run extension activation links from previous known-good metadata
boot previous known-good generation
```

If there is no previous known-good generation, automatic rollback is not
available. That is the first-install failure case and requires reinstall or
repair tooling.

## Explicit Rollback

An explicit rollback request uses the same selection path as failed boot
rollback, but the triggering generation does not need to be marked failed. It
may be marked superseded or left good depending on the operator action.

The first implementation should support rolling back to the immediate previous
known-good generation. Arbitrary generation selection can be added later once
repair tooling exists.

## First Install Seed

`katlos-install` must seed enough metadata for future rollback:

```text
generation record under /var/lib/katl/generations/<id>/metadata.json
root-a PARTUUID and runtime artifact digest
UKI path and kernel command line
generated confext path, digest, compatibility, and activation path
sysext paths, activation paths, and digests
bootState pending
healthState unknown
```

After the first runtime reaches the boot health target, it becomes the first
known-good generation.

## Boot Entry Selection

Boot entries must identify the generation they boot. A generation-specific UKI
or loader entry should point to the selected root PARTUUID and include enough
metadata for the runtime to find its generation record.

The first implementation should keep the previous known-good generation as the
default boot entry and try a candidate with systemd-boot one-shot selection. A
candidate must not become the default entry until it reaches the configured
boot health target. Explicit boot counting can be layered on later; the
generation record remains the source of truth for root and extension selection.

## Validation

Rollback validation must ensure:

```text
selected generation metadata exists and parses
selected root slot PARTUUID exists
selected UKI path exists
selected sysext/confext paths exist under the selected generation
activation links under /run point only to the selected generation
failed generation is not left partially active
```

QEMU update tests should eventually prove that a failed generation returns to
the previous known-good root slot and matching sysext/confext set.
