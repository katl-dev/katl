# Generation Metadata Model

This decision defines the minimum generation spec and status records Katl needs
for first install and later A/B updates.

## Decision

Katl stores each generation as split spec/status records under:

```text
/var/lib/katl/generations/<generation-id>/spec.json
/var/lib/katl/generations/<generation-id>/status.json
```

`spec.json` selects one complete bootable generation: root slot, root artifact,
UKI, loader entry, sysext set, generated confext set, and kernel command line.
The selected artifacts are activated and rolled back together, but they do not
have to share one product version. A generation can combine a KatlOS runtime
root version with an independently versioned Kubernetes sysext as long as the
compatibility metadata says that pair is supported. Rollback must switch the
whole spec rather than independently switching root, sysext, and confext state.

`status.json` carries only mutable generation state. It is bound to `spec.json`
with a `specDigest` such as `sha256:<hex>`. Boot health, rollback, commit, and
explicit repair tooling may update status, but they must never rewrite selection
fields in `spec.json`.

## Required Fields

The first schemas are `katl.dev/v1alpha1`, `GenerationSpec` and
`GenerationStatus`.

Required `GenerationSpec` fields:

| Field | Purpose |
| --- | --- |
| `generationID` | Stable generation directory name under `/var/lib/katl/generations` |
| `runtimeVersion` | Human/runtime version used for compatibility checks and diagnostics |
| `previousGenerationID` | Prior generation this generation was planned from, when present |
| `root.slot` | Selected root slot, initially `root-a`; later `root-b` during A/B updates |
| `root.partitionUUID` | PARTUUID used by boot entries for the selected root partition |
| `root.runtimeArtifactSHA256` | Digest of the runtime root artifact written into the slot |
| `boot.ukiPath` | Canonical final installed UKI path selected with this generation |
| `boot.loaderEntryPath` | `$BOOT`-relative loader entry path when a separate loader entry is used |
| `sysexts[]` | Sysext name, generation-scoped path, activation path, digest, artifact version, payload version such as Kubernetes version, architecture, and compatibility metadata from the canonical sysext vocabulary |
| `confexts[]` | Generated confext name, path, activation path, digest, and compatibility metadata |
| `nodeExtensions[]` | Optional app extension selections, each bound to a staged node extension bundle, app sysext payload, generated app config digest, activation path, status path, health contract, and compatibility metadata |
| `kernelCommandLine[]` | Kernel arguments selected for this generation |
| `createdAt` | Generation creation timestamp |

Required `GenerationStatus` fields:

| Field | Purpose |
| --- | --- |
| `generationID` | Generation this status belongs to |
| `specDigest` | Canonical digest of the matching `spec.json` |
| `commitState` | Mutable candidate, committed, superseded, or abandoned acceptance state |
| `bootState` | Mutable pending, trying, good, or failed boot-attempt state |
| `healthState` | Mutable unknown, healthy, unhealthy, or deferred runtime health state |
| `updatedAt` | Last status update timestamp |
| `statusTransitions[]` | Optional bounded history of status changes, reasons, and operation IDs |
| `nodeExtensions[]` | Mutable observed status summary for selected app extensions, keyed by app ID and linked to operation records |
| `committedAt` | Present when `commitState` becomes committed |
| `committedByOperationID` | Operation that committed the generation, when present |

`commitState` records whether a generation is the accepted desired host state or
has been superseded or abandoned. `bootState` records only boot-attempt health.
A generation is known-good only when:

```text
commitState: committed | superseded
bootState: good
healthState: healthy
```

Mutable boot selection fields such as `defaultGenerationID`,
`trialGenerationID`, `previousKnownGoodGenerationID`, `bootedGenerationID`,
boot-counted trial UKI paths, and recovery flags belong under
`/var/lib/katl/boot/selection.json`, not in generation spec.

## Node Extension Selection

