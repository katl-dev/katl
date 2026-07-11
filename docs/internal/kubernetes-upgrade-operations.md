# Kubernetes Upgrade Operations

Status: working design.

This document applies the generation and operation model from
`docs/internal/generations-and-operations.md` to Kubernetes upgrades.

Katl uses generations to describe desired host state: KatlOS version,
Kubernetes sysext version, rendered configuration, and health expectations.
Kubernetes upgrades remain explicit operations. `katlc` plans and records the
node-local operation, executes it through the long-running `katlc` agent, uses
systemd for service supervision, targets, health checks, and rollback
mechanisms, and kubeadm remains authoritative for mutating Kubernetes cluster
state.

## Decision

Katl models a Kubernetes upgrade as an explicit operation attached to a
candidate generation. The candidate generation selects:

```text
current or updated KatlOS runtime root
target Kubernetes sysext payload
rendered generated confext
operation metadata for the kubeadm upgrade step
local host health expectations after the operation
```

The candidate generation describes the desired steady state after the upgrade
operation completes. It does not mean every service may immediately consume the
target Kubernetes sysext. Target-version kubeadm access before kubelet restart
is operation-scoped tool access, not a second generation and not a partial sysext
selection. The generation still selects one Kubernetes sysext as the
post-operation host state.

The upgrade operation is not a normal runtime configuration apply. It is a
kubeadm-aware action with its own plan, status, diagnostics, and failure
semantics. Normal confext activation must not run `kubeadm upgrade`, `kubectl`,
CNI installers, add-on installers, GitOps controllers, or package managers.

Katl owns:

```text
artifact verification and compatibility checks
target Kubernetes sysext selection
candidate generation creation
rendered kubeadm input under /etc/katl
agent-managed execution of the node-local operation
operation status and diagnostics
host boot health, host rollback, and retry reporting
```

Kubeadm owns:

```text
upgrade plan semantics
kubeadm upgrade apply
kubeadm upgrade node
control-plane static pod manifest mutation
kubelet configuration updates
kubeadm-managed certificate renewal behavior
kubeadm-managed Kubernetes objects such as kubeadm and kubelet ConfigMaps
```

The operator owns:

```text
maintenance window selection
cluster-wide rollout ordering
which control-plane node runs kubeadm upgrade apply
node drain and uncordon decisions
CNI, CoreDNS, kube-proxy, GitOps, and workload-specific policy
manual recovery when Kubernetes cluster state needs repair
```

`katlctl` may provide a higher-level control-client UX for operator-driven
multi-node rollout order, but it must only submit explicit requests to
node-local `katlc` and observe returned operation IDs. It must not generate
candidate generations, create operation records, own retry state, or turn the
node configuration agent into a continuous Kubernetes lifecycle controller.

## Upgrade Flow

Kubernetes upgrades must follow kubeadm and Kubernetes version-skew policy.
Katl should reject or require explicit operator override for requests that skip
minor versions, downgrade Kubernetes, or select a Kubernetes sysext incompatible
with the current KatlOS runtime root and rendered configuration.

The operation starts with a node-local plan. It reuses the generation planning
and status primitives from `katlc apply`, but it is an explicit kubeadm-aware
operation because it mutates an already bootstrapped or joined node:

```text
read current generation spec, status, and boot selection
inspect current Kubernetes and kubelet versions
select the target Kubernetes sysext
validate target kubeadm API compatibility for rendered input
create a candidate generation with explicit upgrade operation metadata
write an agent-executed operation plan and status record
```

The candidate generation is staged before kubeadm mutates cluster state, but
Katl must not blindly restart kubelet into the target payload before kubeadm has
advanced the node. This matters because Katl currently packages `kubeadm`,
`kubelet`, and `kubectl` together in one Kubernetes sysext. The upgrade path
must make target-version kubeadm available for the upgrade operation while
controlling when target-version kubelet becomes active.

