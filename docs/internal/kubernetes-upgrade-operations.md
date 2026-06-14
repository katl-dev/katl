# Kubernetes Upgrade Operations

Status: working design.

This document applies the generation and operation model from
`docs/internal/generations-and-operations.md` to Kubernetes upgrades.

Katl uses generations to describe desired host state: KatlOS version,
Kubernetes sysext version, rendered configuration, and health expectations.
Kubernetes upgrades remain explicit operations. `katlc` plans and records the
node-local operation, systemd executes and supervises it with native units,
targets, health checks, and rollback mechanisms, and kubeadm remains
authoritative for mutating Kubernetes cluster state.

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
systemd units and targets that execute the node-local operation
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

`katlctl` may provide a higher-level coordinator for operator UX and multi-node
rollout order, but that coordinator must still submit explicit operations. It
must not turn the node configuration agent into a continuous Kubernetes
lifecycle controller.

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
write a systemd-supervised operation plan and status record
```

The candidate generation is staged before kubeadm mutates cluster state, but
Katl must not blindly restart kubelet into the target payload before kubeadm has
advanced the node. This matters because Katl currently packages `kubeadm`,
`kubelet`, and `kubectl` together in one Kubernetes sysext. The upgrade path
must make target-version kubeadm available for the upgrade operation while
controlling when target-version kubelet becomes active.

Katl must split target tool availability from target service activation. The
upgrade unit may use target-version kubeadm from the staged candidate payload,
but kubelet must remain on the source payload or be stopped until the kubeadm
phase for that node has completed. Target-version kubelet becomes active only at
the explicit kubelet restart phase recorded by the operation. Health promotion
for the candidate generation requires kubelet to be running from the selected
target sysext.

Normal boot or activation of a candidate Kubernetes upgrade generation must not
race kubelet into the target payload before the operation gate allows it.

The first implementation should use an explicit upgrade unit or transient unit
rather than ordinary boot activation alone. That unit runs with the candidate
generation context, records phase transitions, and restarts kubelet only at the
point accepted by the kubeadm flow for that node role.

## Target Kubeadm Access

The concrete target kubeadm access mode is an implementation decision that must
be made before Kubernetes upgrade execution is enabled. Candidate mechanisms:

```text
operation-private-sysext
  Run target-version kubeadm from the staged target sysext inside the upgrade
  unit's private tool view or mount namespace. Global activation stays on the
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

Until one access mode and one kubelet activation gate are selected, implemented,
and VM-tested, Kubernetes upgrade execution is unsupported by default. `katlc`
must reject normal apply requests that change the Kubernetes sysext on an
already bootstrapped node. `katlctl` or `katlc` may produce a plan-only
operation record, but must not select the candidate for boot, globally activate
the target sysext, run kubeadm, or restart kubelet.

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
Katl may offer an explicit operation flag or a higher-level coordinator for
those steps, but the operation status must show whether they were requested,
skipped, or left to the operator.

## State And Status

Every accepted Kubernetes upgrade request creates an `OperationRecord` under the
Katl writable state tree. It should follow the shared operation model and
reference both the previous and candidate generation IDs.

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
`UpgradeControlPlane` evidence includes:

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

`UpgradeWorker` evidence includes:

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
boot-time operation reconciliation. It requires explicit retry or repair; Katl
must not automatically rerun kubeadm.

Rollback must never independently switch only the root slot, sysext, or confext.
Host rollback selects a complete previous generation. Kubernetes state repair is
a separate kubeadm-aware operation.

After kubeadm mutation, `recoveryRequired` may name a deferred kubeadm-aware
operation type, but Katl must not automatically perform it. The operation status
should record whether the next safe action is retry, operator inspection,
kubeadm repair, etcd snapshot restore, or destructive rebuild. Host rollback may
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
level coordinator requests and records those checks.

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

VM tests should eventually cover:

```text
single-node control-plane minor upgrade
multi-control-plane rolling upgrade with one apply node and one node upgrade
worker upgrade after the control plane
failed kubeadm upgrade that records diagnostics and remains rerunnable
failed host boot after sysext staging that rolls back only host generation
```

Until those VM tests exist, Kubernetes upgrade execution remains unsupported by
default. Plan-only records are allowed; mutating execution is refused until the
target kubeadm access mode and kubelet activation gate are selected,
implemented, and proven.

## Open Questions

Open implementation choices:

```text
Which targetKubeadmAccess.mode is supported first?

Should source kubelet continue running until kubeadm completes for all roles, or
  should any role stop kubelet before invoking target kubeadm?

What exact systemd gate enforces target kubelet activation?

Are combined KatlOS root plus Kubernetes sysext upgrades unsupported until this
  gate is proven?

How is target-version kubeadm repair access exposed after host rollback without
  preserving a hidden target sysext?
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
