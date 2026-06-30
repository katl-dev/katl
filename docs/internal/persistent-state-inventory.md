# Persistent Kubernetes and Node State Inventory

This decision records which node state must survive immutable root updates.
Katl keeps the runtime root read-only and versioned; persistent state belongs
under the writable state partition, normally mounted at `/var`. Paths outside
`/var` that must remain mutable are projected from state with explicit mount or
early-boot identity mechanisms.

## Decision

The writable state partition is the source of truth for persistent node state.
Native `/var` paths stay native. Mutable state under `/etc` is limited to
explicit projections for machine identity, Kubernetes PKI/kubeconfigs/static
pod manifests, and SSH host identity.

The initial on-disk `/var` layout is recorded in
`docs/internal/writable-state-layout.md`.

Generated confext owns steady-state configuration under `/etc`, but it must not
own kubeadm output, SSH host keys, or machine identity.

Katl-owned durable JSON records under `/var/lib/katl` use the self-describing
record envelope accepted in
`docs/internal/adrs/adr-008-persisted-katlos-state-records.md`:

```text
recordType
recordVersion
payload
writtenBy.katlVersion, optional diagnostic metadata
writtenBy.runtimeInterface, optional diagnostic metadata
writtenAt, optional diagnostic metadata
```

`recordType` is the Katl-owned discriminator. `recordVersion` is local to that
record type. `payload` is decoded only after the envelope has been decoded and
dispatched by type and version. The envelope is Katl-native node state, not a
Kubernetes-style API object; it does not imply `metadata`, universal
`spec`/`status`, namespaces, resource versions, managed fields, admission, or
watches.

The v0.1 persisted state format is JSON. Recovery-critical records must remain
inspectable with standard rescue tools. Binary protobuf may be used for a
future non-recovery-critical store only after a separate decision defines an
inspection tool and compatibility policy.

For the initial `recordVersion: 1` records, writer metadata is diagnostic. New
writers should populate `writtenBy.katlVersion`, `writtenBy.runtimeInterface`,
and `writtenAt` when that data is available, but readers must not require those
fields until a later record version explicitly makes them required.

## Record Contract Inventory

The following table is the release-stable inventory for Katl-owned persisted
record families. The payload owner names the Go package expected to own the
versioned payload decoder and semantic validation.

