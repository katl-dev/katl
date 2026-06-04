# Node Install-To-Bootstrap State Machine

Status: current decision.

This document defines how a node moves from installer boot to the kubeadm-ready
runtime handoff. It covers both network/PXE-style install with input supplied up
front and USB/local-handoff install where an operator supplies input after the
installer boots.

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
  systemRole and capability selections
  kubeadm config refs, not kubeadm actions
  one katlosImage reference with digest and expected metadata
```

`systemRole` and capabilities are installed as node intent for later cluster
bootstrap. They do not cause `kubeadm init` or `kubeadm join` during node
install.

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
  runtime/sysext compatibility

PlanInstall
  collect hardware facts, resolve target disk selectors, build a typed install
  plan, and refuse unsafe or ambiguous disk state

CheckExistingInstallState
  inspect any existing Katl GPT, state partition, generation metadata, and boot
  entries before deciding whether to continue, repair, or refuse

PrepareTarget
  wipe explicitly authorized target devices, create partitions, format writable
  filesystems, and mount the target

InstallGeneration
  write the runtime root slot, install boot assets, stage sysexts, materialize
  generated confext, write mount units, write seed data, and create generation
  metadata

WriteInstallRecord
  persist the request, selected image metadata, hardware facts, plan, generation
  record digest, and final checkpoint under target state

SetBootCandidate
  make the verified generation bootable only after target verification succeeds

RebootToRuntime
  request reboot into the installed runtime generation
```

`WaitForLocalConfig` is a non-mutating waiting state, not a failure. It may run
indefinitely until an operator supplies one valid request or stops the installer.

## Runtime States

After reboot, the installed runtime continues the handoff:

```text
RuntimeFirstBoot
  boot selected generation, mount writable state, and load install record

ActivateGeneration
  activate selected sysexts and generated confext for the generation

PrepareKubeadmPrereqs
  project /etc/kubernetes from writable state, ensure containerd prerequisites,
  kubelet service wiring, kubeadm config refs, and role/capability metadata

KubeadmReady
  reach katl-kubeadm-ready.target after local prerequisites are active

WaitingForClusterBootstrap
  terminal handoff state for the node; operator-run katlctl cluster bootstrap
  may now connect and decide init or join based on cluster inventory/systemRole
```

The runtime path must not run kubeadm init, kubeadm join, CNI installation, or
cluster lifecycle actions. Those are explicit operator actions after
`katl-kubeadm-ready.target`.

## Durable Checkpoints

Before the state partition exists, progress is volatile and recoverable only
from installer logs, input files, and target disk inspection.

Initial checkpoints may be written under:

```text
/run/katl/install/state.json
```

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

runtime first boot is interrupted before kubeadm-ready
  rerun runtime units and re-evaluate local prerequisites
```

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
kubeadm-ready
waiting-for-cluster-bootstrap
runtime-booted-not-ready
runtime-failed-needs-repair
```

`waiting-for-cluster-bootstrap` is success for node installation. It means the
node is installed, has reached `katl-kubeadm-ready.target`, and is ready for the
bounded operator-run cluster bootstrap CLI. It also means node install has not
run `kubeadm init` or `kubeadm join`.

## Tests And Follow-Up Beads

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

Existing Beads cover VM validation and dependent implementation:

```text
katl-dty.11.12
  PXE/preseeded node install-to-bootstrap vmtest

katl-dty.11.13
  USB/local-handoff node install-to-bootstrap vmtest

katl-dty.11.15
  persist install-to-bootstrap status checkpoints

katl-dty.11.2
  compile system roles and capabilities into per-node install materials

katl-dty.11.9
  bootstrap node inventory and readiness checks

katl-dty.12.11
  install manifest schema for one KatlOS image reference

katl-dty.12.3
  consume single KatlOS install image in installer
```

These are sufficient follow-ups for this decision.