Katl must split target tool availability from target service activation. The
agent executor may use target-version kubeadm from the staged candidate payload,
but kubelet must remain on the source payload or be stopped until the kubeadm
phase for that node has completed. Target-version kubelet becomes active only at
the explicit kubelet restart phase recorded by the operation. Health promotion
for the candidate generation requires kubelet to be running from the selected
target sysext.

Normal boot or activation of a candidate Kubernetes upgrade generation must not
race kubelet into the target payload before the operation gate allows it.

The first implementation should use an explicit agent-executed upgrade
operation rather than ordinary boot activation alone. That operation runs with
the candidate generation context, records phase transitions, and restarts
kubelet only at the point accepted by the kubeadm flow for that node role.

## Target Kubeadm Access

The first supported `targetKubeadmAccess.mode` is
`operation-private-sysext`.

The upgrade executor mounts or otherwise exposes the verified target Kubernetes
sysext only inside the node-local operation tool view, for example under
`/var/lib/katl/operations/<operation-id>/tools/kubernetes/`. The executor runs
target-version `kubeadm` from that private path and records the observed version,
artifact digest, mount path, and argv in the operation record. The global
systemd-sysext view remains on the source generation until the operation reaches
the recorded target kubelet restart phase.

The first supported `kubeletActivationGate` is
`operation-released-target-kubelet`.

Upgrade candidate generations install a generated `kubelet.service` drop-in
that keeps target-version kubelet inactive until the katlc agent releases the
operation gate at:

```text
/run/katl/operation-gates/<operation-id>/target-kubelet-released
```

The drop-in must be enforced by systemd before `ExecStart` for kubelet. A direct
boot into a Kubernetes-upgrade candidate with no matching released gate leaves
kubelet inactive and reports the candidate health as blocked instead of silently
starting target kubelet. The katlc agent releases the gate only after the
operation has flushed the post-kubeadm mutation marker and entered the planned
`kubelet-restart-running` phase. The operation then restarts kubelet, verifies
that the running kubelet reports the target version, and only then allows host
generation health promotion.

Source-version kubelet remains running until the recorded kubeadm phase
completes for control-plane and worker nodes. `stop-source-kubelet-before-kubeadm`
is not part of the first supported path because control-plane kubelet owns the
static pod lifecycle and stopping it before kubeadm creates a larger failure
surface. Any later role-specific stop policy needs its own decision and VM gate.

Candidate mechanisms considered:

```text
operation-private-sysext
  Run target-version kubeadm from the staged target sysext inside the upgrade
  operation executor's private tool view or mount namespace. Global activation stays on the
  source generation until the planned kubelet restart.

separate-kubeadm-tool-payload
  Publish a small target-version kubeadm tool artifact tied by digest and version
  to the target Kubernetes sysext. This simplifies service gating but adds
  packaging and compatibility bookkeeping.

transient-global-sysext-with-kubelet-gate
  Globally expose the target sysext before kubeadm runs, but block kubelet with a
  hard systemd gate until kubeadm completes. This is higher risk because any
  missed ordering can start target kubelet too early.

stop-source-kubelet-before-kubeadm
  Per-role option only, not the default. For control-plane nodes this is risky
  because kubelet manages static pods.
```

This decision selects the first access mode and gate. Mutating Kubernetes
upgrade execution is supported only through the kubeadm-upgrade operation
executor. `katlc` continues to reject normal apply requests that change the
Kubernetes sysext on an already bootstrapped node. An upgrade candidate records
the authorizing operation ID, `operation-private-sysext` target kubeadm access,
and the `operation-released-target-kubelet` activation gate. Boot activation
accepts the changed sysext only when the referenced operation succeeded,
committed that exact candidate and payload digest, and observed the target
kubelet after releasing the gate.

Repair access after host rollback uses the same operation-private sysext mode.
Katl reopens the failed operation, verifies the target sysext digest from the
operation record, and recreates the private tool view from the retained artifact
or a freshly fetched artifact with the same digest. Katl must not preserve a
hidden globally active target sysext after rollback. If the target artifact
cannot be verified, repair is refused until the operator supplies or fetches the
matching payload again.

