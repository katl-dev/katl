# Node Install-To-Bootstrap State Machine

Status: current decision.

This document defines how a node moves from installer boot to the installed
runtime handoff, then waits for the explicit cluster bootstrap operation that
creates the first Kubernetes-capable generation and runs kubeadm. It covers both
network/PXE-style install with input supplied up front and USB/local-handoff
install where an operator supplies input after the installer boots.

Both paths converge on one `ValidatedInstallRequest` before any destructive disk
mutation. The request references one KatlOS install image payload plus
node-specific configuration. It does not reference loose runtime root, UKI, or
sysext artifacts as user-facing inputs.

## Inputs

Installer input may arrive from:

```text
preseeded install manifest URL/ref from kernel arguments
preseeded split node-config and KatlOS image refs from kernel arguments
preseed files copied into /run/katl or /etc/katl
local handoff POST /v1/install when no preseeded input is present
offline media references in a later ISO/USB wrapper
```

All delivery paths normalize to:

```text
ValidatedInstallRequest
  inputMode: pxe-preseed | local-handoff | offline-media | test
  requestDigest
  node identity and node configuration
  install policy, including destructive install guard and target disk selector
  systemRole
  exact Kubernetes payload version, such as 1.36.1
  bootstrap profile refs, not kubeadm actions
  one katlosImage reference with digest and expected metadata
```

`systemRole`, the requested Kubernetes version, and bootstrap profile refs are
installed as cluster intent for later bootstrap. They do not activate Kubernetes
binaries and do not cause `kubeadm init` or `kubeadm join` during node install.

## Installer States

The installer-side state machine:

```text
BootInstaller
  installer kernel/initrd or UKI starts katlos-install

DiscoverInput
  read safe kernel arguments, preseed files, and local media hints

FetchInput
  fetch referenced input documents or local media files that were named by safe
  boot arguments

WaitForLocalConfig
  only when no preseeded input exists; start local handoff API and wait

ValidateInput
  decode and validate installer input, normalize split refs, reject unknown
  fields, and create a ValidatedInstallRequest

VerifyKatlOSImage
  fetch or locate the KatlOS image, verify top-level digest, mount read-only,
  verify embedded index and component digests, and validate architecture and
  runtime compatibility; do not resolve, bundle, or activate a Kubernetes sysext
  for generation 0

PlanInstall
  collect hardware facts, resolve target disk selectors, build a typed install
  plan, and refuse unsafe or ambiguous disk state

CheckExistingInstallState
  inspect any existing Katl GPT, state partition, generation spec/status, and boot
  entries before deciding whether to continue, repair, or refuse

PrepareTarget
  wipe explicitly authorized target devices, create partitions, format writable
  filesystems, and mount the target

InstallGeneration0
  write the runtime root slot, install boot assets, cache verified image
  components, materialize baseline generated confext, write mount units, write
  seed data, persist cluster intent, and create generation 0 metadata

WriteInstallRecord
  persist the request, selected image metadata, hardware facts, plan, generation
  record digest, and final checkpoint under target state

SetBootCandidate
  make the verified generation bootable only after target verification succeeds

RebootToRuntime
  request reboot into generation 0
```

`WaitForLocalConfig` is a non-mutating waiting state, not a failure. It may run
indefinitely until an operator supplies one valid request or stops the installer.

## Runtime States

After reboot, the installed runtime continues the handoff from generation 0:

```text
RuntimeFirstBoot
  boot selected generation, mount writable state, and load install record

ActivateGeneration
  activate selected sysexts and generated confext for the generation

InstalledRuntimeReady
  reach local runtime health with writable state, operator access, katlc, and
  katlc agent wiring available; this generation 0 handoff does not activate
  Kubernetes binaries, require /etc/kubernetes projection, containerd running,
  kubelet availability, or katl-kubeadm-ready.target

WaitingForClusterBootstrap
  terminal install handoff state for the node; operator-run katlctl cluster
  bootstrap may now submit an explicit request to node-local katlc, which
  validates stored intent, creates the first Kubernetes-capable candidate
  generation, and runs the appropriate kubeadm workflow
```

