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

Generation 0 records user-supplied cluster intent from the install manifest, but
the KatlOS image does not bundle Kubernetes sysext artifacts. It does not fetch
or activate Kubernetes binaries, create `/etc/kubernetes`, run kubeadm, or create
cluster state. The first Kubernetes-capable generation is created by an explicit
bootstrap or join operation after `katlc` fetches and verifies the requested
payload bundle from a user-supplied HTTPS source.

Generation 0 is a hard clean-state invariant, not just a convenient label.

Required generation 0 invariants:

```text
KatlOS runtime root, boot metadata, writable state layout, machine identity, and
  stored cluster intent exist
selected Kubernetes sysext set is empty
kubelet is disabled or absent from the active boot transaction
containerd may run only as baseline host runtime plumbing, not as proof of
  Kubernetes membership
no Kubernetes PKI exists under the projected /etc/kubernetes backing store
no kubeadm static pod manifests exist
no kubeadm kubeconfigs exist
no etcd data exists for a Katl-managed stacked-etcd member
no kubelet join/bootstrap state exists under /var/lib/kubelet
no kubeadm init, join, or upgrade operation has crossed its mutation boundary
```

The `/etc/kubernetes` backing directory may exist as an empty projected state
location, but kubeadm-owned contents must not. A node may safely return to
generation 0 only while these cluster-state invariants remain true. Once kubeadm
has created PKI, kubelet state, static pod manifests, etcd data, or API objects,
host rollback to generation 0 is not a clean cluster reset; cleanup requires an
explicit reset, repair, recovery, or destructive wipe/reinstall path.

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
  a generation with commitState committed; its desired host state has been
  accepted by the responsible install, apply, bootstrap, join, or upgrade path,
  but it may still have bootState pending and may not be the persistent boot
  default

boot-selected generation
  the generation named by boot selection state for a specific purpose:
  defaultGenerationID for persistent default boot, trialGenerationID for bounded
  trial boot, or bootedGenerationID for the current boot

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
bootstrap-init
bootstrap-join-worker
bootstrap-join-control-plane (future additional control-plane join path)
config-apply
host-upgrade
kubeadm-upgrade
kubeadm-reset
recover-control-plane
renew-certificates
replace-etcd-member
```

An operation answers:

```text
What action must occur to reach the desired state?
```

Operations are explicit. Normal configuration apply and generation activation
must not silently run kubeadm, kubectl, CNI installers, GitOps controllers,
package managers, or cluster repair commands.

Normal `katlc apply` is the generation apply path and records a `config-apply`
operation for accepted changes. Its default request mode is `auto`: the planner
prefers online in-place apply when every changed domain has a proven live plan,
falls back to next-boot for safe boot-coupled changes, and rejects
operation-only or unsafe input before rendering partial state. Creating or
activating the first Kubernetes-capable generation during cluster bootstrap is
still generation management, but it is part of an explicit bootstrap or join
operation because kubeadm will mutate node or cluster state before the generation
can be committed. Named operations are reserved for transactional workflows that
run mutating tools such as kubeadm, coordinate external state, change root/UKI
bytes, or repair state outside normal generation apply.

`host-upgrade` is the durable operation kind for KatlOS runtime root and UKI
updates from one verified KatlOS upgrade image. It always stages a next-boot
candidate generation. It is not an online in-place config apply even when the
request is submitted through the same node-local `katlc` API.

`bootstrap-init` and `bootstrap-join-worker` are the day-one durable operation
kinds initiated by `katlctl cluster bootstrap`, but the accepted operation
attempts are created by node-local `katlc`. `bootstrap-join-control-plane` is a
future additional-control-plane operation kind and is unsupported until its
certificate-key handling, etcd membership evidence, rollback limits, and VM
proof exist. For bounded multi-node workflows, `katlctl` may display an
invocation summary that links returned node-local operation IDs, candidate
generation IDs, phase state, redacted diagnostics, and mutation boundaries. That
summary is not an `OperationRecord`, not persistent Katl state, and not used for
node crash recovery.

`kubeadm-upgrade` names the durable operation kind for Kubernetes upgrades.
Role-specific views such as control-plane apply, control-plane node upgrade, and
worker upgrade are operation fields, phases, or CLI presentation, not competing
state models. Naming the boundary does not make Kubernetes upgrade execution
supported before the target kubeadm access mode and target kubelet activation
gate are selected, implemented, and tested.

## Operation Records

An `OperationRecord` is the canonical durable operation record for an accepted
node-local Katl lifecycle workflow. Canonical `OperationRecord`s are owned by
`katlc` and live under:

```text
/var/lib/katl/operations/<operation-id>/
```

The authoritative recovery source is the operation journal under that directory.
`record.json` is a rebuildable snapshot. Terms such as checkpoint, config-apply
status, bootstrap summary, and upgrade status are summaries or views unless they
name that storage root.

One record tracks one explicit node-local attempt from request acceptance to
terminal result. Multi-node workflows link returned node-local operation IDs.
Any `katlctl` invocation summary is non-authoritative and must not be used for
node crash recovery.

Common fields:

```text
apiVersion
kind: OperationRecord
schemaVersion
operationID
operationKind
scope: install-state | host-generation | kubeadm-state | etcd-state |
  destructive-reset
