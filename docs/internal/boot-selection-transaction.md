# Boot Selection Transaction

Status: current decision.

This decision defines how Katl changes the selected boot generation while
keeping generation metadata, systemd-boot state, and recovery behavior
consistent.

## Decision

`katlc` owns durable boot-selection state under:

```text
/var/lib/katl/boot/selection.json
```

Systemd-boot executes the selected entry and provides one-shot or boot-counted
attempt mechanics. `systemd-sysupdate` may stage root and UKI bytes, but it does
not decide Katl generation identity, persistent default selection, known-good
promotion, or rollback target selection.

`selection.json` is the node-local source of truth for mutable boot pointers:

```text
defaultGenerationID
trialGenerationID
previousKnownGoodGenerationID
bootedGenerationID
defaultBootEntry
trialBootEntry
previousKnownGoodBootEntry
bootedBootEntry
bootCountedTrialPath, when used
pendingTransactionID, when a selection update is in progress
recoveryRequired
updatedAt
```

Generation `spec.json` remains immutable. Generation `status.json` records
acceptance, boot state, and health state. It does not contain mutable boot
selection pointers.

## Source Of Truth

| State | Authority |
| --- | --- |
| Desired host artifacts | `/var/lib/katl/generations/<id>/spec.json` |
| Generation acceptance and health | `/var/lib/katl/generations/<id>/status.json` |
| Persistent default, trial, and booted pointers | `/var/lib/katl/boot/selection.json` |
| Actual boot attempt mechanics | systemd-boot one-shot or boot-counted entry state |
| Resource transfer labels and filenames | `systemd-sysupdate` implementation state only |

Katl may validate systemd-boot state and sysupdate-staged resources, but those
states are never authoritative over Katl generation selection.

## Durable boot-move intent

The runtime reads `/var/lib/katl/boot/move-intent.json` under the boot-state
lock before it interprets the legacy pending target. A separate file lets
runtimes continue reading `selection.json` in its existing format while the
intent reader rolls out to every generation the node can boot. The
reader-only runtime creates no intent.

The v1 file uses the persisted-record JSON envelope with
`recordType: katl.boot.move-intent` and `recordVersion: 1`. The payload contains
the following fields.

| Field | Meaning |
| --- | --- |
| `operationID`, `bootID` | The unique ID of the move and the kernel boot ID of the boot that began it. A subsequent boot can encounter the same move. |
| `expectedSelectionRevision`, `desiredSelection` | The compare-and-set input and the exact `BootSelection` payload to publish. Its `revision` is one greater than the expected revision. Reconciliation can distinguish a move awaiting publication from one already published. |
| `sourceGenerationID`, `destinationGenerationID` | The generation the operation moves from and the intended destination. An empty destination records an operation with no safe destination. |
| `priorDefault`, `priorOneShot`, `desiredDefault`, `desiredOneShot` | Each object records the logical `generationID` and the raw systemd-boot EFI entry ID in `entry`. An empty prior entry means the variable was absent, including when firmware already consumed the one-shot before the move began. Preserve the logical prior one-shot generation in that case. An empty desired default entry uses the loader default. A cleared desired one-shot has both fields empty. |
| `statusChanges` | An array of absolute changes with at most one per generation. Each carries `generationID`, `at`, `reason`, `commitState`, `bootState`, `healthState`, and `unavailableReason` (empty when clear). Projection uses the intent’s `operationID` as the status transition ID for each generation, so each change applies once. When the change first commits a generation, the projection uses `at` and `operationID` for its commit attribution. An empty array leaves status alone. |
| `reboot` | An object with `trialID`, `eligible`, and `spent`. An ineligible operation has an empty trial ID and `spent: false`. An eligible operation refers to the ID of the originating trial and can request a reboot once. Once spent, its exact trial ID is stored in `desiredSelection.spentRecoveryTrialID` and published to `selection.json` so retries can recognise it after intent retirement. |

All payload fields are required. The reader rejects a malformed or unreadable
file and unsupported versions, with recovery guidance. Every nonempty EFI
entry must match the named generation's loader entry. The desired default and
any desired target or trial must also agree with the published selection. A
desired one-shot can repeat the default when the selection has no target or
trial. Writers can emit a
format or semantics only after every reachable runtime understands them.
Future incompatible changes require a new version and a reader rollout
before those writers are enabled.

