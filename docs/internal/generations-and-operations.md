# Katl Generations And Operations

Status: working design.

This document defines the shared lifecycle model for Katl node state and
stateful actions.

## Summary

Katl models node lifecycle through two complementary concepts:

```text
Generations
  Declarative, versioned, rollback-aware desired host state.

Operations
  Explicit, auditable, transactional actions required to make reality match
  desired state.
```

This separation lets Katl stay systemd-native and generation-based without
building a Talos-style machine controller or reimplementing Kubernetes lifecycle
management.

## Motivation

Katl uses systemd-native mechanisms such as systemd-boot, sysext, confext,
mount units, boot health checks, and generation activation to manage host
state. These mechanisms work well for operating system configuration, installed
capabilities, and node-level services.

Some lifecycle transitions are different. Kubernetes bootstrap, node join,
certificate renewal, control-plane repair, etcd membership changes, and
Kubernetes version upgrades mutate persistent node or cluster state through
tools such as kubeadm. They cannot safely be treated as simple configuration
changes or hidden inside confext activation.

Katl therefore keeps declarative host state in generations and models
transactional workflows as operations.

## Generations

A generation describes the desired state of a node.

Examples include:

```text
KatlOS runtime version
kernel and boot artifacts
enabled sysexts
rendered confext configuration
selected Kubernetes sysext version
host networking configuration
container runtime configuration
node role and capabilities
health expectations
```

A generation answers:

```text
What should this machine look like?
```

Generations are versioned, health-checked, and rollback-aware. Rollback selects
a complete previous generation; it must not independently switch only the root
slot, sysext set, or confext set.

The initial installed baseline is generation 0. `katlos-install` creates it
after validating the install request, writing the runtime root, installing boot
artifacts, preparing writable state, and seeding enough systemd wiring for the
installed runtime to accept node-local operations.

Generation 0 is intentionally not a Kubernetes cluster member. It is the
post-install KatlOS baseline.

Generation 0 may have verified access to bundled Kubernetes sysext artifacts from
the KatlOS image, and it records user-supplied cluster intent from the install
manifest. It does not activate Kubernetes binaries, create `/etc/kubernetes`, run
kubeadm, or create cluster state. The first Kubernetes-capable generation is
created by an explicit bootstrap or join operation.

## Generation Lifecycle Terms

The shared lifecycle uses these terms:

```text
candidate generation
  a validated generation spec whose immutable selection fields exist, but whose
  status still has commitState candidate

selected generation
  the generation chosen for one execution context: next boot, current boot, live
  apply, or rollback; selection is not commit

active generation
  the generation whose selected root, UKI, sysext, confext, and state projections
  are currently realized by the running system

live-active generation
  a generation whose sysext/confext/state projections are realized in the current
  boot by a node-local operation; live-active is not boot health

committed generation
  a generation with commitState committed; it is the persistent desired host
  state for future boots, but it may still have bootState pending

known-good generation
  a committed or superseded generation whose status has bootState good and
  healthState healthy

activation
  realizing a selected generation through systemd-boot, /run extension links,
  confext links, mount units, and native systemd services

health promotion
  changing a tried boot generation to bootState good and healthState healthy
  after katl-boot-complete.target

rollback
  selecting a previous known-good generation spec as a complete unit
```

## Operations

An operation represents a stateful workflow required to transition the node,
host capability set, or Kubernetes cluster state.

Examples include:

```text
BootstrapCluster
JoinCluster
UpgradeControlPlane
UpgradeWorker
RenewCertificates
ResetNode
ReplaceEtcdMember
```

An operation answers:

```text
What action must occur to reach the desired state?
```

Operations are explicit. Normal configuration apply and generation activation
must not silently run kubeadm, kubectl, CNI installers, GitOps controllers,
package managers, or cluster repair commands.

Normal `katlc apply` is the generation apply path. Creating or activating the
first Kubernetes-capable generation during cluster bootstrap is still generation
management, but it is part of the explicit `BootstrapCluster` or `JoinCluster`
operation because kubeadm will mutate node or cluster state before the generation
can be committed. Named operations are reserved for transactional workflows that
run mutating tools such as kubeadm, coordinate external state, or repair state
outside normal generation apply.

`BootstrapCluster` and `JoinCluster` are operation types initiated by
`katlctl cluster bootstrap`. For bounded multi-node workflows, the durable record
may be a coordinator run record rather than only a node-local systemd operation
record. A coordinator run record must capture per-node operation attempts,
candidate generation IDs, phase state, redacted diagnostics, and whether kubeadm
has mutated node or cluster state.