Generation 0 is valid only while its clean-state invariant holds: no active
Kubernetes sysext, no enabled kubelet, no kubeadm-owned PKI or kubeconfigs, no
static pod manifests, no kubelet join state, no local etcd data, and no prior
kubeadm mutation boundary for the node. Empty backing directories for future
state projections are allowed; kubeadm-owned contents are not. If any of that
state exists, the node is no longer a clean generation 0 bootstrap target and
must go through explicit reset, recovery, or destructive wipe/reinstall.

## Generation 0 Runtime Contract

Generation 0 is the installed KatlOS baseline that VM handoff tests and later
bootstrap operations can share as a checklist. It is successful only after the
installed runtime reaches `waiting-for-cluster-bootstrap`.

This is the target contract for the operation-backed generation lifecycle. Older
scaffolding that uses `katl-kubeadm-ready.target` as the first-boot handoff,
writes only generation `metadata.json`, or records a Kubernetes sysext as
generation 0 selected state is pre-contract behavior. The generation 0 refit
must move that behavior to the split generation records and explicit bootstrap
operation boundary below. VM tests written for the refit should treat legacy-only
state as a failure unless they are explicitly testing transition compatibility.

Required systemd state:

```text
local-fs.target reached with the installed writable state partition mounted
katl-generation-activate.service completed for generation 0
systemd-sysext.service and systemd-confext.service completed or reported no
  selected extension work for generation 0
katlc-agent.service completed startup audit of operation state
katl-boot-complete.target reached for the installed-runtime profile
katl-kubeadm-ready.target not reached and not required
kubelet.service disabled, inactive, or absent
kubeadm init/join/upgrade automation units inactive or absent
```

The generation 0 activation set is KatlOS-only. It may activate generated
confext for host configuration and Katl services, but it must not activate a
Kubernetes sysext or make kubeadm/kubelet readiness part of installed-runtime
health.

Required state under `/var/lib/katl`:

```text
/var/lib/katl/install/status.json
/var/lib/katl/install/state.json
/var/lib/katl/install/input-source.json
/var/lib/katl/install/request.json
/var/lib/katl/install/manifest.json
/var/lib/katl/install/katlos-image.json
/var/lib/katl/install/hardware-facts.json
/var/lib/katl/install/plan.json
/var/lib/katl/install/logs/
/var/lib/katl/generations/0/spec.json
/var/lib/katl/generations/0/status.json
/var/lib/katl/boot/selection.json
/var/lib/katl/identity/machine-id
/var/lib/katl/cluster/intent.json
/var/lib/katl/operations/
```

The install files are operator-facing summaries and attachments for the install
operation. Generation and boot records are the host-state source of truth.
Operation recovery state is authoritative only under
`/var/lib/katl/operations/<operation-id>/`.

Generation 0 `spec.json` must select:

```text
generationID: 0
runtime root slot and root PARTUUID written by install
installed UKI or loader entry for the selected root
kernel command line containing the installed machine identity
generated confext needed for host configuration, operator access, and Katl
  service wiring
empty selected Kubernetes sysext list
no /etc/kubernetes projection requirement
```

Generation 0 `status.json` starts as:

```text
commitState: committed
bootState: pending
healthState: unknown
```

After installed-runtime health succeeds, generation 0 status becomes:

```text
commitState: committed
bootState: good
healthState: healthy
```

`/var/lib/katl/boot/selection.json` must name generation 0 as the current boot
and persistent default, with no trial generation and no pending Kubernetes
health validation. Later bootstrap may create and activate a candidate
generation, but generation 0 handoff itself has no Kubernetes-capable candidate.

Machine identity requirements:

```text
/var/lib/katl/identity/machine-id exists, is non-empty, and matches the
  machine identity used by the running system
the generation 0 kernel command line or equivalent boot metadata preserves that
  identity for repeat boots
host static hostname and inventory node name are stored as install/cluster
  intent, not inferred from Kubernetes Node objects
```