| Path pattern | recordType | recordVersion | Payload owner | Mutability | Digest rule | Migration policy | Rollback sensitivity | Test fixture path |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `/var/lib/katl/generations/<id>/spec.json` | `katl.generation.spec` | 1 | `internal/installer/generation` | Immutable after creation | `katl.generation.status.payload.specDigest` is computed from canonical generation spec payload bytes for this version | New selection semantics require a new generation record version and explicit old/new fixture coverage | High: rollback selection depends on this record | `internal/installer/persistedrecord/testdata/v1/katl.generation.spec.json` |
| `/var/lib/katl/generations/<id>/status.json` | `katl.generation.status` | 1 | `internal/installer/generation` | Mutable health and commit state only | Must carry the canonical digest of the matching generation spec payload | New commit, boot, health, or digest semantics require a new record version and rollback fixture | High: known-good and rollback eligibility depend on this record | `internal/installer/persistedrecord/testdata/v1/katl.generation.status.json` |
| `/var/lib/katl/generations/<id>/config-apply-status.json` | `katl.generation.config-apply-status` | 1 | `internal/installer/generation` | Mutable operation-facing status for one config apply generation | No standalone content digest; validates generation IDs, phase, and referenced diagnostic artifacts | New phase semantics, rollback result shape, or kubeadm action semantics require a new record version | Medium: config apply repair and live/next-boot reconciliation depend on it, but generation rollback authority remains spec/status/boot selection | `internal/installer/persistedrecord/testdata/v1/katl.generation.config-apply-status.json` |
| `/var/lib/katl/boot/selection.json` | `katl.boot.selection` | 1 | `internal/installer/generation` | Mutable boot transaction state | No standalone content digest; path, boot entry, and generation references must validate against generation records | New promotion, trial, boot-count, or recovery semantics require a new record version and VM rollback coverage | High: boot default, trial, previous known-good, and repair state depend on it | `internal/installer/persistedrecord/testdata/v1/katl.boot.selection.json` |
| `/var/lib/katl/install/status.json` | `katl.install.status` | 1 | `internal/installer/status` | Mutable installer and runtime handoff summary | Contains request and image digests; no separate record digest | New state machine meanings or handoff semantics require a new record version | Medium: first-install repair and handoff diagnostics depend on it; normal generation rollback does not | `internal/installer/persistedrecord/testdata/v1/katl.install.status.json` |
| `/var/lib/katl/operations/<id>/record.json` | `katl.operation.record` | 1 | `internal/installer/operation` | Mutable operation snapshot rebuilt from journal | Must agree with latest valid journal event, latest sequence, and journal digest | New operation recovery, mutation marker, or repair semantics require a new record version and journal fixture | High: interruption recovery and repair classification depend on it | `internal/installer/persistedrecord/testdata/v1/katl.operation.record.json` |
| `/var/lib/katl/operations/<id>/journal/<seq>.<event>.json` | `katl.operation.journal-event` | 1 | `internal/installer/operation` | Append-only | Ordered canonical event bytes feed the operation journal digest recorded in `record.json` | New event shape or replay semantics require a new record version; old event fixtures must keep replaying | High: operation recovery source of truth | `internal/installer/persistedrecord/testdata/v1/katl.operation.journal-event.json` |
| `/var/lib/katl/cluster/intent.json` | `katl.cluster.intent` | 1 | `internal/installer` | Immutable install-normalized cluster bootstrap intent until an explicit future reintent operation exists | Source request and KatlOS image digests tie the payload to install input; no separate record digest | New bootstrap intent semantics require a new record version and bootstrap compatibility fixture | Medium: bootstrap depends on it; host rollback must not mutate it | `internal/installer/persistedrecord/testdata/v1/katl.cluster.intent.json` |
| `/var/lib/katl/config-requests/<source>/<version>.json` | `katl.config-request.decision` | 1 | `internal/installer/configapply` | Immutable decision for one source/version request | `payload.requestDigest` binds the decision to the submitted config | New decision, freshness, or apply-mode semantics require a new record version | Medium: idempotent config apply and repair diagnostics depend on it | `internal/installer/persistedrecord/testdata/v1/katl.config-request.decision.json` |

The fixture paths above are the canonical locations for released compatibility
fixtures. The common persisted-record package owns envelope fixtures and
negative envelope tests. Payload owners own valid payload fixtures and
record-specific semantic failures.

Installer files under `/var/lib/katl/install/` other than `status.json` are
operation attachments, input copies, plans, or diagnostics until a future
decision promotes them into release-stable record contracts. Node app snapshots
under `/var/lib/katl/operations/<id>/apps/<appID>/status.json` are app-owned
operation evidence identified by each app bundle's status schema ID; they are
not Katl core record envelopes unless a later node app contract says so.

## Decoding And Unknown Fields

Readers must process Katl record files in this order:

```text
decode the envelope as JSON
reject missing recordType, missing recordVersion, missing payload, unsupported
  recordType, or unsupported recordVersion with a recovery-safe diagnostic
reject unknown top-level envelope fields unless a future envelope version
  explicitly defines an extension field
dispatch by recordType and recordVersion
decode payload with the selected versioned decoder
reject unknown payload fields unless that record version explicitly documents an
  extension or annotations map
validate path context, payload identity, digests, transitions, and semantic
  constraints
```

Readers must not silently rewrite records just because they were read. Writers
must write canonical JSON and use atomic replace for mutable records. Immutable
records use create-without-replace semantics after path validation.

## Migration Policy

Changing a persisted record contract requires a concrete migration decision. A
record version changes when a payload field is added, removed, renamed, changes
meaning, changes type, or gains a new enum value whose reader behavior was not
already defined.

Every migration must:

```text
name the affected recordType and old/new recordVersion values
state whether old records remain readable and for how long
state whether rollback to the previous known-good runtime remains compatible
add valid fixtures for old and new record versions
add negative fixtures for unsupported versions and malformed payloads
define canonical digest bytes for each affected version
write records atomically and preserve unknown records it does not own
record migration outcome as an operation when node state is mutated
run the relevant VM gate when boot selection, rollback, operation recovery,
  install handoff, kubeadm state, or destructive reset behavior is affected
```

Rollback-sensitive migrations must prove that a failed trial runtime can still
select the previous known-good generation or reach a clear repair state. A
trial runtime must not write a record version that the previous known-good
runtime cannot read well enough to roll back or report repair unless the
operation explicitly declares rollback compatibility broken and has a tested
repair path.

## Schema Change Checklist

Before adding or changing a Katl persisted record:

```text
update ADR-008 or this inventory with the recordType, path, owner, and version
add or update the versioned payload decoder
add valid fixtures under internal/installer/persistedrecord/testdata/v1/
add negative fixtures for missing payload, unsupported version, unknown fields,
  malformed timestamps, and path/type mismatch where applicable
define canonical digest bytes and update any status or journal digest checks
define migration behavior from every released earlier version
run go test for the payload owner and the common persisted-record package
run VM gates when rollback-sensitive, boot-sensitive, disk-layout-sensitive,
  destructive-reset-sensitive, or kubeadm-state-sensitive behavior changes
```

## Separate Contracts

These are not Katl persisted record-envelope contracts:

```text
user-authored Katl source config
Katl config bundle archives
compiled per-node install material
KatlOS image artifact metadata
Kubernetes and node extension bundle manifests
node app durable snapshots identified by app status schema IDs
the katlc protobuf agent API
kubeadm-owned PKI, kubeconfigs, static pod manifests, kubelet state, etcd data,
  and Kubernetes API objects
```

Each has its own compatibility policy. Persisted Katl node state may reference
those contracts by digest, path, schema ID, or bundle identity, but it does not
inherit their versioning rules.

## State Layout Inventory