Combined KatlOS root plus Kubernetes sysext upgrades are unsupported in the
first mutating Kubernetes upgrade implementation. The first supported path keeps
the KatlOS runtime root generation constant and changes only the Kubernetes
sysext plus generated config required by kubeadm. A combined root and
Kubernetes change needs a separate gate proving host boot rollback, target
kubeadm repair access, and Kubernetes post-mutation recovery together.

Required gates before mutating execution is enabled:

```text
unit tests for request rejection, selected targetKubeadmAccess.mode, and
  kubeletActivationGate transitions
golden tests for the generated kubelet gate drop-in and operation record fields
systemd-analyze verify for kubelet.service, the generated drop-in, and
  katl-kubeadm-ready.target ordering
VM test proving target kubeadm runs from operation-private sysext while source
  kubelet stays active
VM test proving direct boot into an unreleased upgrade candidate blocks target
  kubelet
VM test proving repair access after host rollback recreates only the private
  target kubeadm tool view
```

## Etcd Ownership For Upgrades

Day-one Katl control-plane support is stacked etcd only. External etcd remains
out of scope until a separate decision defines credentials, member health,
backup, restore, upgrade, and disaster recovery gates.

For stacked etcd, Katl owns storage planning, mount ordering, operation records,
redacted diagnostics, and the health evidence it collects. Kubeadm and etcd own
member creation, member removal, data contents under `/var/lib/etcd`, static pod
manifests, and etcd certificates. The default data path is `/var/lib/etcd` on
the writable state partition. A dedicated `KATL_ETCD` partition may be mounted
there when the install plan explicitly selects it, but it is persistent node
state and not a generation artifact.

Every mutating Kubernetes upgrade that can affect control-plane or etcd state
requires snapshot evidence before kubeadm is invoked. The first supported path
must refuse control-plane upgrade execution unless the operation request names a
verified etcd snapshot record with at least:

```text
snapshotRef
snapshotDigest
snapshotRevision, when observable
createdAt
capturedMemberListDigest
sourceKubernetesVersion
sourceEtcdVersion, when observable
storageLocation
operatorIdentityContext
```

The snapshot may be operator-managed or produced by a future Katl snapshot
operation, but the upgrade operation must record what was checked. Katl must not
write raw snapshots into normal operation records and must redact credentials or
keys from snapshot metadata. Worker-only `kubeadm upgrade node` operations do
not require a new snapshot, but they must reference the control-plane snapshot
record that authorized the rollout.

Upgrade ordering is serialized:

```text
1. verify API health, etcd endpoint health, and member list before the first
   control-plane mutation
2. run exactly one kubeadm upgrade apply operation on the selected apply node
3. verify API health, local etcd health, and member list after the apply node
4. run additional control-plane kubeadm upgrade node operations one at a time
5. after each control-plane node, verify API health, local etcd health, member
   list, and quorum before continuing
6. upgrade workers only after the control-plane rollout is healthy
```

Katl must reject minor-version skips, downgrades, and Kubernetes or kubeadm
version-skew violations before staging a mutating operation. A healthy
three-control-plane cluster may tolerate one unavailable etcd member, but a
planned upgrade must not intentionally make more than one member unavailable or
continue when quorum evidence is unknown. Failed control-plane upgrade or join
operations stop the rollout; Katl must not remove members, restore snapshots, or
retry on another control-plane automatically.

Host rollback after etcd or Kubernetes control-plane mutation is not cluster
rollback. Rolling back the KatlOS root, Kubernetes sysext, or generated config
does not roll back `/var/lib/etcd`, `/etc/kubernetes`, Kubernetes API objects,
static pod manifests, member records, or etcd schema/data changes. The operation
record must state whether kubeadm or etcd mutation started, whether snapshot
evidence exists, and whether the next supported action is retry, kubeadm-aware
repair, snapshot restore, or destructive wipe/reinstall.