`nodeExtensions[]` is the typed generation-level view of optional app sysexts.
It exists in addition to `sysexts[]` so generic systemd extension activation can
stay mechanical while app-specific selection, configuration, status, health, and
rollback metadata remain explicit. A selected node extension also has a matching
`sysexts[]` entry for the staged app sysext payload and, when the app has
node-specific configuration, a matching `confexts[]` entry for the generated
app config.

Each `GenerationSpec.nodeExtensions[]` entry records immutable selection data:

```text
appID
artifactKind: katl.node-app-sysext.v1
artifactVersion
payloadVersion
sourceRef, normalized to <appID>/<payloadVersion>@sha256:<bundle-manifest-digest>
bundleManifestDigest
sysextPayloadDigest
architecture
capabilities[]
  name
  version
  configSchemaIDs[]
  operationKinds[]
runtimeCompatibility
  supportedRuntimeInterfaces[]
  minKatlOSVersion, when present
  maxKatlOSVersion, when present
  requiredKernelModules[]
  requiredUnits[]
  requiredMounts[]
  requiredCapabilities[]
stagedPayloadPath
activationPath
configDigest, when generated app config exists
configConfextName, when generated app config exists
configActivationPath, when generated app config exists
statusPath
operationSnapshotPathTemplate
readinessUnits[]
healthStates[]
rollback
  failClosedActions[]
  liveRollbackSupported
  requiresRebootForRollback
  externalStateWarning
```

`sourceRef` pins the node extension bundle manifest digest. The sysext payload
digest is the activation digest and must match the corresponding `sysexts[]`
entry. `configDigest` is the digest of the generated app-specific configuration
inside the selected confext; it is present only for apps with config handoff. A
selected app sysext without the required generated config is invalid unless the
bundle declares no required config handoff paths.

The first activation paths are generation-selected `/run` paths:

```text
/run/extensions/katl-node-extension-<appID>.raw
/run/confexts/katl-node-extension-<appID>-config.raw
/run/katl/apps/<appID>/status.json
/var/lib/katl/operations/<operation-id>/apps/<appID>/status.json
```

`activationPath` and `configActivationPath` are links or activation inputs
regenerated from the selected generation during boot or live activation. They
are not mutable desired state. The selected payloads remain under the
generation-scoped `sysext/` and `confext/` material or verified artifact cache
entries pinned by digest.

Katl validates node extension selections as part of the same compatibility set
as root, UKI, kernel command line, Kubernetes sysext, and generated confext.
Validation must prove:

```text
the bundle manifest digest and sysext payload digest match staged content
artifactKind is katl.node-app-sysext.v1
appID is unique within the generation
capability names and versions are supported by Katl
runtimeCompatibility accepts the selected KatlOS runtime interface
required kernel modules, units, mounts, and host capabilities are available
generated app config matches a supported config schema and selected digest
statusPath and operation snapshot paths are Katl-owned paths
readinessUnits and healthStates are declared by the selected bundle
```

Generation metadata must reject mutable extension state outside generation
records. In particular, Katl must reject:

```text
raw sysext paths supplied as desired config
global systemd extension directories as desired config
package-manager requests as extension selection
mutable latest tags without bundle digest pins
in-place rewrites of app sysext or app config activation paths
app-owned durable selection files such as /var/lib/katl/apps/<appID>/current
status files as authority for selecting a payload or config digest
```

`GenerationStatus.nodeExtensions[]` summarizes observed app extension state and
links it to durable operation evidence. It may contain only mutable health and
diagnostic fields:

```text
appID
healthState: unknown | healthy | unhealthy | deferred
readinessUnitStates[]
lastStatusPath
lastStatusDigest, when a redacted snapshot was captured
lastOperationID
operationSnapshotPath
failureReason, redacted
updatedAt
```

App live status under `/run/katl/apps/<appID>/status.json` is ephemeral runtime
state. Durable app status evidence belongs to the relevant `OperationRecord`
under `/var/lib/katl/operations/<operation-id>/apps/<appID>/status.json`.
Generation status may cache the latest health summary and operation link for
operator display, but rollback and known-good selection must validate
`spec.json`, `status.json.specDigest`, and the generation health fields rather
than trusting app status content.