When the file is absent, the runtime imports the validated legacy selection
and the spec-bound statuses of the running and referenced generations under
the same lock. An omitted `revision` is revision 0, and the import preserves
`pendingTransactionID`. A staged generation with a predecessor, a trial or
next-boot pointer, pending health validation, and an operation ID is recorded as a
legacy trial. First-install health and manual selections have no legacy
trial. The import also observes `LoaderEntryDefault` and
`LoaderEntryOneShot` through the shared efivarfs reader. Mounted, readable
efivarfs distinguishes a consumed or unset one-shot from a read error. A read
error or a referenced generation record error remains part of the observation.
Import changes no file or EFI variable.

With an intent present, boot activation checks the kernel-selected generation
against the four logical targets before interpreting status. The generation
must pass the kernel root PARTUUID check and the bootability verifier's
boot-time checks for the bound partition, loader entry, and UKI. If the
current intent already projected its own slot invalidation, activation can
still use that generation while the artifacts and identity pass verification.
Other recorded invalidations still apply. The writer must durably record
intent before projecting status, then clear the EFI one-shot, set the
default, and set the desired one-shot. It must verify both variables,
publish selection, and retire the intent before allowing any slot overwrite.
The boot-time identity check is sufficient during the intent because the
writer preserves the slot bytes throughout that interval.

Successful activation records the kernel-selected generation in `/run`.
During a pending move, node status and the console display that generation
as current and the intent's desired one-shot or default as the next target
after the move completes. The reader-only runtime leaves the durable
selection and intent untouched. It withholds legacy recovery authorization
and refuses boot health, target moves, and managed reboots. It also keeps
interrupted live promotion receipts eligible for recovery at a later startup
after a runtime capable of reconciliation completes the move. A missing
intent uses the existing boot health and selection path.

## Transaction Flow

To try a new candidate generation:

```text
1. validate candidate spec/status and previous known-good status
2. write pendingTransactionID in selection.json
3. install or validate the candidate boot entry and UKI path
4. arm systemd-boot one-shot or boot-counted trial entry
5. atomically replace selection.json with trialGenerationID set
6. fsync the boot-selection directory
7. reboot or report the armed trial
```

To promote a booted generation after `katl-boot-complete.target`:

```text
1. verify bootedGenerationID matches the boot entry, root PARTUUID, and spec
2. verify the generation reached bootState good and healthState healthy
3. update selection.json so defaultGenerationID becomes the booted generation
4. clear trialGenerationID and pendingTransactionID
5. record previousKnownGoodGenerationID
6. update systemd-boot persistent default or bless boot state
7. mark the previous committed generation superseded when appropriate
```

To roll back after a failed trial:

```text
1. mark the tried generation failed/unhealthy
2. select previousKnownGoodGenerationID from validated selection and status
3. arm or restore the previous known-good boot entry
4. clear trialGenerationID and pendingTransactionID
5. record recoveryRequired when no previous known-good generation is available
6. allow systemd to reboot only when the validated previous generation was
   selected successfully
```

Every `selection.json` update uses same-filesystem temporary files, `fsync`,
atomic rename, and directory `fsync`. Temporary files are ignored during
recovery.

## Failure Handling

If power is lost after resources are staged but before `selection.json` changes,
the generation is still only a candidate and is not selected for boot.

If power is lost after systemd-boot is armed but before `selection.json` names
the trial generation, boot-time reconciliation must refuse promotion and restore
the previous known-good selection when available.

If power is lost after `selection.json` names a trial but before reboot, the next
boot continues the bounded trial or reports that the trial was armed but not
started.

If a booted generation cannot prove `bootedGenerationID`, root PARTUUID, and spec
digest agreement, Katl must not promote or bless the boot.

If the trial fails after the recovery units are available, Katl persists the
failed trial and restored default before `katl-boot-recovery.service` requests a
normal systemd reboot. Activation arms that reboot only on the selected trial
after validating a distinct known-good fallback, and failure handling consumes
the authorization before requesting it. The fallback cannot re-arm recovery and
is the final automatic attempt. If it also fails, Katl records recovery-required
state and leaves the node running for console diagnosis instead of creating a
reboot loop.

If `selection.json` is missing, corrupt, or inconsistent with generation
metadata, boot-time reconciliation reports recovery-required. It may reconstruct
only when enough validated generation status and boot entry evidence proves a
single safe previous known-good generation.

## Boundaries

Commit is generation acceptance. It is not persistent default selection.

Known-good promotion is a boot-health result plus a boot-selection transaction.

Rollback selects a complete previous generation. It never independently switches
only root, sysext, confext, or kubeadm-owned runtime state.

`katlctl` may request or observe selection-changing operations through
node-local `katlc`, but it must not write `selection.json`, loader entries,
kernel arguments, or generation status.
