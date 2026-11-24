# Storage Identity Ownership

Status: decided design; migration in progress (Bead katl-ma0s).

This document defines how Katl selects, binds, addresses, revalidates and
persists the identity of the storage it changes: the system disk and its
partitions, and data volumes. It applies to install, reruns and reinstalls,
live volume changes, every later boot, host upgrade and rollback.

## Requirement

Every destructive or persistent storage operation acts only on storage that the
operator selected and Katl bound. When identity is not unique, Katl refuses
before changing anything and tells the operator what to do next. Callers must
be structurally unable to break this rule, not merely expected to follow it.

The rule exists because one failure class recurred across several
implementations. Storage that had already been bound was later found again by
GPT label, by kernel name, or from the current attempt's state alone. Separate
implementations of one responsibility also applied different rules: install
planned volumes together while live apply planned them one at a time, and
three runtime paths each derived the system disk in their own way.

## Invariants

1. A selection resolves to exactly one disk, or Katl refuses and lists the
   candidates. This happens before any mutation.
2. Destructive commands accept only bound `storage.Disk` or `storage.Partition`
   values. Each one is revalidated against fresh discovery facts and addressed
   through `/dev/disk/by-diskseq` immediately before the write.
3. One planner sees the system disk, every kept volume binding and every
   desired volume. Install and live apply both use it, so a conflict refused
   at install is refused live as well.
4. Bindings made within an installer boot or a live apply only accumulate. A
   retry reuses them or refuses; it never resolves a selector again in their
   place.
5. A volume binding mounts only if it resolves by the binding rule below.
   Otherwise the volume stays unmounted and the operator receives guidance.
6. Wherever the system disk is needed, it is derived from the partition
   backing `/` and is never stored.
7. Recovery never restarts or remounts a volume that a destructive command has
   touched.

## Binding rule

A recorded volume binding resolves only when all of these hold:

- Exactly one partition across all attached disks carries the recorded
  PARTUUID.
- That partition is not on the system disk.
- Where a filesystem UUID is recorded, the partition's filesystem UUID matches
  it.

Each destructive step Katl performs changes a volume's PARTUUID or its
filesystem UUID: repartitioning, mkfs and wipefs all do. A stale binding is
therefore visible on the disk itself, even when a crash interrupts the step
before the generation commits. Katl persists no additional identity, such as a
hardware anchor or a ledger.

## Ownership

`internal/storage` owns binding, planning, revalidation and every destructive
storage command. `internal/installer/discovery` keeps producing facts and
matching selectors. `internal/systemdisk` stays a sysfs-only resolver for the
boot-time generator.

Exported surface:

- `Disk`, `Partition` and `System`, with unexported fields. Only the package
  can construct them.
- `Booted(facts) (System, error)`, which wraps `systemdisk.Resolve`.
- `PlanInstall(facts, selector, retained)`.
- `Plan(facts, System, kept []Binding, desired []VolumeRequest) (Plan, error)`.
- `(Plan) Execute(ctx, runner) (Result, error)`. The `Result` carries the new
  bindings and the set of touched volumes.
- `(Plan) SameTargets(Plan) bool`.
- `Locate(facts, Binding) (Partition, error)`.
- `(System) PrepareSlots(ctx, runner)`, used by host upgrade.
- `Binding`, the single volume binding type. It keeps today's JSON and is
  referenced by `generation.Record`.

The writer is package-private. It revalidates the target, renders the
diskseq path and runs the command. Claims, uniqueness checks and binding
matching are also package-private.

Execution re-plans from fresh facts with the same function and refuses if the
bound targets differ from those that were validated. Plans hold per-boot
diskseq values, so they are never stored. An operation record keeps only a
summary of the targets.

## Identity across boots

- **System disk.** The loader entry's `root=PARTUUID=` anchors the #453
  `katl-system-disk-generator`. The generator mounts `/efi` and `/var` from the
  disk holding the running root, by diskseq. It stops the boot before `/var`
  is mounted when that disk is ambiguous, shares its PARTUUIDs with another
  disk, or is not the disk firmware booted from. The agent and host upgrade
  use `storage.Booted` and no other derivation.
- **Volumes.** Generations keep their `VolumeBindings` and render static
  `PARTUUID=`/`UUID=` mount units into the confext. Before
  `katl-volumes-activate.service` starts `katl-volumes.target`, its
  `ExecStartPre=` runs a Go check that resolves every binding of the active
  generation, the one the generated confext links, by the binding rule. On
  failure the check names the volume, the cause and the remedy. Boot, live apply and rollback all pass through this
  unit. Katl accepts the residual risk of a clone being hot-plugged between
  the check and the mount within one start of the unit. Boot health orders
  after the unit with `Wants=`: a refused volume is reported as failed boot
  health in node status without marking the generation failed, since rolling
  back cannot change which disks are attached; see
  `docs/internal/boot-health-semantics.md`.