## Operation Records

An `OperationRecord` is the canonical durable run record for an accepted Katl
lifecycle workflow. Terms such as run record, checkpoint, config-apply status,
and upgrade operation record are workflow-specific storage views of the same
model, not separate lifecycle models.

One record tracks one explicit attempt from request acceptance to terminal
result. Records may be node-local or coordinator-scoped. A multi-node `katlctl`
workflow may have one coordinator record plus linked node-local records for each
node mutation.

Common fields:

```text
apiVersion
kind: OperationRecord
schemaVersion
operationID
operationKind
scope: install-state | host-generation | kubeadm-state | etcd-state |
  destructive-reset | coordinator
parentOperationID, when present
coordinatorOperationID, when present
actor
requestDigest
recordRevision
latestJournalSeq
phasePlan[]
previousGenerationID, when present
candidateGenerationID, when present
phase
phaseIndex
completedPhases[]
terminal
resourceLocks[]
invocations[]
  invocationID
  systemdInvocationID
  unitName
  startedAt
  completedAt
  result
externalMutationStarted
preExecMutationMarkers[]
  markerID
  invocationID
  phase
  tool
  argvDigest
  expectedMutationScopes[]
  markedAt
mutationScopes[]
mutatingToolRan
mutatingToolInvocations[]
diagnosticArtifacts[]
hostRollback
postMutationRollbackAllowed
recoveryRequired
retryHint
interruption
resume
nextAction
result
createdAt
updatedAt
completedAt
failureReason, redacted
```

Common evidence fields:

```text
evidenceVersion
nodeIdentity:
  inventoryNodeName
  hostStaticHostname
  kubeadmNodeRegistrationName
  observedAPINodeName
  observedAPINodeUID
kubeadmEvidence:
  subcommand
  observedVersion
  configPath
  configDigest
  currentPhase
  completedPhases[]
  firstMutationPhase
  exitStatus
  redactedOutputArtifact
apiEvidence:
  endpoint
  endpointSource
  tcpReachable
  tlsVerified
  livez
  readyz
  observedServerVersion
  lastError, redacted
staticPodManifestEvidence:
  before[]
  after[]
etcdMemberEvidence:
  before[]
  after[]
  addedMemberIDs[]
  removedMemberIDs[]
  localMemberID
  probeSource
  probeError, redacted
redactionEvidence:
  rulesVersion
  redactedKinds[]
  sensitiveMaterialPresent[]
```

Evidence is diagnostic and recovery input, not the source of truth. Retry and
repair operations must re-probe live node state, Kubernetes API state, static pod
manifests, kubelet state, and etcd state before deciding what to skip, rerun, or
repair.

All `OperationRecord` updates are journal-first. Katl writes
`/var/lib/katl/operations/<id>/journal/<seq>.<event-id>.json`, fsyncs the file
and journal directory, then atomically replaces `record.json` as a recoverable
snapshot and fsyncs the operation directory. Recovery ignores temporary files and
rebuilds `record.json` from the highest valid journal sequence when the snapshot
is missing, stale, or digest-invalid.

Phase updates are monotonic:

```text
phaseIndex may stay the same or advance; it must not decrease
completedPhases[] is append-only
terminal result fields are immutable once written
externalMutationStarted may only change false -> true
mutationScopes[] and preExecMutationMarkers[] are append-only
```

Before invoking any tool that may mutate disk, kubeadm-owned node state, etcd,
or Kubernetes API state, Katl must durably write and fsync a pre-exec mutation
marker. After that marker exists, recovery must classify the operation as
post-mutation or mutation-unknown unless later evidence proves a safer state.

This shared model normalizes lifecycle vocabulary. Workflow-specific storage
views such as install checkpoints, config apply status, bootstrap run records,
and upgrade records must conform to this model once they become durable.

## Command And System Boundaries

`katlc` is the node-local authority. It validates node-local input, compiles or
selects candidate generations, plans operation records, launches
systemd-supervised operation units, and records node-local status.

`katlctl` is the operator UX and remote or multi-node orchestration layer. It
may connect to installed nodes, submit explicit operation requests, coordinate
bootstrap or rolling upgrade order, and report cluster-level progress.
`katlctl` may create bounded coordinator run records for operator-triggered
bootstrap, join, or rollout workflows. Those records are operation status for the
requested run, not desired cluster state and not instructions for a background
reconciler.

Systemd executes and supervises node-local operations. It owns unit ordering,
dependency management, restart handling, logging, health targets, and boot
success tracking.