## Generation 0

First install creates generation 0, the post-install KatlOS baseline:

```text
/var/lib/katl/generations/<generation-id>/
  manifest.json
  spec.json
  status.json
  confext/
  sysext/
```

The installer writes the runtime artifact to `root-a`, records the selected
root PARTUUID and runtime artifact digest, records generated confext metadata in
`spec.json`, and marks `status.json`:

```text
commitState: committed
bootState: pending
healthState: unknown
```

Runtime health completion later marks generation 0 `bootState: good` and
`healthState: healthy`. Generation 0 must boot the installed runtime, mount
writable state, expose operator access, store the user-supplied cluster intent
from the install manifest, and provide the Katl/systemd wiring needed to accept
later node-local operations. It is not Kubernetes-capable, does not activate
Kubernetes binaries, and does not run `kubeadm init` or `kubeadm join`.

Generation 0 validity requires a clean Kubernetes state boundary:

```text
kubelet disabled or absent from the active generation
no selected Kubernetes sysext
no Kubernetes PKI in /var/lib/katl/kubernetes/etc-kubernetes
no kubeadm static pod manifests
no kubeadm kubeconfigs
no stacked-etcd data in /var/lib/etcd
no kubelet join/bootstrap state in /var/lib/kubelet
no operation record proving kubeadm crossed a mutation boundary for this node
```

The backing directories for projected state may exist so mount units and
tmpfiles can be verified, but kubeadm-owned contents must not. If a failed
bootstrap or join created kubeadm-owned state, selecting generation 0 as host
state is not enough to make the node clean. `katlc` must require an explicit
reset, repair, recovery, or destructive wipe/reinstall path before treating the
node as a clean generation 0 bootstrap target again.

Generation 0 spec must not list a Kubernetes sysext unless that sysext is
actually selected and active for generation 0, which is not the day-one model.
The KatlOS install image does not bundle Kubernetes sysext candidates.
Generation 0 records bootstrap intent; a later `katlc` operation fetches and
stages the requested Kubernetes payload bundle from a user-supplied HTTPS source
before a Kubernetes-capable generation can select it.

The explicit cluster bootstrap or join operation asks `katlc` to validate the
stored intent and create a later generation, commonly described as generation 1.
That candidate generation selects the Kubernetes sysext, rendered kubeadm-ready
configuration, kubelet/containerd wiring, and `/etc/kubernetes` projection.

For first bootstrap or join, generation 1 selects the locally staged Kubernetes
sysext whose payload version exactly matches the install intent, for example
manifest version `v1.36.0` selecting a sysext fetched from a verified
`v1.36.0` payload bundle. The generation 1 spec stores the selected sysext path,
activation path, digest, artifact version, payload version, architecture, and
compatibility metadata defined in `docs/internal/installer-runtime-design.md`.
It remains a candidate until kubeadm succeeds and local post-kubeadm health
checks pass. At that point the
bootstrap or join operation may commit it as the accepted desired host state:

```text
commitState: committed
bootState: pending
healthState: unknown
```

That commit accepts the generation as desired host state, but it does not move
the persistent boot default and does not make the generation known-good.
Generation 1 becomes known-good only after a later boot reaches
`katl-boot-complete.target`, updates `bootState: good` and
`healthState: healthy`, and the boot-selection transaction promotes it.

The first install path does not need inactive-slot rollback because there is no
previous installed generation.

Boot attempt and health state transitions are defined in
`docs/internal/boot-health-semantics.md`.

## Status Mutation

Only `status.json` is mutable after the generation is created. These status
fields may change:

```text
commitState
bootState
healthState
statusTransitions[]
nodeExtensions[]
committedAt
committedByOperationID
```

