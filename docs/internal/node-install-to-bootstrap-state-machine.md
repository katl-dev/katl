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
  kubeadm config refs, not kubeadm actions
  one katlosImage reference with digest and expected metadata
```

`systemRole`, the requested Kubernetes version, and kubeadm config refs are
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
  runtime/sysext compatibility; verify the manifest Kubernetes version resolves
  to exactly one bundled sysext candidate without activating it for generation 0

PlanInstall
  collect hardware facts, resolve target disk selectors, build a typed install
  plan, and refuse unsafe or ambiguous disk state

CheckExistingInstallState
  inspect any existing Katl GPT, state partition, generation metadata, and boot
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
  systemd operation wiring available; this generation 0 handoff does not activate
  Kubernetes binaries, require /etc/kubernetes projection, containerd running,
  kubelet availability, or katl-kubeadm-ready.target

WaitingForClusterBootstrap
  terminal install handoff state for the node; operator-run katlctl cluster
  bootstrap may now ask katlc to validate stored intent, create the first
  Kubernetes-capable candidate generation, and run the appropriate kubeadm
  workflow
```

Cluster bootstrap creates the first Kubernetes-capable generation and then runs
kubeadm:

```text
katlctl cluster bootstrap
  ask katlc to validate stored cluster intent
  select the bundled Kubernetes sysext whose payload version exactly matches the
  install manifest version
  render kubeadm config refs under /etc/katl
  project /etc/kubernetes from writable state
  ensure containerd prerequisites, kubelet service wiring, and systemRole
  metadata
  create and activate generation 1 as a candidate

KubeadmReady
  reach katl-kubeadm-ready.target after local prerequisites are active

KubeadmOperation
  record a BootstrapCluster attempt for the init node or a JoinCluster attempt
  for joining nodes based on cluster inventory and systemRole
  run kubeadm init or kubeadm join
  run local post-kubeadm health checks
  commit generation 1 only after kubeadm and health checks succeed
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
/var/lib/katl/generations/<generation-id>/metadata.json
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
generation metadata, and boot entries.

After the target state partition exists, installer checkpoint and status files
are the installer operation record. `state` is the current operation phase, and
`completedStates[]` is the installer-specific view of completed phases.
`/var/lib/katl/install/status.json` is the operator-facing run record for the
install attempt. The other files under `/var/lib/katl/install/` are request,
plan, artifact, input, or diagnostic attachments referenced by that record.
Pre-target discovery and validation remain transient and do not need durable
operation records.

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
`runtime-failed-needs-repair` and reference a deferred `RepairInstallState`
operation shape. `RepairInstallState` may inspect checkpoints, disk identity,
Katl GPT state, generation metadata, and boot entries unless an explicit
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
bootstrap` operation. It also means Kubernetes binaries are not active and
kubeadm has not run.

`cluster-bootstrap-complete` means the bootstrap or join operation created and
committed the first Kubernetes-capable generation after kubeadm and local health
checks succeeded.

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