Node-local operations run under systemd-supervised units, usually a templated
service such as:

```text
katl-operation@<operationID>.service
```

External commands should be launched through a `katlc` operation wrapper such as:

```text
katlc operation run-tool --operation-id <id> --phase <phase> -- <tool> <args>
```

The wrapper records systemd `INVOCATION_ID`, writes the pre-exec mutation marker
immediately before launching the tool, captures redacted output and exit status,
and updates the operation journal. Record locks are held only while writing
journal or snapshot state. Resource locks such as `generation-state.lock`,
`kubeadm-state.lock`, and install disk locks are held across the bounded
mutating phase they protect.

At boot, `katl-operation-reconcile.service` audits nonterminal operation
records after generation activation and before boot-complete or kubeadm-ready
targets:

```text
katl-operation-reconcile.service
  Type=oneshot
  ExecStart=/usr/bin/katlc operation reconcile --boot
  RequiresMountsFor=/var/lib/katl
  After=local-fs.target katl-generation-activate.service
  After=systemd-sysext.service systemd-confext.service
  Before=katl-kubeadm-ready.target katl-boot-complete.target
  Before=katl-operation@.service
```

This service is a boot-time audit and finalizer, not a daemon and not a cluster
reconciler. It may classify stale records, finish idempotent host bookkeeping,
and record diagnostics. It must not run kubeadm, kubectl, mutate etcd, join
nodes, continue coordinator rollout order, refresh expired join material, or
clean Kubernetes state.

Kubeadm remains authoritative for Kubernetes cluster mutation. It owns
bootstrap, join workflows, control-plane upgrades, node upgrades, kubelet
configuration updates, certificate behavior, and kubeadm-managed Kubernetes
objects.

Katl owns the boundary around those tools:

```text
host state
generation management
configuration rendering
operation planning
operation status and diagnostics
health verification
host rollback decisions
```

## Lifecycle Model

The installer creates generation 0:

```text
Install KatlOS
  -> create generation 0
  -> store user-supplied cluster intent from the install manifest
  -> boot generation 0
  -> reach installed-runtime health
```

Cluster bootstrap creates and commits the first Kubernetes-capable generation:

```text
katlctl cluster bootstrap
  -> ask katlc to validate stored cluster intent
  -> create candidate generation 1
  -> select the requested bundled Kubernetes sysext
  -> render kubeadm input and required host configuration
  -> project /etc/kubernetes from writable state
  -> activate generation 1 as a candidate
  -> verify containerd, kubelet wiring, kubeadm tools, and local readiness
  -> run kubeadm init or kubeadm join
  -> run post-kubeadm health checks
  -> commit generation 1 only after kubeadm and operation health checks succeed
```

The Kubernetes-capable generation is host state, but its first commit is gated by
the bootstrap or join operation because kubeadm mutates persistent node or
cluster state. That commit records the accepted desired host state. It does not
make the generation known-good until a later boot reaches
`katl-boot-complete.target`.

Cluster bootstrap and node join use the same operation model:

```text
BootstrapCluster
  -> create and activate the first Kubernetes-capable generation as a candidate
  -> run kubeadm init
  -> verify local control-plane health
  -> commit the candidate generation
  -> publish bootstrap artifacts and mark operation complete

JoinCluster
  -> create and activate the first Kubernetes-capable generation as a candidate
  -> run kubeadm join
  -> verify node-local join health
  -> commit the candidate generation and mark operation complete
```

Kubernetes upgrades use the same pattern after bootstrap:

```text
Generation N
  Kubernetes 1.36.0

Generation N+1
  Kubernetes 1.36.1

UpgradeControlPlane or UpgradeWorker
  -> run kubeadm upgrade apply or kubeadm upgrade node
  -> restart kubelet at the planned point
  -> verify local health
  -> update generation N+1 status and commit only through the operation record
```

During Kubernetes upgrades, target-version kubeadm availability is part of the
operation execution context. Target-version kubelet activation is a later
operation phase. This preserves generation semantics while matching kubeadm's
required ordering.

## Generation State Transitions

The common generation state transitions are:

```text
create candidate
  write immutable generation spec and initial status with commitState candidate,
  bootState pending, and healthState unknown

select for next boot
  arm the candidate with bounded boot selection; the current boot remains on the
  previous active generation

live activation
  activate a candidate in the current boot for an explicit operation; record
  live-active operation status, but do not treat it as boot health

boot activation
  boot with generation identity, recreate selected /run activation links, and
  enter bootState trying

health promotion
  after katl-boot-complete.target, set bootState good and healthState healthy

commit
  set commitState committed and make the generation the persistent desired host
  default; if another generation was the previous committed default, mark it
  superseded only after the new default is accepted

failed boot
  set the tried candidate failed/unhealthy, then select the previous known-good
  generation

live apply
  use config-apply-status.json for live phases; live activation does not by
  itself prove boot health or known-good eligibility
```