Minimum gates before mutating upgrade execution:

```text
unit tests for snapshot-required rejection and version-skew rejection
golden tests for etcd evidence and snapshot fields in operation records
VM test for three-control-plane serial upgrade with member/quorum evidence
VM test for failed post-mutation control-plane upgrade that records recovery
  required and does not auto-remove or restore etcd state
snapshot/restore VM tests before Katl claims snapshot recovery support
```

## Post-Decision Execution Sketches

The following role flows are the intended shape after the selected target
kubeadm access mode and kubelet activation gate have implementation and VM
coverage. They are not supported execution paths before that gate is closed.

Control-plane apply node:

```text
stage target Kubernetes sysext in a candidate generation
make target kubeadm available to the operation
run kubeadm upgrade plan
record the selected target version
run kubeadm upgrade apply <target-version>
drain policy is operator-controlled or explicitly requested by the operation
restart kubelet using the target Kubernetes payload
run local post-upgrade health checks
commit the candidate host generation or leave boot health pending according to
  the operation result
```

Additional control-plane nodes:

```text
stage target Kubernetes sysext in a candidate generation
make target kubeadm available to the operation
run kubeadm upgrade node
restart kubelet using the target Kubernetes payload
run local post-upgrade health checks
commit the candidate host generation or leave boot health pending according to
  the operation result
```

Worker nodes:

```text
stage target Kubernetes sysext in a candidate generation
make target kubeadm available to the operation
run kubeadm upgrade node
restart kubelet using the target Kubernetes payload
run local post-upgrade health checks
commit the candidate host generation or leave boot health pending according to
  the operation result
```

Drain and uncordon are intentionally not hidden inside generation activation.
Katl may offer an explicit operation flag or a higher-level control-client flow
for those steps, but the operation status must show whether they were requested,
skipped, or left to the operator.

## State And Status

Every accepted Kubernetes upgrade request creates a node-local `OperationRecord`
under `/var/lib/katl/operations/<operation-id>/`. It should follow the shared
operation model and reference both the previous and candidate generation IDs
when a candidate exists. Rejected or plan-only requests before the execution gate
may omit a candidate generation ID and must record why no mutating execution was
allowed.

The operation record should include:

```text
source request digest and operator identity context
previous generation id
candidate generation id
node role for upgrade purposes
current Kubernetes version
target Kubernetes version
current and target Kubernetes sysext digests
rendered kubeadm config path and digest
requested drain and uncordon behavior
target kubeadm access mode and observed kubeadm version
targetKubeadmAccess.mode
targetKubeadmAccess.artifactPath
targetKubeadmAccess.artifactDigest
targetKubeadmAccess.observedVersion
sourceKubeletPolicy: keep-running | stop-before-kubeadm
kubelet activation gate state
kubeletActivationGate.state: locked | released | restart-running |
  target-observed | blocked
kubeletActivationGate.enforcementUnit
kubeletActivationGate.bootTokenPath
globalTargetSysextActiveBeforeKubeadmMutation
targetKubeadmRepairAccessAfterRollback
whether kubelet was kept running, stopped, or restarted before kubeadm mutation
observed kubelet version before and after the planned restart
phase
timestamps
redacted kubeadm output and diagnostic artifact paths
```

Upgrade operation records also use operation-kind-specific evidence.
`kubeadm-upgrade` control-plane evidence includes:

```text
upgradeMode: apply | node
sourceKubernetesVersion
targetKubernetesVersion
sourceSysextDigest
targetSysextDigest
kubeadmEvidence:
  plan phase
  apply or node phase
  firstMutationPhase
  selected target version
apiEvidence:
  before upgrade
  after kubeadm mutation
  after kubelet restart
staticPodManifestEvidence:
  before/after digests for control-plane manifests and etcd manifest
  kubeadm backup directory paths under /etc/kubernetes/tmp
etcdMemberEvidence:
  member list and local member ID before and after
  endpoint health or quorum probe result when available
kubeletEvidence:
  version before and after restart
  node Ready/version before and after when API reachable
```