parentOperationID, when present
clientRequestID, when present
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
  agentStartID
  executorAttemptID
  childProcess, when present
  pid, when present
  startedAt
  completedAt
  exitStatus, when present
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

This shared model normalizes lifecycle vocabulary. Workflow-specific files such
as install checkpoints, config apply status, bootstrap summaries, and upgrade
summaries may remain as compatibility read models, but once a workflow is an
accepted operation, authoritative state and recovery data must be in the
`katlc`-owned `OperationRecord`.

## Command And System Boundaries

`katlc` is the node-local authority. It validates node-local input, compiles or
selects candidate generations, plans operation records, executes accepted
operations through its long-running agent, configures KatlOS state, and records
node-local status.

`katlctl` is a control client. It may keep local client configuration for
connection profiles and known node details, read inventory or compiled plans,
connect from an operator workstation to installed nodes, submit explicit
requests to node-local `katlc`, wait on returned operation IDs, stream status,
sequence multi-node requests, and relay explicit client-side outputs such as
kubeconfig data when requested. Its own persistent state is limited to
communication and known-node config. It must not generate, create, own, or
persist generation specs, generation status, `OperationRecord`s, retry state, or
authoritative node lifecycle state. Any aggregate status it displays is a client
view, not desired cluster state and not instructions for a background
reconciler.

## Day-One `katlc` Agent API

The agent architecture, command/query split, executor, storage backend, and
non-reconciler boundary are defined in
`docs/internal/katlc-agent-architecture.md`.

The day-one node agent is a systemd-managed long-running `katlc` process on each
KatlOS host. It accepts explicit lifecycle operation requests from `katlctl`
running on an operator workstation. The selected transport is gRPC over TCP on a
configured management endpoint. `katlctl` must connect to that endpoint directly
from the workstation; it must not SSH to nodes, run remote shell commands, or
depend on a host-local socket forwarding path.

```text
katlc-agent.service
  ExecStart=/usr/bin/katlc agent serve --listen tcp://<management-address>
  RequiresMountsFor=/var/lib/katl
  After=local-fs.target katl-generation-activate.service
```

Optional on-host diagnostics must not create a second supported operation
submission path. If `katlc` grows local debug commands, they are for inspecting
or repairing the local agent under operator break-glass access, not for normal
bootstrap, apply, upgrade, or rollback workflows.

`katl-dty.11.25.7.2` owns the exact unit files. This contract fixes the API and
observable endpoint shape those units must expose.

Endpoint discovery is client-driven for `katlctl` and node-local only for
operator inspection:

```text
katlctl inventory or client config -> node address and TCP management endpoint
/run/katl/katlc/endpoint.json -> local read-only view of the running agent
```

`endpoint.json` is ephemeral discovery data owned by the running agent. It must
contain the API version, node machine ID, agent start time, supported operation
kinds, and the locally observed listener details. It may be world-readable if it
contains no secret material. It is not durable lifecycle state. Existing VM
harness RPCs such as `RunCommand` and `ReadFile` are pre-agent test plumbing;
API-level VM tests should exercise the same TCP gRPC path an operator
workstation uses.

The minimum gRPC surface is:

```text
KatlcAgent.GetNodeStatus(GetNodeStatusRequest) returns (NodeStatus)
KatlcAgent.SubmitOperation(SubmitOperationRequest) returns (OperationAccepted)
KatlcAgent.GetOperation(GetOperationRequest) returns (OperationStatus)
KatlcAgent.WatchOperation(WatchOperationRequest) returns (stream OperationEvent)
```

`GetNodeStatus` reports generation 0 readiness, current boot selection,
machine identity, supported API versions, supported operation kinds, and whether
conflicting operation locks are held. It must not synthesize authoritative state
from `katlctl` summaries.

`SubmitOperationRequest` has the common envelope:

```text
apiVersion: katl.dev/v1alpha1
kind: SubmitOperationRequest
clientRequestID
operationKind: bootstrap-init | bootstrap-join-worker
actor
expectedMachineID
expectedCurrentGenerationID
expectedClusterIntentDigest
requestDigest
dryRun
operationTimeout
request:
  kind-specific request body
```

The bootstrap request body contains only the data needed for node-local `katlc`
to validate stored intent and render or select the first Kubernetes-capable
candidate generation:

```text
inventoryNodeName
systemRole
kubernetesPayloadVersion
bootstrapProfileRef
controlPlaneEndpoint, when required
stableEndpoint, when requested
candidateGenerationID, optional client hint
kubeadmInputDigest or compiled profile digest, when supplied by a plan
joinMaterialRef or secret envelope for join operations
```

`katlc` must revalidate the request against `/var/lib/katl/cluster/intent.json`,
the selected generation records, available KatlOS image artifacts, and live
machine identity before accepting it. Client-supplied fields are constraints and
provenance, not authority to rewrite node-local state.

`requestDigest` is the canonical SHA-256 digest of the normalized, schema-valid
operation request envelope and kind-specific non-secret fields with
`requestDigest` omitted from the canonical input. The client may provide it as
an idempotency check, but `katlc` must compute the digest itself and reject a
supplied mismatch before accepting the request. Secret-bearing fields are
represented in the digest by stable redacted descriptors and separate secret
material digests. A repeated `clientRequestID` with the same
`requestDigest` is idempotent and returns the existing operation status. A
repeated `clientRequestID` with a different digest is rejected. A request with
the same operation intent but no matching idempotency key may create a new
operation only if the relevant locks and live-state preflights allow it.

`dryRun: true` validates the request against stored intent, live machine
identity, available artifacts, and lock availability, then returns a redacted
plan summary without creating an `OperationRecord`, generation, material files,
or operation executor task. It is a client-visible validation result, not
authoritative lifecycle state. The existing `katlctl cluster bootstrap
--dry-run` path may use this API or perform equivalent preflight checks, but a
real operation is accepted only with `dryRun: false`.

On acceptance, `katlc` writes and fsyncs the canonical `OperationRecord` under
`/var/lib/katl/operations/<operation-id>/` before creating or activating a
candidate generation, materializing kubeadm input, or launching kubeadm. The
response contains:

```text
operationID
operationKind
requestDigest
recordPath
acceptedAt
initialStatus
```

`OperationStatus` is a redacted read model built from the node-local operation
record and current live probes:

```text
operationID
operationKind
requestDigest
phase
phaseIndex
completedPhases[]
terminal
result
candidateGenerationID
activationState
generationCommitState
postKubeadmHealthState
bootHealthPending
externalMutationStarted
mutationScopes[]
resourceLocks[]
latestJournalSeq
updatedAt
nextAction
diagnostics[], redacted
```

`GetOperationRequest` identifies one accepted operation:

```text
operationID
expectedRequestDigest, optional
includeDiagnostics: normal | verbose
```

`WatchOperationRequest` identifies one accepted operation and stream start:

```text
operationID
expectedRequestDigest, optional
afterJournalSeq
watchTimeout
```

`OperationEvent` contains:

```text
operationID
journalSeq
eventType
phase
terminal
status: OperationStatus
diagnostics[], redacted
```

`WatchOperation` streams operation events starting after `afterJournalSeq`.
Watches are convenience streams only. They may disconnect, time out, or be
unavailable without changing operation state. `katlctl` must fall back to
`GetOperation` polling and must treat the node-local operation record as
authoritative.
Any aggregate `katlctl` summary built from status responses is disposable client
output; crash recovery and retry decisions use `/var/lib/katl/operations` on the
node.

Timeouts are explicit:

```text
operationTimeout
  upper bound requested for the node-local operation attempt; `katlc` may reject
  unsupported or unsafe values and records the accepted timeout in the operation
  record. When it expires, `katlc` records a timeout event, stops or abandons
  the internal operation executor task, re-probes mutation markers and live
  state, writes a terminal result of timed-out or failed-needs-repair based on
  whether mutation may have started, and releases only locks that are safe to
  release after that classification

watchTimeout
  client-side stream lifetime; expiry does not cancel the operation

pollTimeout
  client-side wait budget; expiry does not cancel the operation
```

Day-one cancellation is limited to client waiting. Once `SubmitOperation`
returns accepted, stopping `katlctl`, dropping the TCP connection, or timing out
a watch does not cancel the node-local operation. A future explicit cancel or
repair API must create its own operation record and must not erase the original
attempt.

Concurrency is lock-based. Mutating operations declare required resource locks
before acceptance:

```text
generation-state.lock
kubeadm-state.lock
boot-selection.lock, when boot state is changed
install-state.lock, only for installer or install repair operations
```

`GetNodeStatus`, `GetOperation`, and `WatchOperation` may run concurrently.
`SubmitOperation` rejects conflicting mutating requests while an operation holds
or has reserved a required lock. For day one, at most one bootstrap or join
operation may be active on a node. Multi-node sequencing belongs to `katlctl`,
but every node-local acceptance decision is made independently by that node's
`katlc`.

Diagnostic redaction is part of the API contract. Normal API responses, watch
events, operation summaries, and diagnostic artifacts exposed through the agent
must redact:

```text
private keys
kubeconfigs and client certificates
kubeadm bootstrap tokens
certificate keys and upload-certs material
bearer tokens and Authorization headers
URLs with embedded credentials
full secret-bearing node config
raw kubeadm stdout/stderr when it may contain secrets
```

Redacted diagnostics may include command names, exit status, phase names,
redacted URLs, certificate fingerprints, token fingerprints, digests, and paths
to root-only redacted artifacts. Secret material required to run kubeadm may be
accepted over the agent API and stored only in root-owned operation material
files under `/var/lib/katl/operations/<operation-id>/material/` with mode
`0600`; it must not be echoed in normal status.

Day-one security is intentionally small but must still match the remote-client
shape:

```text
no unauthenticated management listener
katlctl runs off-node and connects to the katlc management endpoint advertised
  by inventory or client configuration
katlctl does not SSH to nodes or execute remote shell commands
home-ops authentication may be minimal, for example an operator-managed key,
  token, certificate, or trusted transport, but it must be explicit
any on-host debug command is secondary to the TCP gRPC contract and must not be
  required for normal operation submission or status
no multi-tenant RBAC beyond local OS user/group permissions
no Kubernetes API, kubeconfig, or cluster identity required before bootstrap
node-local katlc revalidates machine ID, stored intent, request digest, and
  operation locks before accepting a mutating request
```

