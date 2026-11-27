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

## Boot move owner

One owner in `internal/generation` writes boot moves. Production writers
keep the legacy flow in Transaction Flow below until they move to this owner.
Every step runs under the boot-state lock, in this order:

1. Reconcile any outstanding intent, then admit the move. A stale
   `expectedSelectionRevision` is rejected. Repeating a move that was already
   published and resolved changes nothing and reports it resolved. A repeat
   is recognised only when the next revision holds its selection and every
   generation it changes carries its operation ID, so a different move with
   an equal selection is rejected as stale. An operation ID that already
   appears in a target generation's transitions is refused, as is a move
   while a one-shot for a generation other than the running one or the
   default is selected, because the intent could not record it.
2. Durably write the intent. Before writing, the owner validates the intent
   with the v1 reader's rules and checks the status transitions and the
   bootability of every target that is not the running generation.
3. Check the revision in `selection.json`: it must be the expected revision,
   or the intent's desired selection already published. On a conflict the
   move stops before it changes status, EFI or reboot eligibility.
4. Project `statusChanges`. A generation whose transitions already carry the
   intent's operation ID was projected by this move and is skipped.
5. If EFI differs from the projection, or EFI cannot be read, clear
   `LoaderEntryOneShot`, set `LoaderEntryDefault`, then set the desired
   one-shot. The owner then reads both variables back. A read error or a
   mismatch stops the move with the intent in place.
6. Spend reboot eligibility, if the move has any, by rewriting the same
   intent with `reboot.spent` and `desiredSelection.spentRecoveryTrialID` set.
7. Publish `desiredSelection`, unless it is already published.
8. Resolve the intent by removing it, unless the intent is a trial
   authority. Only after this step can the caller overwrite a slot or
   remove a loader entry.

The owner requests a reboot only from a call that spent eligibility in step 6,
on the boot that began the move, when the running generation is not the
move's next target. A crash between the spend and the dispatch leaves zero
reboots for that trial, and a retry never requests a second one.

The projection depends on the boot. On the boot that began the move, it is
the desired default and the desired one-shot. Once that boot has ended, the
desired one-shot counts as delivered: firmware boots a one-shot at most once,
so reconciliation accepts it as consumed and never arms it again. If the
variable still holds the desired entry, the owner leaves it. Any other
one-shot is cleared.

Reconciliation runs at activation of every boot, before another move is
admitted, and before a target is reported as selected. It first validates
the running generation with the activation check, which requires one of the
four recorded targets with a matching root PARTUUID, then completes the
intent from its first unfinished step. A conflicting selection revision, an
unrelated running generation or a wrong root PARTUUID stops reconciliation
without changes. With no intent, reconciliation imports a legacy trial as an
in-memory trial authority (see Migration below). Generation 0 and manual selections
have no legacy trial and import nothing.

Every transition that a timer or a fault causes, such as a deadline expiry,
a health failure, or a trial that ended without confirmation, is a move.
Each records a status transition carrying its operation ID, reason and time,
so tests observe the event in `status.json` rather than on a wall clock. The
owner takes its clock as an input. The deadlines that trigger these moves are
unchanged: the 90-second management-link check in `katl-boot-health` and the
10-minute `katl-boot-deadman.timer`.

## Trial authority lifetime

A trial authority is an intent whose reboot is eligible and unspent and
whose `desiredSelection.trialGenerationID` equals `destinationGenerationID`.
Only the arm move creates a durable one; a legacy import holds one in memory
(see Migration below). Every other intent resolves when its selection is
published.

Admission enforces the rules that keep trial authority with the owner rather
than with caller convention:

- An arm keeps the published default; only promotion moves it to the
  candidate.
- A promotion publishes the bound candidate as the default.
- A move that does not arm a trial may not publish a selection that the
  reader would classify as a legacy trial: pending health validation and a
  `pendingTransactionID` for a generation with a predecessor.

An authority survives publication and the reboot, because it is the only
durable record that arms recovery. While it exists, only a move that resolves
it is admitted. That move replaces the authority with its own intent, which
uses the authority's `trialID` as its `reboot.trialID`.

`bootID` names the boot that began the move, so it is never compared with the
candidate's boot ID. Activation binds the authority to a boot when that boot
is not the boot that began the move and the kernel-selected generation is the
destination. It must also pass the activation check. A health result can
resolve only a bound authority.

Three outcomes resolve an authority:

- **Promotion** publishes the candidate as the default, with an ineligible
  reboot.
- **Failure** publishes the fallback, or `recoveryRequired` when no bootable
  fallback remains. A failure with a fallback spends the trial's reboot. A
  failure without one has no destination and no reboot.
- **An unconfirmed trial** is resolved by reconciliation when the authority is
  published and a later boot runs another recorded target. The trial has
  then ended, whether the candidate failed before confirming or firmware
  never booted it. Reconciliation marks the candidate's boot `failed`, and
  computes the selection with the same failure rule as a failed trial: the
  fallback becomes the default when it passes the recovery check, and
  `recoveryRequired` is set otherwise. It spends the reboot without
  dispatching it. A next target that is neither bootable nor running stops
  the resolution without changes, with the authority in place.

