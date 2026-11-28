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
selection and intent untouched. It withholds recovery authorization
and refuses boot health, target moves, and managed reboots. It also keeps
interrupted live promotion receipts eligible for recovery at a later startup
after a runtime capable of reconciliation completes the move. A missing
intent uses the existing boot health and selection path.

## Boot move owner

One owner in `internal/generation`, `BootMover`, writes boot moves. It is the
only code that writes `selection.json`, `move-intent.json`, `LoaderEntryDefault`,
`LoaderEntryOneShot`, and the generation status transitions that belong to a
boot move. Config apply, host upgrade, live promotion, manual selection, slot
invalidation, removal, pruning, boot activation, and boot health all call it,
and they differ only in their inputs. Every step runs under the boot-state
lock, in this order:

1. Reconcile any outstanding intent, then admit the move. A stale
   `expectedSelectionRevision` is rejected. Repeating a move that was already
   published and resolved changes nothing and reports it resolved. A move that
   changes status is a repeat only when the next revision holds its selection
   and every generation it changes carries its operation ID, so a different
   move with an equal selection is rejected as stale. A move with no status
   changes leaves no durable operation ID, so a request for the selection that
   already holds is a repeat. an operation ID that already
   appears in a target generation's transitions is refused, as is a move
   while a one-shot for a generation other than the running one or the
   default is selected, because the intent could not record it. That
   one-shot blocks every move, including a change of the one-shot, until the
   node reboots into it, because lifting the block would let the issuing boot
   accept a running generation outside the four recorded targets.
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

Admission refuses to write an intent when a recorded target other than the
running generation, meaning the prior or desired default or one-shot, runs a
release that cannot read boot-move records. `internal/generation` owns that
rule: final releases and alpha, beta and rc candidates are judged by base,
and one whose base is below 2026.11.6 is refused, so 2026.11.5 and
2026.11.5-rc.1 are refused and 2026.11.6-rc.1 is not. Any `-dev.N` build or
version outside the release grammar, such as `0.0.0-dev`, is a reader. A
development stamp does not identify the code a build contains: the
driver-qualification lane stamps from the calendar month, and the nightly
candidate derives its stamp from the release tags reachable on the built
branch. The rule therefore does not protect a development build made from
code older than 2026.11.6, nor the published 2026.7.0-dev.0 to
2026.7.0-dev.4 development releases, which predate the reader; development
builds are unsupported outside testing. The refusal
changes nothing and tells the operator to boot the named generation from the
boot menu and upgrade or remove it. `katlctl node upgrade` applies the same
predicate before it starts, refusing a source below 2026.11.6 when the target is
at or after its own release, but only the owner's refusal holds for an older
`katlctl` or a request sent straight to the agent. An arm is also refused when
the running generation is not the published default, because the fallback is
always the published default and must complete the records a failed trial
leaves. It succeeds only when the default has run a release with the owner,
which the owner records as a marker file at
`/var/lib/katl/boot/owners/<generation ID>` when it publishes that default. The
default has a marker when it is the running generation, or boots the same runtime
artifact (`Root.RuntimeArtifactSHA256`) as the running generation, as a live
promotion does.

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

Activation reconciles in a restricted mode, because its sandbox can neither
open the root partition for a full bootability check nor write EFI variables.
It projects status, accepts EFI that already matches, publishes the selection
and resolves the intent where it can. A step that needs an EFI write or a full
check, including the `unconfirmed-` resolution of a trial, is deferred with
every file as it was. A deferral does not fail activation of an accepted
generation. `katl-boot-health.service`, or `katl-boot-recovery.service` when
health fails, reconciles unrestricted before its own decision, so the move
completes in the same boot. Until then every move and managed reboot refuses
with the reconcile error. When no intent remains, activation also observes the
boot with the same rules the reader-only runtime applies, publishing only the
booted fields, the active generation, and the booted one-shot. With an intent
present it writes nothing to `selection.json`, and the move that resolves the
intent publishes the booted fields instead.

Only `FailTrial` may cross the reboot spend. Reconciliation from every other
entry point stops before the spend when it meets an unspent, eligible,
non-authority intent and refuses with a message that
`katl-boot-recovery.service` finishes the failed trial. Only the recovery and
deadman units turn a requested reboot into a reboot, so a spend anywhere else
would lose the trial's one reboot.

Every transition that a timer or a fault causes, such as a deadline expiry,
a health failure, or a trial that ended without confirmation, is a move.
Each records a status transition carrying its operation ID, reason and time,
so tests observe the event in `status.json` rather than on a wall clock. The
owner takes its clock as an input. The deadlines that trigger these moves are
the 90-second management-link check in `katl-boot-health` and the 10-minute
`katl-boot-deadman.timer`. The timer is not enabled in `timers.target`.
Activation starts it only when it binds a trial authority to the boot, and
`katl-boot-health` stops it once `ConfirmBoot` or `FailTrial` has decided the
boot. `OnBootSec=` counts from boot, so a timer started late still expires ten
minutes after boot. A failed start fails the trial: activation completes,
reports that it could not start the boot deadline, and exits unsuccessfully, so
the failed health service runs `katl-boot-recovery.service` on the bound trial.
A failed stop is logged and does not fail boot health.

## Trial authority lifetime

A trial authority is an intent whose reboot is eligible and unspent and
whose `desiredSelection.trialGenerationID` equals `destinationGenerationID`.
Only the arm move creates a durable one; a legacy import holds one in memory
(see Migration below). Every other intent resolves when its selection is
published.

Admission enforces the rules that keep trial authority with the owner rather
than with caller convention:

- An arm keeps the published default until the trial resolves.
- A promotion publishes the bound candidate as the default.
- A move that does not arm a trial may not publish a selection that the
  reader would classify as a legacy trial: pending health validation and a
  `pendingTransactionID` for a generation with a predecessor.

A move that withdraws a trial is admitted only while the authority exists and
is not bound, because the candidate has not booted under it. Its intent
replaces the authority, clears the trial pointers and pending state, clears the
one-shot, and grants no reboot. The candidate's status is whatever the
request projects; it is not marked failed. The only production caller is slot invalidation
when the slot holds the armed candidate. No public command withdraws a trial: a
second apply, a manual selection, and an upgrade are refused with the guidance
to reboot into the trial.

An authority survives publication and the reboot, because it is the only
durable record that arms recovery. While it exists, only a move that resolves
it is admitted. That move replaces the authority with its own intent. If the
resolution records reboot eligibility, it uses the authority's `trialID`
as its `reboot.trialID`.

`bootID` names the boot that began the move, so it is never compared with the
candidate's boot ID. Activation binds the authority to a boot when that boot
is not the boot that began the move and the kernel-selected generation is the
destination. It must also pass the activation check. A health result can
resolve only a bound authority.

Promotion publishes the candidate as the default without granting a reboot.

A health failure or deadline on a boot bound to the authority ends a confirmed
trial of candidate `C`. `FailTrial` is the owner's only entry point for this
failure. On a boot with no bound trial it returns a report-only result and
writes nothing, except that a consumed manual selection is cleared by a move
with no status changes. It derives the operation ID by prefixing the authority's trial ID
with `fail-`, so repeat triggers can finish the same move without spending
another reboot. When reconciliation finds a published authority on a later
boot of a different recorded generation `R`, it resolves the trial as
unconfirmed under the operation ID `unconfirmed-<trialID>`. It records the
reboot as spent without requesting one.

Both paths check each distinct fallback candidate at most once in this order:

1. `F`, the recorded previous known-good generation. Use the published
   default if the pointer is empty or the selection is a one-shot, provided
   the default differs from `C`.
2. `D`, the published default, if it differs from both `C` and `F`.
3. `R`, the running generation, for an unconfirmed trial if it differs from
   `F` and `D`.

A usable fallback differs from `C`, is
[known-good](rollback-selection-rules.md#known-good-rule), and passes the full
bootability check. For `F`, the recorded fallback entry must agree with the
generation spec. `D` and `R` use their own spec entries. An unavailable
candidate allows the next check. An indeterminate candidate stops the walk
with the authority still pending and the boot target unchanged.

| Trial | Candidate outcome | Published default | `recoveryRequired` | Destination | Reboot |
| --- | --- | --- | --- | --- | --- |
| Confirmed | `F` or `D` usable | First usable candidate | False | Published default | One if unspent |
| Confirmed | All unavailable or absent | `C`, which is running | True | Empty | None |
| Unconfirmed | `F` or `D` usable | First usable candidate | False | Published default | None |
| Unconfirmed | `F` and `D` unavailable or absent, `R` usable | `R` | False | `R` | None |
| Unconfirmed | All unavailable or absent | `R`, which is running | True | Empty | None |
| Either | First indeterminate check | Unchanged | Unchanged | — | None |

Every completed resolution marks `C` failed and unhealthy, records it as
the failed boot, and clears the trial and one-shot state. The selection's
previous-known-good pointers name the usable fallback, or are cleared when
recovery is required. The default entry follows the selected generation's
loader binding and EFI follows the published selection. The resolving move
also publishes the running generation as the booted and active generation.

When recovery is required the owner publishes the running generation, with
`recoveryRequired`, an empty destination, and no reboot. A later healthy boot
of the published default clears `recoveryRequired` in the `ConfirmBoot` move. So
does a healthy boot of a known-good generation chosen from the boot menu, not a
selected target: it is accepted, becomes the default, and clears
`recoveryRequired`. Every other mutation stays refused until then, with that step
in the message. An indeterminate check keeps the trial pending. Boot health that fails
with no bound trial, including the first boot after install, is report-only and
sets no `recoveryRequired`.

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
which expects the legacy selection's revision. A trial armed by a runtime
without the owner has that reader-only runtime as its fallback. Durable import
is revisited when no supported rollback target is reader-only.

An arm interrupted before its one-shot is written resolves on a later boot
with the same reason as a trial that firmware booted, because firmware
consumes the one-shot on every boot and the state cannot tell the two
apart.

When a status change commits a generation, the owner keeps an existing
`committedAt` and `committedByOperation`, so live promotion does not overwrite
them.

## Atomic Writes

Every `selection.json` update uses same-filesystem temporary files, `fsync`,
atomic rename, and directory `fsync`. Temporary files are ignored during
recovery.

## Failure Handling

If power is lost after resources are staged but before the arm move writes its
intent, the generation is still only a candidate and is not selected for boot.

If power is lost after the arm move wrote its intent, reconciliation at the next
activation completes it from its first unfinished step, or resolves it as
unconfirmed when the candidate is not running and its issuing boot has ended.

If power is lost after the trial is armed but before reboot, the next boot
continues the bounded trial or reports that the trial was armed but not
started.

If a booted generation cannot prove `bootedGenerationID`, root PARTUUID, and spec
digest agreement, Katl must not promote or bless the boot.

If the trial fails on a boot bound to its authority, `FailTrial` persists the
failed trial and the fallback default and spends the reboot before
`katl-boot-recovery.service` or `katl-boot-deadman.service` requests a normal
systemd reboot by exiting successfully. Any other outcome, including a
report-only result, exits unsuccessfully so systemd leaves the node running.
The fallback is not a trial and is the final automatic attempt. If it also
fails, its failure is report-only and the node stays running for console
diagnosis instead of looping through reboots.

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
