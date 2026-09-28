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