| Path | Owner | Mutability | Placement |
| --- | --- | --- | --- |
| `/var/lib/katl` | Katl installer, `katlc`, KatlOS runtime services | Mutable generation status, operation records, staged artifacts, activation records, and repair status | Native `/var`; primary Katl state root |
| `/var/lib/katl/operations` | Katl installer, `katlc`, KatlOS runtime services | Durable node-local operation records that distinguish host repair from kubeadm or etcd repair | Native `/var`; not generation artifacts and not activated through sysext/confext |
| `/var/lib/katl/operations/<id>/journal` | Katl installer, `katlc`, KatlOS runtime services | Append-only durable operation events used to rebuild `record.json` after interruption | Native `/var`; operation recovery source of truth |
| `/var/lib/katl/operations/<id>/apps/<appID>/status.json` | `katlc`, KatlOS runtime services | Redacted snapshot of selected node app sysext runtime status when an operation needs durable evidence | Native `/var`; copied from bounded live app status and owned by the operation record |
| `/var/lib/katl/cluster/intent.json` | `katlos-install`, `katlc` | Normalized non-secret cluster intent from install input: desired node role, selected Kubernetes payload version, selected bootstrap profile reference, and endpoint intent | Native `/var`; generation 0 input for later bootstrap, not Kubernetes cluster state |
| `/var/lib/katl/config-requests` | `katlc`, KatlOS runtime services | Request decision index for accepted or rejected node configuration inputs | Native `/var`; links to canonical operation IDs when a request is accepted |
| `/var/lib/katl/boot/selection.json` | Katl installer, `katlc`, KatlOS runtime services | Durable default, trial, previous known-good, and booted generation pointers | Native `/var`; boot selection state outside generation directories |
| `/var/lib/katl/generations/<id>/spec.json` | Katl installer, `katlc`, KatlOS runtime services | Immutable generation selection fields after creation | Native `/var`; selected by boot metadata |
| `/var/lib/katl/generations/<id>/status.json` | Katl installer, `katlc`, KatlOS runtime services | Mutable commitState, bootState, and healthState bound to spec by `specDigest` | Native `/var`; known-good eligibility after digest validation |
| `/var/lib/katl/generations/<id>/confext` | Katl installer, `katlc`, KatlOS runtime services | Immutable generated configuration for that generation | Native `/var`; exposed at boot through selected `/run/confexts` activation path |
| `/var/lib/katl/generations/<id>/sysext` | Katl installer, `katlc`, KatlOS runtime services | Immutable extension artifacts for that generation | Native `/var`; exposed at boot through selected `/run/extensions` activation path |
| `/var/lib/katl/identity/machine-id` | Katl installer, systemd | Random install-generated node identity; stable across boots and updates; write-protected after install | Native `/var`; generation 0 passes this value through the selected loader entry with `systemd.machine_id=` |
| `/etc/machine-id` | systemd and D-Bus consumers | Stable identity, read very early | Must resolve to the `/var/lib/katl/identity/machine-id` value at runtime; not user supplied or deterministic across reinstalls; a late service is not sufficient |
| `/var/lib/katl/ssh/host-keys` | sshd, Katl installer | Stable SSH host identity | Native `/var`; projected into `/etc/ssh` before sshd starts |
| `/etc/ssh/ssh_host_*` | sshd | Stable host keys | Project from `/var/lib/katl/ssh/host-keys`; generated confext may own sshd config snippets but not host keys |
| `/etc/ssh/sshd_config.d/*.conf` | Katl renderer/confext | Katl-owned steady-state SSH policy | Generated by Katl; not user-supplied mutable node state |
| `/etc/passwd`, `/etc/shadow`, `/etc/group`, `/etc/sudoers*`, `/etc/pam.d/*`, `/etc/sysusers.d/*` | Katl/base packages | Katl-owned host identity and auth policy | Base root or Katl-generated policy; user-supplied confext input must not own these paths |
| `/etc/hostname` | Katl config/confext | Generated node config | Generated confext; change through a new configuration generation |
| `/etc/systemd/network/*.network` | Katl config/confext, systemd-networkd | Generated network config | Generated confext; change through a new configuration generation |
| `/etc/katl/*` | Katl config/confext | Generated Katl and kubeadm input config | Generated confext; kubeadm output stays elsewhere |
| `/etc/kubernetes` | kubeadm, kubelet | Mutable kubeadm output, PKI, kubeconfigs, and static pod manifests | Project from `/var/lib/katl/kubernetes/etc-kubernetes`; Katl owns the projection, not the contents |
| `/var/lib/katl/kubernetes/etc-kubernetes` | kubeadm, kubelet | Persistent backing store for kubeadm-owned `/etc/kubernetes` | Native `/var`; bind-mounted to `/etc/kubernetes` before kubelet/kubeadm automation |
| `/var/lib/kubelet` | kubelet, with kubeadm-written bootstrap/config files | Mutable pod state, plugin state, checkpoints, kubelet config, and `kubeadm-flags.env` | Native `/var`; persistent across root updates |
| `/var/lib/containerd` | containerd, kubelet | Mutable image/content/snapshot/container state | Native `/var`; persistent across root updates |
| `/var/lib/etcd` | etcd static pod, kubeadm | Mutable control-plane database | Native `/var` by default or optional dedicated partition mounted at `/var/lib/etcd` |
| `/var/log/journal` | systemd-journald | Optional persistent logs | Native `/var`; may be absent when persistent journald is disabled |
| `/run` | systemd, Katl runtime selector, services | Ephemeral boot state only | Never persistent; `katl-generation-activate.service` regenerates selected extension links, app live status roots, locks, and operation gates each boot |
| `/tmp` | applications | Ephemeral | Never persistent |

Katl-rendered kubeadm/kubelet input under `/etc/katl` may drift from these live
paths after bootstrap, join, upgrade, or manual kubeadm repair. That drift is not
a Katl confext ownership conflict. Katl may report it, but only explicit
kubeadm-aware operations may mutate the live files or kube-system ConfigMaps.