`kubeadm-upgrade` worker evidence includes:

```text
sourceKubernetesVersion
targetKubernetesVersion
kubeadmEvidence:
  subcommand: upgrade node
  currentPhase
  completedPhases[]
  firstMutationPhase
apiEvidence:
  endpoint before and after
  node object UID, Ready state, and kubelet version before and after
staticPodManifestEvidence:
  not-applicable; any control-plane manifest on a worker is a diagnostic anomaly
etcdMemberEvidence:
  not-applicable
kubeletEvidence:
  kubelet config digest before and after
  kubelet service state before and after planned restart
```

Suggested phases:

```text
planned
execution-refused-unsupported
staged
kubeadm-plan-running
kubeadm-plan-complete
kubeadm-apply-running
kubeadm-node-running
kubelet-restart-running
health-check-running
healthy
failed
host-rollback-running
host-rolled-back
```

Control-plane apply operations use the `kubeadm-apply-running` phase. Additional
control-plane nodes and workers use `kubeadm-node-running`.

Diagnostics should preserve enough information for rerun and repair while
redacting tokens, private keys, bearer credentials, kubeconfigs, and discovery
material. Kubeadm backup directories under `/etc/kubernetes/tmp` are diagnostic
artifacts. Katl must not treat them as its own rollback database.
Redaction applies to argv, environment, stdout/stderr, temporary configs,
kubeconfigs, and diagnostic artifacts before they enter normal operation
records.

## Failure And Rollback

Katl distinguishes host rollback from kubeadm-mutated node or cluster-state
rollback. The generic host rollback boundary is defined in
`docs/internal/rollback-selection-rules.md`.

Before kubeadm mutates cluster state, a failed candidate can be abandoned like
any other failed host generation. Katl returns the node to the previous
known-good generation and reports the rejected or failed operation.

After kubeadm mutates node or cluster state, host rollback cannot promise to undo
the Kubernetes upgrade. Kubeadm may roll back some control-plane changes when an
upgrade fails, and kubeadm upgrade operations are intended to be rerunnable, but
Katl must not claim that selecting the previous sysext generation restores the
previous Kubernetes node or cluster state.

Failure handling after kubeadm mutation should be:

```text
stop automatic activation of further upgrade phases
flush externalMutationStarted and a pre-exec mutation marker before invoking
  kubeadm upgrade apply or kubeadm upgrade node
record the kubeadm phase, exit status, and diagnostic artifacts
leave enough target-version kubeadm access for rerun or repair
avoid repeated automatic retries unless explicitly requested
report whether host generation rollback was performed
report that Kubernetes state may require kubeadm-aware repair
```

If kubeadm has already mutated node or cluster state but target kubelet has not
yet been activated, rollback still selects a complete previous host generation.
The operation status must report that Kubernetes state may now expect the target
version and that target-version kubeadm repair access may still be required.
Rollback must not preserve the target sysext as a hidden repair tool. Any
target-version kubeadm access after rollback remains operation-scoped and must be
visible in diagnostics.

If host boot fails after staging or after kubelet restart, normal Katl rollback
selects the previous known-good generation as defined by the generation
spec/status model. The operation status must still report whether kubeadm
already mutated node or cluster state before the host rollback occurred.

Any reboot during `kubeadm-apply-running`, `kubeadm-node-running`, or unknown
target-kubelet gate state becomes stale-post-mutation or stale-ambiguous during
katlc agent startup audit. It requires explicit retry or repair; Katl must not
automatically rerun kubeadm.

Rollback must never independently switch only the root slot, sysext, or confext.
Host rollback selects a complete previous generation. Kubernetes state repair is
a separate kubeadm-aware operation.