Stored cluster intent requirements:

```text
systemRole from the accepted install request
requested Kubernetes payload version from the accepted manifest
bootstrap profile refs or resolved profile identifiers
inventory node identity and operator-facing node labels/taints needed to render
  later kubeadm input
endpoint intent or resolved control-plane endpoint fields needed for later
  kubeadm and kubeconfig rendering
requestDigest and source KatlOS image digest tying the intent to the install
```

Cluster intent is desired input for a later explicit operation. It is not proof
of Kubernetes membership and must not include kubeadm-generated PKI,
kubeconfigs, static pod manifests, bootstrap tokens, or live API state.

Required day-one `katlc` operation wiring:

```text
katlc is installed in the runtime
a katlc TCP gRPC management endpoint is reachable from the operator workstation
  as defined by inventory or client configuration
local katlc endpoint details are inspectable on the node for debugging only
katlctl can submit an explicit bootstrap-init or bootstrap-join-worker request
  to that endpoint
accepted operations create node-local OperationRecords under
  /var/lib/katl/operations/<operation-id>/
katlc-agent.service can classify nonterminal operation records at startup
katlc executes accepted operations through its internal executor, not through
  systemd re-execution of katlc CLI subcommands
```

Until the day-one agent API contract is implemented, generation 0 VM tests may
assert only the installed `katlc` binary and baseline agent service wiring.
Endpoint listener configuration, health RPC, and remote `katlctl` discovery
belong to that API contract.

Operator access before Kubernetes:

```text
console or configured SSH/local access reaches the installed node
operator can inspect systemd status, journal output, and /var/lib/katl summaries
operator uses katlctl from the workstation against the node management endpoint;
  local katlc commands, if present, are break-glass debug tools only
no Kubernetes API, kubeconfig, CNI, or GitOps controller is required for access
```

Forbidden generation 0 Kubernetes state:

```text
selected Kubernetes sysext in generation 0 spec or active extension links
enabled or active kubelet membership
/var/lib/katl/kubernetes/etc-kubernetes/pki/*
/var/lib/katl/kubernetes/etc-kubernetes/*.conf
/var/lib/katl/kubernetes/etc-kubernetes/manifests/*
/etc/kubernetes as a non-empty root/confext-owned directory
/var/lib/kubelet/bootstrap-kubeconfig
/var/lib/kubelet/kubeconfig
/var/lib/kubelet/config.yaml
/var/lib/kubelet/pki/*
/var/lib/etcd/* or Katl-managed stacked-etcd partition contents
OperationRecord evidence that kubeadm init, join, or upgrade crossed or may have
  crossed a mutation boundary for this node, including pre-exec mutation markers,
  externalMutationStarted, mutatingToolRan, or non-empty mutationScopes[]
```

Empty reserved directories and mountpoint placeholders are allowed only when
they are safe for later projection validation. VM tests for generation 0 should
check both positive handoff facts and the forbidden-state list. A node that
fails either side of the checklist is not a clean generation 0 bootstrap target.

Cluster bootstrap creates the first Kubernetes-capable generation and then runs
kubeadm:

```text
katlctl cluster bootstrap
  submit a bootstrap-init or bootstrap-join-worker request to node-local katlc
  katlc validates stored cluster intent
  katlc fetches the user-supplied HTTPS Kubernetes payload bundle whose payload
  version exactly matches the stored install intent
  katlc verifies the bundle manifest and stages the sysext under Katl-owned
  storage
  katlc resolves bootstrap profiles and renders kubeadm input under /etc/katl
  katlc projects /etc/kubernetes from writable state
  katlc ensures containerd prerequisites, kubelet service wiring, and systemRole
  metadata
  katlc creates and activates generation 1 as a candidate

KubeadmReady
  reach katl-kubeadm-ready.target after local prerequisites are active

KubeadmOperation
  katlc records a bootstrap-init attempt for the init node or a role-specific
  bootstrap-join attempt for joining nodes based on cluster inventory and
  systemRole
  katlc runs kubeadm init or kubeadm join through the agent executor
  katlc runs local post-kubeadm health checks
  katlc sets generation 1 commitState committed only after kubeadm and operation
  health checks succeed; boot health and persistent default promotion remain
  pending until a later boot reaches katl-boot-complete.target
```