This is sufficient for home-ops day one and VM validation. Stronger remote
authentication, certificate enrollment, audit export, and policy authorization
can be added later without changing the boundary that authoritative lifecycle
state lives on the node under `/var/lib/katl`.

Machine identity follows the same ownership split. `katlos-install` creates the
initial machine ID, stores it under `/var/lib/katl/identity/machine-id`, and
renders it into generation 0 boot metadata with `systemd.machine_id=<id>`.
Future generation planning by `katlc` preserves and validates that value.
`katlctl` must not write machine-id files, loader entries, kernel arguments, or
`/run` activation state.

Systemd supervises `katlc-agent.service`, boot health targets, and ordinary
KatlOS services. It is not the operation submission or dispatch API, and Katl
must not model normal operations as systemd re-execution of `katlc` CLI
subcommands. The target runtime must remove the systemd-run `katlc operation`
execution path, including templated operation units and any public operation
execution CLI entrypoints used only for that re-exec model.

Accepted operations run inside the long-running `katlc` agent under an internal
operation executor. The executor may invoke kubeadm, systemd tools, or other
bounded external commands as child processes when the operation contract
requires them. It must write the pre-exec mutation marker immediately before
launching a mutating tool, capture redacted output and exit status, and update
the operation journal. Record locks are held only while writing journal or
snapshot state. Resource locks such as `generation-state.lock`,
`kubeadm-state.lock`, and install disk locks are held across the bounded
mutating phase they protect.

Boot-time bookkeeping belongs to the agent startup path and explicit boot-health
logic, not to a separate user-visible operation entrypoint. The agent may
classify stale records, finish idempotent host bookkeeping, and record
diagnostics. It must not run kubeadm, kubectl, mutate etcd, join nodes, continue
multi-node rollout order, refresh expired join material, or clean Kubernetes
state as an automatic reconcile loop.

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

Runtime configuration apply creates a new generation for every accepted change:

```text
katlc apply
  -> authenticate and audit the NodeConfigurationChange request
  -> merge cluster defaults, systemRole overrides, and node overrides
  -> classify domain diffs for auto, live, next-boot, operation-only, or reject
  -> write a config-apply OperationRecord before mutation
  -> render generated confext and write candidate generation spec/status

accepted live
  -> activate the candidate confext in the current boot
  -> run only bounded domain live actions
  -> commit the generation after live checks pass
  -> leave boot health and persistent default promotion pending until a later boot

accepted next-boot
  -> arm bounded trial boot selection for the candidate
  -> leave the current boot unchanged

operation-only or rejected
  -> record redacted diagnostics
  -> write no partial generation artifacts
```

Host KatlOS upgrades use the same generation records but a separate operation:

```text
host-upgrade
  -> accept exactly one verified KatlOS upgrade image reference
  -> verify image and component digests, architecture, and runtime interface
  -> stage runtime root and UKI through the sysupdate-backed transfer path
  -> render or preserve generated confext according to the accepted request
  -> write candidate generation spec/status
  -> arm bounded trial boot selection
  -> commit/promotion waits for boot health after the candidate boot
```

Cluster bootstrap creates and commits the first Kubernetes-capable generation:

```text
katlctl cluster bootstrap
  -> ask katlc to validate stored cluster intent
  -> katlc creates candidate generation 1
  -> katlc fetches and stages the requested Kubernetes payload bundle
  -> katlc selects the staged Kubernetes sysext
  -> katlc renders kubeadm input and required host configuration
  -> katlc projects /etc/kubernetes from writable state
  -> katlc activates generation 1 as a candidate
  -> katlc verifies containerd, kubelet wiring, kubeadm tools, and local readiness
  -> katlc runs kubeadm init or kubeadm join through the agent executor
  -> katlc runs post-kubeadm health checks
  -> katlc commits generation 1 only after kubeadm and operation health checks succeed
```