## Failure And Rollback

Generations provide host rollback. Operations provide transactional status and
repair context.

Before an operation mutates kubeadm-owned node state or Kubernetes API state, a
failed candidate generation can usually be abandoned and the node can return to
the previous known-good generation.

After an operation mutates kubeadm-owned node or cluster state, host rollback
must not claim to undo that mutation. For example, rolling back from a target
Kubernetes sysext to a previous host generation does not necessarily roll back
kubeadm changes already written to `/etc/kubernetes`, kubelet state, etcd, or
Kubernetes API objects.

Operation status must therefore record:

```text
previous generation id
candidate generation id, when one exists
operation phase
pre-exec mutation marker for each mutating tool invocation
whether kubeadm or another mutating tool has run
mutation scopes such as etc-kubernetes, kubelet-state, etcd-state, or
  cluster-objects
operation-kind-specific evidence for kubeadm, API, static pod, kubelet, and
  etcd state
diagnostic artifact paths
whether host rollback was attempted
whether kubeadm-aware repair or retry is required
```

This keeps host state declarative and rollback-aware while acknowledging that
some operations are inherently transactional.

## Recovery And Repair

Recovery and repair are explicit operations, not automatic failure handlers. A
terminal state such as `failed-needs-repair` means Katl cannot safely continue
without an explicit repair or retry decision. It does not authorize hidden
cleanup, kubeadm repair, etcd mutation, request replacement, or destructive
reinstall. The associated operation record must say whether mutation had already
started and which mutation scopes were touched.

Boot-time operation reconciliation classifies stale records:

```text
not-stale
  terminal record, or owned by a live systemd invocation from the current boot

stale-pre-mutation
  nonterminal record from an earlier boot where externalMutationStarted=false,
  mutatingToolRan=false, preExecMutationMarkers[] is empty, and mutationScopes[]
  is empty

stale-host-only
  interrupted Katl-owned host work such as generation status update, activation
  link recreation, host rollback bookkeeping, diagnostics, or commit finalization

stale-post-mutation
  externalMutationStarted=true, mutatingToolRan=true, mutationScopes[] is
  non-empty, a pre-exec mutation marker exists, or the phase was a
  kubeadm/kubectl/etcd-running phase

stale-ambiguous
  missing or corrupt phase ownership, unknown mutation boundary, or a mutating
  operation kind without enough durable state to prove pre-mutation
```

Automatic resume is allowed only for Katl-owned idempotent host phases that were
explicitly marked resumable. Examples include finishing host rollback
bookkeeping, rebuilding `record.json` from a valid journal, or completing a
generation commit whose preconditions were already durably recorded. Automatic
resume is refused for kubeadm init/join/upgrade, kubectl, etcd mutation,
coordinator workflow continuation, expired join material refresh, and all
stale-ambiguous records.

Stale-post-mutation and stale-ambiguous records must keep `recoveryRequired`
until an explicit retry or repair operation succeeds. A classified
`failed-needs-repair` operation can still count as successful boot-time
reconciliation; inability to classify, read, or write Katl state is a boot health
failure.

Recovery operation shapes are deferred until implemented. Unsupported recovery
requests must be refused with diagnostics rather than partially executed.
Recovery operation scope must be explicit:

```text
install-state
host-generation
kubeadm-state
etcd-state
destructive-reset
```

Each scope needs its own preflights, allowed mutations, forbidden mutations, and
validation gates. Deferred recovery operation types include
`RepairInstallState`, `RepairGenerationStatus`, `RetryOperation`,
`RenewCertificates`, `ResetNode`, `ReplaceEtcdMember`, and
`RestoreEtcdSnapshot`. Naming them documents boundaries only; it does not make
them supported behavior.

## Testing Contract

The operation model needs tests at the level where behavior becomes concrete:

```text
unit tests for operation planning and validation
golden tests for generated operation records and systemd units
systemd-analyze verify for generated units where practical
VM tests for install, bootstrap, join, upgrade, rollback, and repair workflows as
  they are implemented
```

Documentation-only changes to this model do not require VM gates. Any
implementation that changes boot, install, update, kubeadm, or operation
execution behavior needs the relevant VM gate or an explicit recorded host
capability gap.