The install path never runs kubeadm init, kubeadm join, CNI installation, or
cluster lifecycle actions. Cluster bootstrap is the explicit operation that both
creates the first Kubernetes-capable generation and runs kubeadm.

## Durable Checkpoints

Before installation begins, no node state is persisted. Discovery, local
handoff, validation, image verification, and install planning may expose
transient status through CLI/API responses or installer logs, but refusal before
mutation must not create durable node state.

Before the target state partition exists, any progress is volatile and
recoverable only from installer logs, input files, and target disk inspection.
Transient diagnostics may use `/run`, but `/run` content is not install state
and is not a checkpoint contract.

After the target state partition is mounted, durable installer state is written
under:

```text
/mnt/target/var/lib/katl/install/state.json
/mnt/target/var/lib/katl/install/input-source.json
/mnt/target/var/lib/katl/install/manifest.json
/mnt/target/var/lib/katl/install/request.json
/mnt/target/var/lib/katl/install/katlos-image.json
/mnt/target/var/lib/katl/install/hardware-facts.json
/mnt/target/var/lib/katl/install/plan.json
/mnt/target/var/lib/katl/install/logs/
```

After runtime boot, the same installed state is visible under:

```text
/var/lib/katl/install/
/var/lib/katl/install/status.json
/var/lib/katl/generations/<generation-id>/spec.json
/var/lib/katl/generations/<generation-id>/status.json
```

The minimum checkpoint fields:

```text
state
completedStates[]
inputMode
inputSource, redacted
requestDigest
katlosImageDigest
generationID
targetDiskStableID
selectedRootSlot
bootArtifactVersion
refusalReason
retryHint
updatedAt
lastError, redacted
```

Checkpoint state is diagnostic and resume-oriented. The source of truth for
already-mutated disk state remains the actual GPT layout, filesystems, installed
generation spec/status, and boot entries.

After the target state partition exists, the installer writes a canonical
install `OperationRecord` under `/var/lib/katl/operations/<operation-id>/`.
Installer checkpoint and status files under `/var/lib/katl/install/` are
operation attachments and operator-facing summaries. `state` is the current
operation phase, and `completedStates[]` is the installer-specific view of
completed phases.
`/var/lib/katl/install/status.json` is the operator-facing install status
summary for the install attempt. The other files under `/var/lib/katl/install/`
are request, plan, artifact, input, or diagnostic attachments referenced by that
summary.
Pre-target discovery and validation remain transient and do not need durable
operation records.

Once the target state partition exists, durable install checkpoint updates must
be backed by the shared `OperationRecord` journal protocol. Destructive disk
phases such as partitioning, formatting, and root-slot writes must durably record
a pre-exec mutation marker before invoking the mutating tool. The actual GPT
layout, filesystems, installed generation spec/status, and boot entries remain
the source of truth for already-mutated disk state.

## Local Handoff Rules

Local handoff starts only when no valid preseeded input is discovered.

Rules:

```text
invalid submissions are rejected and the installer remains waiting
the first schema-valid submission is stored with mode 0600 and accepted
after one schema-valid submission, the handoff API refuses all later submissions
accepted input is converted into the same ValidatedInstallRequest as preseeded
  input
no disk mutation starts until validation succeeds
```

This protects USB and remote-console workflows from accidental second
configuration pushes. A second valid request with different content requires
operator repair action, not silent replacement.

Acceptance consumes the local handoff slot before KatlOS image fetch and
component verification. If image verification later fails, the installer may
retry the same accepted request; replacing the request requires an explicit
repair or restart path.