Binding is derived, not recorded: every boot of the candidate that is not the
issuing boot binds the authority again, so the candidate may run more than
once before a health result resolves the trial. What the owner guarantees is
at most one automatic reboot per trial, because the spend is durable before
the selection is published and before any reboot is requested.

An interrupted arm whose issuing boot has ended follows the same rule. It
completes, then resolves as unconfirmed unless the candidate is running.

### Lifecycle example

Generation `gen1` is the default. On boot `b-1`, the arm move writes the
intent below, sets `LoaderEntryDefault` to `katl-gen1.conf`, sets
`LoaderEntryOneShot` to `katl-gen2.conf`, verifies both, and publishes
revision 5. The authority remains.

```json
{"operationID": "apply-7", "bootID": "b-1", "expectedSelectionRevision": 4,
 "sourceGenerationID": "gen1", "destinationGenerationID": "gen2",
 "priorDefault": {"generationID": "gen1", "entry": "katl-gen1.conf"},
 "priorOneShot": {"generationID": "", "entry": ""},
 "desiredDefault": {"generationID": "gen1", "entry": "katl-gen1.conf"},
 "desiredOneShot": {"generationID": "gen2", "entry": "katl-gen2.conf"},
 "desiredSelection": {"revision": 5, "defaultGenerationID": "gen1",
   "trialGenerationID": "gen2", "previousKnownGoodGenerationID": "gen1",
   "pendingTransactionID": "apply-7", "pendingHealthValidation": true, "...": "..."},
 "statusChanges": [{"generationID": "gen2", "bootState": "pending", "...": "..."}],
 "reboot": {"trialID": "apply-7", "eligible": true, "spent": false}}
```

The managed reboot starts boot `b-2`. Firmware consumes the one-shot and
boots `gen2`. Activation reconciles: the issuing boot has ended, so the empty
one-shot matches, and `gen2` is the destination, so the authority binds to
`b-2`.

The health check on `b-2` fails. The failure move replaces the authority:

```json
{"operationID": "fail-apply-7", "bootID": "b-2", "expectedSelectionRevision": 5,
 "sourceGenerationID": "gen2", "destinationGenerationID": "gen1",
 "priorDefault": {"generationID": "gen1", "entry": "katl-gen1.conf"},
 "priorOneShot": {"generationID": "gen2", "entry": ""},
 "desiredDefault": {"generationID": "gen1", "entry": "katl-gen1.conf"},
 "desiredOneShot": {"generationID": "", "entry": ""},
 "desiredSelection": {"revision": 6, "defaultGenerationID": "gen1",
   "failedBootGenerationID": "gen2", "...": "..."},
 "statusChanges": [{"generationID": "gen2", "bootState": "failed",
   "healthState": "unhealthy", "reason": "katl-boot-health-deadline-expired", "...": "..."}],
 "reboot": {"trialID": "apply-7", "eligible": true, "spent": false}}
```

The process crashes after it verifies EFI and before it spends the reboot.
`katl-boot-recovery.service` runs as the OnFailure retry on `b-2`. It
reconciles the same intent, skips the status already projected under
`fail-apply-7`, finds EFI matching, rewrites the intent with `"spent": true`
and `"spentRecoveryTrialID": "apply-7"`, publishes revision 6, removes the
intent, and requests one reboot into `gen1`. If the crash had come after the
spend, the retry would publish and resolve without a reboot, leaving `gen1`
as the next boot. Revision 6 keeps `spentRecoveryTrialID`, so later retries
on `b-2` also request no reboot.

### Migration

A legacy trial has a staged generation with a predecessor, a trial or next
pointer, pending health validation and a `pendingTransactionID`. It is
imported as a trial authority. Its operation ID and trial ID are the
`pendingTransactionID`. Its `bootID` is `legacy-import`, which is never a
kernel boot ID, because the boot that armed the trial is unknown. The import
observes EFI for the prior entries. An EFI read error refuses the import.

A legacy import writes no intent and publishes nothing in this release. A
rollback target may run the reader-only release, which refuses any intent it
cannot complete, so a durable import would leave that release unable to
boot the node's own legacy trial. The import therefore keeps the authority in
memory, and each reconciliation derives it again from the unchanged legacy
selection. The authority becomes durable only in the intent of the move that
resolves it: a promotion, a failure, or an unconfirmed-trial resolution,
which expects the legacy selection's revision.

### Open questions

- Withdrawing or superseding an armed trial before its reboot, for example
  when the operator cancels a next-boot change, has no move today: while the
  authority exists only a resolving move is admitted. #820 must define that
  workflow.

### Notes for #820

- `removeBootMoveIntent` is kept as written; review it when production
  callers start resolving intents.
- The owner keeps an existing `committedAt` when a status change commits a
  generation, while the live promotion path in `boot_health.go` overwrites
  it. #820 must choose one rule when it moves that path to the owner.
- An arm interrupted before its one-shot is written resolves on a later boot
  with the same reason as a trial that firmware booted, because firmware
  consumes the one-shot on every boot and the state cannot tell the two
  apart.

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