All `spec.json` fields describe the selected root, UKI, command line, sysext
set, and generated confext set. Those fields must not be changed in place during
a normal update. A new desired runtime state gets a new generation directory.

Status writers must load `spec.json`, compute its canonical digest, verify that
the digest matches `status.json.specDigest`, validate the requested transition,
write a same-filesystem temporary status file, fsync it, rename it over
`status.json`, and fsync the generation directory. They must not rewrite
`spec.json` while updating status.

`commitState` records generation acceptance only. It does not record persistent
boot default selection or known-good promotion. `bootState` records boot trial
status only. It is not the live configuration apply phase. `healthState` records
generation boot health. Live apply progress, diagnostics, rollback attempts, and
external mutation boundaries belong in a `katlc`-owned `OperationRecord` under
`/var/lib/katl/operations/<operation-id>/`. A generation-local
`config-apply-status.json` may exist as a compatibility summary, but it is not
the authoritative recovery record.

Valid commit transitions are:

```text
candidate -> committed
candidate -> abandoned
committed -> superseded
```

Valid boot transitions are:

```text
pending -> trying -> good
pending | trying -> failed
```

A runtime config generation created for `live` starts with `commitState:
candidate`, `bootState: pending`, and `healthState: unknown`. If live activation
succeeds, its apply status may become live-active, but the generation is not
known-good until a boot health promotion marks it `good` and `healthy`. A
rollback target that is already `good/healthy` and `committed` or `superseded`
stays known-good; selecting it for rollback must not erase that status.

Explicit repair tooling may update only `status.json` in an existing generation,
and only through a durable operation record that records previous values, new
values, reason, and diagnostics. Repair tooling must not change root slot,
PARTUUID, UKI path, kernel command line, sysext selection, or confext selection
in place. If those fields are wrong or missing, Katl must create a new
generation, roll back to an existing valid generation, or refuse.

## Update And Apply Classification

Updates create a new generation directory before switching boot selection. A new
generation may change the runtime root, the Kubernetes sysext, selected node
extension sysexts, generated confexts, or any supported combination of those
artifacts. Runtime-root updates write the inactive root slot. Sysext-only,
node-extension-only, or confext-only updates may reuse the current root slot and
root artifact digest while selecting new extension content. KatlOS-only updates
may keep the existing Kubernetes sysext and node extension selections when those
payloads are compatible with the new runtime root. Kubernetes-only updates may
keep the existing KatlOS runtime root and node extension selections when the new
sysext is compatible with them.

The planner classifies each requested change before writing generation
artifacts:

```text
online in-place config apply
  Confext-only change that reuses the current root, UKI, kernel command line,
  and sysext set. The request creates a new generation, activates its generated
  confext in the current boot, runs only bounded domain live actions, and records
  a `config-apply` OperationRecord. This is the default accepted mode for
  `apply.mode: auto` when every diff is proven live-applicable.

next-boot config apply
  Confext-only change that is valid but boot-coupled, lockout-sensitive, or not
  proven safe online. The request creates a new generation and arms a bounded
  trial boot without changing the current boot.

host-upgrade
  KatlOS runtime root, UKI, kernel command line, or runtime image update. The
  request is an explicit host-upgrade operation and always stages a next-boot
  generation through the verified KatlOS image and sysupdate-backed transfer
  path.

Kubernetes payload upgrade
  Kubernetes sysext payload change on a bootstrapped node. The request is an
  explicit kubeadm-aware operation, not normal config apply. Until its gate is
  implemented and VM-tested, `katlc` rejects or records plan-only status.

operation-only lifecycle change
  Bootstrap, join, reset, repair, certificate renewal, etcd membership changes,
  and other workflows that run mutating tools. These require named operations
  and must not be hidden inside generation activation.

rejected input
  Unknown domains, unsupported fields, arbitrary /etc writes, raw extension
  activation paths, package-manager requests, host account ownership, or any
  ambiguous change where safety cannot be proven.
```