The Kubernetes-capable generation is host state, but its first commit is gated by
the bootstrap or join operation because kubeadm mutates persistent node or
cluster state. That commit records the accepted desired host state. It does not
move the persistent boot default or make the generation known-good. Known-good
promotion requires a later boot to reach `katl-boot-complete.target`.

Cluster bootstrap and node join use the same operation model:

```text
bootstrap-init
  -> katlc creates and activates the first Kubernetes-capable generation as a candidate
  -> katlc runs kubeadm init
  -> katlc records cluster-global bootstrap-state evidence
  -> katlc verifies local control-plane health
  -> katlc commits the candidate generation
  -> katlc records bootstrap artifacts and marks operation complete

bootstrap-join-worker
  -> katlc creates and activates the first Kubernetes-capable generation as a candidate
  -> katlc runs kubeadm join
  -> katlc records node-local join evidence
  -> katlc verifies node-local join health
  -> katlc commits the candidate generation and marks operation complete

bootstrap-join-control-plane
  -> future operation kind for additional control-plane joins
  -> unsupported until certificate-key handling, etcd membership evidence,
     rollback limits, and VM proof are designed and implemented
```

Kubernetes upgrades will use the same operation pattern after the upgrade
execution gate is closed. Until an ADR selects the target kubeadm access mode
and target kubelet activation gate, upgrade requests stop at rejected or
plan-only `katlc` status:

```text
Generation N
  Kubernetes 1.36.2

Generation N+1
  Kubernetes 1.36.3

kubeadm-upgrade
  -> katlc validates and records a plan-only or refused operation
  -> no bootable candidate is selected
  -> no target Kubernetes sysext is globally activated
  -> no kubeadm upgrade or kubelet restart runs
```

Post-ADR Kubernetes upgrade execution must keep target-version kubeadm
availability in the operation execution context and target-version kubelet
activation as an explicit operation phase. This preserves generation semantics
while matching kubeadm's required ordering.

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
  set commitState committed after the accepting path's preconditions pass; this
  accepts the generation as desired host state but does not change the persistent
  boot default or prove boot health

boot selection update
  update /var/lib/katl/boot/selection.json and systemd-boot state; unproven
  generations use trialGenerationID, one-shot selection, or boot counting, while
  defaultGenerationID moves only after known-good promotion

known-good promotion
  after katl-boot-complete.target, set bootState good and healthState healthy;
  the boot-selection path may then make this generation the persistent default
  and mark the previous committed generation superseded

failed boot
  set the tried candidate failed/unhealthy, then select the previous known-good
  generation

live apply
  record live phases in the katlc-owned OperationRecord; an optional
  generation-local config-apply-status.json may mirror the latest summary, but
  it is not authoritative. Live activation does not by itself prove boot health
  or known-good eligibility. A successful live apply may commit the generation
  as accepted desired host state, but persistent default promotion still waits
  for a later boot to become good and healthy
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

Katlc agent startup audit classifies stale records:

```text
not-stale
  terminal record, or owned by a live agent executor attempt from the current
  agent start

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
multi-node workflow continuation, expired join material refresh, and all
stale-ambiguous records.

Stale-post-mutation and stale-ambiguous records must keep `recoveryRequired`
until an explicit retry or repair operation succeeds. A classified
`failed-needs-repair` operation can still count as successful boot-time
reconciliation; inability to classify, read, or write Katl state is a boot health
failure.

Recovery operation shapes are deferred until implemented. Unsupported recovery
requests must be refused with diagnostics rather than partially executed.
`katlctl` may submit an explicit recovery request to node-local `katlc`, but
`katlc` is the only authority that validates, plans, records, and executes node
recovery.

Cluster-global recovery and rebuild semantics, including single-control-plane
loss, majority control-plane loss, CA loss, stale etcd data, and the rule that
general cluster rebuild means destructive wipe/reinstall plus new bootstrap, are
defined in `docs/internal/cluster-recovery-and-rebuild.md`.

### Recovery Operation Contract Requirements

Before a recovery kind can be implemented, its contract must define:

```text
request envelope
  operationKind, targetOperationID, scope, actor, explicitIntent, expected node
  identity, expected request/generation/member/snapshot digests as applicable,
  authorized mutation scopes, and dry-run or plan-only mode