## Retry And Refuse Behavior

Safe retry cases:

```text
no input found
  continue waiting or retry discovery

invalid local handoff submission
  reject submission and keep waiting

image download or digest verification fails before mutation
  retry fetching or verifying the same accepted request; replacing the request
  requires explicit repair or restart

plan validation fails before mutation
  report and refuse until input or hardware is corrected

interrupted after state partition creation
  reload durable checkpoint, verify requestDigest and target disk identity, and
  continue only through idempotent states

runtime first boot is interrupted before installed-runtime readiness
  rerun runtime units and re-evaluate generation 0 prerequisites
```

Safe retry means deterministic resume through idempotent states using the same
accepted request digest and target disk identity. It is not repair.

Refuse cases:

```text
new requestDigest differs from a durable accepted request
user-facing input contains loose runtimeRoot, UKI, or sysext artifact refs
target disk selector resolves to a different disk after mutation began
existing installed Katl generation conflicts with the requested generation
target disk contains ambiguous partial Katl state that cannot be matched to the
  checkpoint
KatlOS image digest or embedded component digest differs from accepted request
runtime/sysext compatibility metadata is invalid
local handoff receives a second valid request after acceptance
```

Repair tooling may later make some refuse cases recoverable, but the default
installer must not silently reinterpret them.

Repair-required cases must stop in `failed-needs-repair` or
`runtime-failed-needs-repair` and reference a deferred `repair-install-state`
operation shape. `repair-install-state` may inspect checkpoints, disk identity,
Katl GPT state, generation spec/status, and boot entries unless an explicit
operator request authorizes a named mutation. It must not replace the accepted
request, reinterpret a different target disk, delete Kubernetes state, delete
etcd state, or perform destructive reinstall work without a separate
destructive-reset request.

## Failure Diagnostics

Failures record enough context for an operator or VM test to decide whether to
retry, repair, or rebuild inputs.

Diagnostics include:

```text
state and completedStates
inputMode and redacted input source
requestDigest and KatlOS image digest
target disk stable identity and selected root slot, when known
validation errors
image index and component role that failed verification
hardware fact or disk selector mismatch
systemd unit status snippets for runtime first boot
whether any destructive mutation has started
```

Diagnostics must redact credentials, tokens, private keys, kubeadm join
material, and full secret-bearing node config.

## Terminal States

Installer terminal states:

```text
waiting-for-config
install-refused
failed-before-mutation
failed-after-mutation
failed-needs-repair
reboot-requested
```

Runtime terminal states:

```text
installed-runtime-ready
waiting-for-cluster-bootstrap
cluster-bootstrap-complete
runtime-booted-not-ready
runtime-failed-needs-repair
```

`waiting-for-cluster-bootstrap` is success for node installation. It means the
node is installed, generation 0 reached local runtime health, stored cluster
intent is available, and the node can accept an explicit `katlctl cluster
bootstrap` request for a node-local `katlc` operation. It also means Kubernetes
binaries are not active and
kubeadm has not run.

`cluster-bootstrap-complete` means node-local `katlc` created and committed the
first Kubernetes-capable generation through a bootstrap or join operation after
kubeadm and local health checks succeeded.

## Tests And Follow-Up Work

Unit tests should cover:

```text
input discovery precedence
local handoff accepts exactly one valid request
invalid handoff keeps waiting
split refs normalize into ValidatedInstallRequest
requestDigest mismatch refusal
checkpoint resume through idempotent states
pre-mutation versus post-mutation failure classification
redacted diagnostic output
```

Existing follow-up work covers VM validation and dependent implementation:

```text
PXE/preseeded node install-to-bootstrap vmtest

USB/local-handoff node install-to-bootstrap vmtest

persist install-to-bootstrap status checkpoints

compile system roles into per-node install materials

bootstrap node inventory and readiness checks

install manifest schema for one KatlOS image reference

consume single KatlOS install image in installer
```

These are sufficient follow-ups for this decision.