After kubeadm mutation, `recoveryRequired` may name a deferred kubeadm-aware
operation type, but Katl must not automatically perform it. The operation status
should record whether the next safe action is retry, operator inspection,
kubeadm repair, etcd snapshot restore, or destructive wipe/reinstall. Host rollback may
make the node bootable; it must not report Kubernetes cluster state repaired
unless a separate kubeadm/etcd-aware operation succeeds.

## Configuration Changes During Upgrade

Rendered kubeadm input may change as part of the candidate generation, but
Katl must keep two cases separate:

```text
Kubernetes version upgrade
  Explicit kubeadm upgrade operation. The target sysext version changes and the
  operation runs kubeadm upgrade apply or kubeadm upgrade node.

Kubeadm or kubelet configuration change without a Kubernetes version upgrade
  Render desired input and record explicit action required. Normal runtime
  configuration apply does not mutate live Kubernetes objects.
```

Katl should validate the rendered kubeadm API version against the selected
target Kubernetes sysext. If `kubernetesVersion` is present in the rendered
kubeadm config, it must match the target payload version or fail validation
before staging the operation.

User-supplied config must still not own kubeadm output paths:

```text
/etc/kubernetes
/var/lib/kubelet
/var/lib/katl/kubernetes
```

Those paths remain mutable kubeadm and kubelet state, projected or persisted
from the writable state partition.

## Health Checks

The host generation becomes healthy only after local checks pass. Initial
checks should be deliberately bounded:

```text
selected Kubernetes sysext is active
kubeadm, kubelet, and kubectl report the expected target version
kubelet.service is active after the planned restart
/etc/kubernetes remains projected from writable state
containerd.service is active
node-local kubeadm output expected for the role exists
```

Control-plane API readiness may be checked when local credentials are available,
but cluster-wide convergence, CNI readiness, add-on health, and workload health
remain outside the node-local host health contract unless an explicit higher
level control-client workflow requests and records those checks through
node-local operations.

## Testing Contract

Unit and golden tests should cover:

```text
upgrade request validation
version-skew and minor-skip rejection
target sysext compatibility validation
generation spec/status for an upgrade operation
operation status phase transitions
redaction of kubeadm diagnostics
separation of host rollback from Kubernetes cluster-state rollback
```

VM release gates cover:

```text
single-control-plane patch upgrade through kubeadm apply
worker upgrade after the control plane
real stacked-etcd snapshot evidence before control-plane mutation
candidate generation staging, commit, reboot activation, and node readiness
pre-mutation failure and post-mutation repair-required operation semantics
```

The v0.1 proof pair is Kubernetes v1.36.0 to v1.36.1 in a two-node VM cluster.
Multi-control-plane rolling upgrade, member replacement, and destructive
failure recovery remain separate gates.

## Deferred Questions

Deferred implementation choices:

```text
Should a later role-specific path stop source kubelet before target kubeadm for
  workers or non-apply control planes?

Should target kubeadm eventually be split into a separate signed tool payload
  after operation-private sysext is proven?

What UX should katlctl expose for operator-managed snapshot records before Katl
  has its own snapshot operation?

When can combined KatlOS root plus Kubernetes sysext upgrades be enabled after
  the independent root-update and Kubernetes-upgrade gates exist?
```

## References

Primary upstream behavior is defined by Kubernetes documentation:

```text
https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-upgrade/
https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-upgrade/
https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/upgrading-linux-nodes/
https://kubernetes.io/releases/version-skew-policy/
https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-reconfigure/
```

Local Katl boundary decisions:

```text
docs/internal/generations-and-operations.md
docs/internal/generation-metadata-model.md
docs/internal/rollback-selection-rules.md
docs/internal/kubeadm-config-input-design.md
docs/internal/adrs/adr-002-live-and-next-boot-config-apply-modes.md
docs/internal/adrs/adr-003-runtime-config-input-and-trust.md
docs/internal/systemd-sysupdate-update-decision.md
```