Cluster intent and cluster state are separate. `/var/lib/katl/cluster/intent.json`
is Katl-owned desired input for a future bootstrap or join operation. Cluster
PKI, service account keys, kubeconfigs, bootstrap tokens, uploaded certificate
material, etcd identity, etcd data, and Kubernetes API objects are cluster state
created by kubeadm/Kubernetes/etcd. They are not generation artifacts, not stored
in generation 0, and not recovered from Katl operation records. Their ownership
and backup boundary is defined in
`docs/internal/cluster-bootstrap-state-model.md`.

`katlctl` does not own `/var/lib/katl` state. It may keep local client
configuration for connection profiles and known node details, but node lifecycle
state, operation records, generation records, health state, machine identity, and
boot-selection state live on the node and are owned by `katlc`, `katlos-install`,
or KatlOS runtime services.

## Rollback Boundary For Persistent Kubernetes State

Persistent Kubernetes state is mounted or native writable state, not selected by
generation spec. `/etc/kubernetes`, its backing path,
`/var/lib/kubelet`, `/var/lib/etcd`, and Kubernetes API objects survive host
generation rollback.

Generation rollback changes host artifacts around that state: root, UKI, kernel
command line, sysext activation set, and confext activation set. It does not
rewind kubeadm output, kubelet runtime files, etcd contents, etcd member records,
or cluster API objects.

## Service Ordering Implications

Katl must establish persistent identity and projected state before dependent
services read it:

```text
state partition mounted at /var
machine identity available to systemd
katl-generation-activate.service creates selected sysext/confext activation paths under /run
systemd-sysext.service
systemd-confext.service
katlc-agent.service startup audit
/etc/kubernetes bind mount active
/etc/ssh host key projection active
systemd-networkd.service
sshd.service
containerd.service
kubelet.service
kubeadm child processes launched by the katlc agent executor
```

The `/etc/kubernetes` bind must be validated after confext activation so a
confext overlay cannot hide the persistent Kubernetes subtree.

This ordering is dependency ordering for services that exist in a selected
generation, not a generation 0 boot-health checklist. `/etc/kubernetes`,
containerd, kubelet, and kubeadm automation become required only for
kubeadm-ready generations.

`/run` activation links are derived state. Every boot recreates them from the
generation selected by boot metadata and `/var/lib/katl/boot/selection.json`,
then validates the selected generation spec and status before systemd-sysext or
systemd-confext consumes the links.

The concrete first implementation rules are:

| Path | Mount behavior | Dependent services |
| --- | --- | --- |
| `/var` | `KATL_STATE` mounted by `var.mount` using the installed PARTUUID | All persistent node services |
| `/var/lib/containerd` | Native directory on `/var`, no bind mount | `containerd.service` orders after `var.mount` and uses `RequiresMountsFor=/var/lib/containerd` |
| `/var/lib/kubelet` | Native directory on `/var`, no bind mount | `kubelet.service` orders after `var.mount`, `containerd.service`, and `etc-kubernetes.mount` |
| `/var/lib/etcd` | Native directory on `/var` by default; optional `var-lib-etcd.mount` for a Katl-owned `KATL_ETCD` partition | Kubelet, kubeadm automation, and `katl-kubeadm-ready.target` order after the optional mount when present |
| `/etc/kubernetes` | Bind mount from `/var/lib/katl/kubernetes/etc-kubernetes` with `etc-kubernetes.mount` | Kubelet, kubeadm automation, and `katl-kubeadm-ready.target` order after the projection |

The detailed mount rules are recorded in
`docs/internal/writable-state-layout.md`. The focused `/etc/kubernetes`
projection decision is recorded in
`docs/internal/etc-kubernetes-projection.md`.

## Deferred Details

The exact generated unit file contents belong in the follow-up mount unit
implementation tasks. VM validation must prove that `/etc/machine-id`,
`/etc/kubernetes`, SSH host keys, native `/var` service state, and selected
extension activation paths are present before their consumers start.