common preflights
  target record exists; journal is valid or reconstructable; stale
  classification is known; resource locks are available; node identity matches;
  requested scope matches the failed operation; live state has been re-probed;
  requested mutations are authorized by the operation-specific contract

common forbidden behavior
  no boot-time automatic recovery; no hidden kubeadm, kubectl, or etcd
  mutation; no multi-node workflow continuation; no request replacement; no
  target disk reinterpretation; no deletion of /etc/kubernetes,
  /var/lib/kubelet, or /var/lib/etcd unless the specific destructive contract
  allows it

common evidence
  parent record digest, stale classification inputs, live probe results,
  redacted tool output, pre/post state digests, mutation markers,
  skipped/rerun phase decisions, and refusal reason

common refusal states
  unsupported-recovery-kind, missing-target-record, record-digest-mismatch,
  node-identity-mismatch, stale-ambiguous, scope-not-authorized,
  live-state-conflict, insufficient-evidence, data-loss-not-acknowledged,
  quorum-risk, version-incompatible, and concurrent-operation
```

Recovery operation scope must be explicit:

```text
install-state
host-generation
kubeadm-state
etcd-state
destructive-reset
```

### Deferred Recovery Operation Matrix

Naming these operation types documents boundaries only; it does not make them
supported behavior. Each row must be completed with tests before implementation:

| Operation kind | Required contract before support |
| --- | --- |
| `repair-install-state` | Scope `install-state`; preflight same request digest, target disk stable ID, GPT/filesystem/root-slot/generation 0/boot-entry probes; allow only same-request checkpoint/status/boot metadata repair; forbid request replacement, target disk switch, Kubernetes or etcd deletion, and destructive reinstall |
| `repair-generation-status` | Scope `host-generation`; preflight generation spec/status/journal/boot-entry consistency; allow commit/rollback bookkeeping repair only; forbid partial root, sysext, or confext switching |
| `retry-operation` | Scope matches the original operation; preflight parent record, stale class, request digest, live probes, and material validity; allow rerun only of idempotent or kind-declared retryable phases as a child operation; forbid automatic retry, stale-ambiguous replay, request changes, and implicit cleanup |
| `renew-certificates` | Scope `kubeadm-state`; preflight kubeadm version, certificate expiry, API/static pod state, and redacted kubeconfig access; allow explicit kubeadm certificate renewal and declared restarts; forbid upgrades, etcd membership changes, and config drift repair |
| `kubeadm-reset` | Scope `destructive-reset`; preflight explicit data-loss acknowledgement, node identity, system role, cluster membership, and etcd handling decision; allow only the declared reset surface; forbid etcd member replacement, snapshot restore, install input replacement, and undeclared disk wipe |
| `replace-etcd-member` | Scope `etcd-state`; preflight quorum, member identity, peer URLs, certificate compatibility, version skew, and local member mapping; allow only a named member remove/add flow; forbid snapshot restore, stale data reuse without proof, and automatic failed-join cleanup |
| `restore-etcd-snapshot` | Scope `etcd-state`; preflight explicit disaster intent, snapshot path/digest/revision, Kubernetes/etcd version compatibility, topology, participant set, and current-state backup decision; allow only the declared snapshot restore procedure; forbid in-place merge, unknown snapshots, partial topology restore, and treating host rollback as etcd rollback |

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