- **Rollback.** Preflight runs `Locate` on the target generation's bindings
  and refuses before switching when any of them does not resolve. If an
  automatic fallback reaches such a generation anyway, the activation gate
  leaves that volume unmounted, node status reports boot health failed, and
  the node stays reachable through `katlctl`.
- **gpt-auto.** Every loader entry carries `systemd.gpt_auto=no`
  (`internal/generation/loader.go`), so systemd's gpt-auto generator takes no
  part.

## Compatibility with v2026.11.1 and earlier

The binding schema and the mount units are unchanged, so existing volumes keep
mounting. The only new behaviour is a refusal when an identifier is duplicated
or stale, which the release notes must state. The system disk needs no
migration, and nothing new is required before `/var` is mounted.

Rolling back to a runtime that predates the activation gate runs that
runtime's own ungated units. This is accepted and documented.

A volume disk replaced by a dd clone, with the original detached, mounts
exactly as a cloned system disk does.

## Rejected alternatives

- **A per-node ledger with write-ahead consumption.** The ledger would store
  what the disk already shows. It would add a second authority, epochs,
  seeding from the generation, and a recovery journey for a lost `/var`.
- **A durable hardware anchor in each binding.** Hardware identity is weak on
  virtio disks, and the anchor would refuse a legitimate replacement of a
  failed disk.
- **Boot-time bind services with `/run` symlinks, or generated `/run` mount
  units.** A `What=` outside `/dev` loses the device-unit dependency, and
  generating the units needs an extra daemon-reload and moves volume mounts
  out of the confext.
- **Destructive acknowledgements keyed to resolved identity.** This changes
  the katlctl and agent API to close a gap that the binding rule closes
  without any interface change.
- **Stored plans.** A stored plan holds per-boot diskseq values and skips the
  recheck against fresh facts.
- **gpt-auto for `/var`.** It is disabled on every loader entry, and it covers
  neither data volumes nor a root selected by PARTUUID.

## Enforcement

The types are the main enforcement. A lint test adds a backstop. It fails the
build when non-test Go outside `internal/storage` names `wipefs`, `sfdisk`,
`mkfs.*`, `systemd-repart`, `blkdiscard` or `katlos-write-root-slot` in an
argv. It also fails when non-test Go or shipped unit trees contain
`by-partlabel`, `PARTLABEL=` or `LABEL=`. Sysupdate transfer definitions are
allowlisted. Kernel-name patterns are not linted, because the development
runtime legitimately boots from `root=/dev/vda`.

## Contract tests

Each test names the wrong implementation it is meant to catch.

| Test | Catches |
| --- | --- |
| A new wipe volume resolves to a kept volume's disk; install and live apply give the same refusal, from one table | Per-volume planning; planner drift |
| `Locate` with the PARTUUID present on two disks | Taking the first match |
| `Locate` with a matching PARTUUID but a different filesystem UUID | PARTUUID-only checks; stale rollback; crash after mkfs |
| `Locate` with a matching partition on the system disk | Claims that ignore the system disk |
| A kernel name belongs to a different diskseq at write time | Path addressing |
| Failure after mkfs on volume `a`: `a` is reported and no unit for it is started | Restarting everything that was stopped |
| An install rerun whose volume selector now matches another disk | Selector re-resolution |
| Desired equals current produces no steps; `wipe: true` on a bound volume does not wipe again | A non-idempotent diff |
| Host upgrade with a decoy Katl disk and with a clone of the root | A global PARTUUID lookup |
| The lint test | New code that bypasses the package |

VM coverage keeps `TestInstallerReinstallBesideOldKatlDisk` and the host
upgrade decoy-disk test, and adds:

- boot with a cloned volume disk attached: volumes stay unmounted, the node is
  reachable and guidance is shown;
- rollback after a volume replacement: refused;
- upgrade of a v2026.11.1 node that has volumes.

## Migration

These steps gate the v2026.11.2 release. They close every known path to data
loss.

1. Land #453 (system-disk generator and PARTUUID addressing after
   partitioning).
2. Merge #457 with its shared planner and touched-not-restored fixes, as an
   interim safety step.
3. Add the activation gate. `matchVolumeBinding` requires both identifiers
   and excludes the system disk, the activation unit gains its
   `ExecStartPre=` check, and rollback preflight runs the same check. Confirm
   on a node installed by v2026.11.1 that no `/etc` copy shadows the `/usr`
   unit.
4. Move the agent and host upgrade onto `systemdisk.Resolve`, deleting
   `runningSystemDisk` and `inspectRootSlots` and addressing the root-slot
   relabel by diskseq.
5. Extend the installer's retained rerun binding to volume disks.

These steps follow the release and keep the invariants closed:

6. Extract `internal/storage` with the types and a lint allowlist, with no
   behaviour change.
7. Refuse at execution when the targets differ from those validated.
8. Empty the lint allowlist and delete the duplicate types and helpers.

## Out of scope

- `KATL_ETCD` is planned in the disk layout but has no runtime mount. Its
  ownership needs its own decision.
- Node address roles, the subject of #455, follow the same approach in a
  separate design.