A Kubernetes upgrade operation may stage a candidate generation before every
service consumes it. That does not create an intermediate generation with mixed
Kubernetes tooling. The record still selects the target Kubernetes sysext as one
complete post-operation state; operation metadata controls when kubelet is
allowed to consume that payload. A candidate Kubernetes upgrade generation must
remain `commitState: candidate` with `healthState: unknown` or deferred until
the kubeadm phase and planned kubelet restart have completed and local health
checks pass. Mutable gate state for target kubelet activation belongs in the
operation record, not in `spec.json`.

Until a later ADR selects and tests the target kubeadm access mode and target
kubelet activation gate, `katlc` must reject or record plan-only Kubernetes
sysext changes on already bootstrapped nodes. It must not select a bootable
candidate, globally activate a target Kubernetes sysext, run kubeadm upgrade, or
restart kubelet for those requests.

Katl uses `systemd-sysupdate` as the default resource transfer and staging
primitive for KatlOS runtime root and UKI updates, but the Katl generation
spec/status remains authoritative for the complete selected runtime state. The
mapping is defined in
`docs/internal/systemd-sysupdate-update-decision.md`.

Generation activation should use systemd-native functions where practical:
systemd-boot for boot target selection, systemd-sysext and systemd-confext for
extension activation, native mount units for state projections, tmpfiles for
Katl-owned directory preparation, and boot health targets for known-good
promotion. Generation spec, status, and boot selection state are the Katl
coordination layer around those native mechanisms.

Boot selection updates are transactional state changes across
`/var/lib/katl/boot/selection.json`, systemd-boot one-shot or boot-counted
entries, and generation status. The write order and recovery rules are defined
in `docs/internal/boot-selection-transaction.md`.

Runtime configuration changes are confext-only generations unless they are
combined with an explicit root or sysext update. The apply-mode decision for
those generated confext changes is recorded in
`docs/internal/adrs/adr-002-live-and-next-boot-config-apply-modes.md`.

Those runtime changes start as Katl configuration applied to the installed node.
The node renders the generated confext locally and writes a new generation
record that selects the rendered confext together with the current or updated
sysext set. User-supplied runtime input does not directly name generated confext
activation paths or mutate the sysext activation set outside generation
specs.

`katlc` and KatlOS runtime services create generation spec/status only after
validation accepts the requested config. Unknown domains, unsupported fields,
unsupported sysext selection requests, and unsupported apply modes do not get
placeholder generation directories; they produce rejected request status only.

Before a candidate generation is made bootable, Katl must validate the selected
runtime root, UKI, Kubernetes sysext, node extension sysexts, generated confexts,
app config digests, status paths, readiness units, and health expectations as one
compatibility set. The generation spec stores the exact artifact digests and
versions that were validated, then asks the boot selector to try that generation.

Rollback returns to the previous generation spec and therefore restores:

```text
root slot
UKI path
kernel command line
sysext activation set
confext activation set
node extension selection and generated app config selection
```

Rollback selection rules are defined in
`docs/internal/rollback-selection-rules.md`.

## Deferred Fields

The first model intentionally defers:

```text
TPM measured boot state
verity metadata for generated confext images
per-file confext manifests
multi-architecture root type details
signed update metadata envelopes
operator-facing release notes
```

Those fields can be added in a later API version without changing the core rule
that a generation spec selects the complete runtime state as one unit.

## Legacy Metadata Compatibility

Older scaffolding may contain a single `metadata.json` file with immutable
selection fields and mutable status fields. New code should read both legacy
`metadata.json` and split `spec.json`/`status.json` during a bounded transition,
but new writes create split files.

On the first status write to a legacy generation, Katl should derive `spec.json`
from immutable fields, derive `status.json` from `commitState`, `bootState`, and
`healthState`, record `specDigest`, and then stop mutating `metadata.json`. If
rollback across the schema transition must support older roots, Katl needs a
dual-read transition release before split-only records become mandatory.
