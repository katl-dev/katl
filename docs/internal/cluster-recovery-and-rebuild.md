# Cluster Recovery And Rebuild

Status: working design.

This document defines the disaster-recovery and rebuild boundary for Katl
clusters bootstrapped with kubeadm. It is intentionally conservative: Katl may
help operators run explicit recovery operations, but it must not infer cluster
repair from host generation rollback. A general cluster rebuild is a destructive
wipe and reinstall path, not a same-cluster recovery operation.

## Decision

Cluster recovery is an explicit operation family. It is not install, normal
generation activation, boot-time reconciliation, or a hidden retry inside
`katlctl cluster bootstrap`.

Katl may recover host generation bookkeeping from node-local state. It may
classify kubeadm, Kubernetes, and etcd failures using operation records and live
probes. It must not automatically recreate cluster CA material, restore etcd
snapshots, remove etcd members, reuse stale `/var/lib/etcd`, or rerun kubeadm
init/join after a post-mutation interruption.

For day one, and likely beyond, Katl's general rebuild story is:

```text
wipe selected nodes or disks with explicit data-loss acknowledgement
install clean KatlOS generation 0
run a new explicit cluster bootstrap
create a new Kubernetes cluster identity
```

That path discards the old cluster unless the operator uses a separate,
explicit, supported restore workflow. Katl must not present a general
`rebuild-cluster` command as disaster recovery.

## Recovery Inputs

Supported recovery decisions depend on explicit inputs:

```text
node-local Katl generation and operation records
kubeadm-owned state under projected /etc/kubernetes
kubelet state under /var/lib/kubelet
etcd state or snapshots
Kubernetes API reachability and observed cluster state
control-plane endpoint intent
operator acknowledgement of data loss, when destructive action is requested
```

Katl operation records contain redacted evidence. They do not replace backups of
etcd snapshots, cluster PKI, service account keys, kubeconfigs, or operator
secret material.

## Scenario Matrix

| Scenario | Supported initial behavior | Required explicit operation before automation |
| --- | --- | --- |
| Failed bootstrap before kubeadm mutation | Abandon candidate generation and return host to previous known-good state | `retry-operation` may rerun once live probes prove no mutation occurred |
| Failed bootstrap after kubeadm mutation | Classify as `failed-needs-repair` with mutation scopes | `kubeadm-reset`, `retry-operation`, or destructive wipe/reinstall when the operator discards the cluster |
| Interrupted worker join | Re-probe node object, kubelet state, and `/etc/kubernetes`; skip only when already joined | `retry-operation` with same request digest and valid join material |
| Interrupted control-plane join | Stop rollout and require repair; do not continue additional joins | `replace-etcd-member`, `kubeadm-reset`, or retry after member/state proof |
| Single control-plane node lost, no backup | Original cluster is not recoverable by Katl | Wipe/reinstall and bootstrap a new cluster identity |
| Single control-plane node lost, valid backup | Refuse until restore contract exists | `restore-etcd-snapshot` plus PKI restore and version checks |
| One control-plane lost in healthy multi-control-plane cluster | Existing cluster may continue if quorum remains | `recover-control-plane` or `replace-etcd-member` followed by control-plane join |
| Majority of control planes lost | No implicit repair | `restore-etcd-snapshot` or operator-managed etcd disaster recovery |
| Lost cluster CA private key | Not recoverable as same trust root by Katl | Restore from backup or wipe/reinstall and bootstrap a new cluster identity |
| Expired bootstrap token | Recreate when API and credentials are healthy | Bounded join-material refresh operation |
| Lost certificate key for uploaded certs | Regenerate/upload when CA material and control plane are healthy | Bounded control-plane join-material operation |
| Etcd data exists on a node being reinstalled | Refuse non-destructive reuse | Destructive reset or explicit reattach/recover operation with member proof |

## Reinstall And Reattach

A reinstall is not automatically a recovery.

Rules:

```text
destructive reinstall
  may create a clean generation 0 and discard local Kubernetes state when the
  operator explicitly acknowledges data loss

non-destructive reinstall or repair
  must preserve /var/lib/katl, projected /etc/kubernetes, /var/lib/kubelet, and
  /var/lib/etcd unless a specific operation owns a mutation

reattach existing etcd data
  unsupported by default; requires proof of cluster ID, member ID, peer URLs,
  certificates, endpoint reachability, and Kubernetes version compatibility

reinstall a lost worker
  can join as a new node after stale node identity is resolved by the operator

reinstall a lost control-plane node
  requires explicit etcd membership handling before or during join
```

If existing kubeadm or etcd state is detected on a node that claims to be clean
generation 0, Katl must refuse bootstrap and require reset or recovery. A host
generation alone does not prove cluster-state cleanliness.

## Destructive Rebuild Boundary

General cluster rebuild is not a Katl recovery operation.

Rules:

```text
wipe/reinstall selected nodes
  allowed only through the installer's destructive install guard or a future
  destructive reset contract with explicit data-loss acknowledgement

new bootstrap after wipe/reinstall
  creates a new Kubernetes cluster identity and new kubeadm-owned cluster state

old cluster state
  is discarded unless an explicit restore workflow imports validated backup
  artifacts

same-cluster rebuild without backup
  unsupported
```

This keeps the default operating model simple: if the operator chooses general
rebuild, Katl returns nodes to clean generation 0 and then performs normal
bootstrap. It does not try to reinterpret stale cluster PKI, stale etcd data, or
old operation records as a recoverable cluster.

## v0.1 Wipe/Reinstall Contract

The v0.1 wipe/reinstall path creates a new clean cluster identity. It does not
preserve or reattach the previous Kubernetes cluster, stacked-etcd cluster,
cluster CA, service account keys, bootstrap tokens, kubeconfigs, node object
identity, CNI state, or Katl operation history from the selected nodes.

The user-facing acknowledgement must be explicit and specific. A request is not
accepted unless the operator supplies an affirmative destructive flag and a
human-readable acknowledgement equivalent to:

```text
I understand this will erase KatlOS, Kubernetes, kubelet, etcd, CNI, operation,
and generation state on the selected nodes and bootstrap a new cluster identity.
```

Short flags such as `--force`, `--yes`, or a repeated failed bootstrap attempt
are not enough. The installer manifest requires `install.wipeTarget: true`, and
install status records `wipeTargetAccepted` after validation. User-facing wipe
commands may require stronger cluster lifecycle acknowledgements before they
produce installer manifests. Diagnostics must not record secrets from the
discarded cluster.

For v0.1, wipe/reinstall owns only selected KatlOS target disks and Katl-owned
state on those disks:

```text
wiped and recreated
  Katl GPT partitions on the selected target disk, including ESP/XBOOTLDR when
  present, root slots, writable state, optional Katl-owned etcd partition, boot
  entries, generation specs/status, selected sysext/confext links, cached
  Kubernetes and app payloads, operation records, /etc/kubernetes backing
  state, /var/lib/kubelet, /var/lib/containerd, /var/lib/etcd, CNI state, node
  identity, machine-id, SSH host keys, and generated confext output

preserved
  operator-managed backups, off-node artifact repositories, external cluster
  backups, and non-target disks unless the install manifest explicitly selects
  a Katl-owned extra data disk with its own destructive wipe authorization

refused
  ambiguous target disk identity, mismatch with the accepted request digest
  after mutation starts, existing non-Katl partitions that are not selected by
  the destructive install plan, or any attempt to keep stale kubeadm/etcd state
  while also claiming clean generation 0
```

Stale Kubernetes state is handled by deleting the local state through this
declared destructive path, not by running an implicit `kubeadm reset` or by
editing individual files. After wipe/reinstall, the node must satisfy the same
generation 0 invariants as a fresh install:

```text
generation 0 is the selected and booted generation
no Kubernetes sysext or app extension is selected
no kubeadm output exists under the projected /etc/kubernetes backing store
/var/lib/kubelet and /var/lib/etcd are empty or absent before bootstrap
operation records contain only the new install/reinstall operation history
machine identity and SSH host keys are regenerated for the clean node
stored bootstrap intent names the requested Kubernetes bundle source/ref
katl-kubeadm-ready.target is not reached until a later bootstrap operation
  creates a Kubernetes-capable generation
```

The operation is idempotent only around the same accepted destructive request.
Before destructive mutation starts, a rejected or failed request can be retried
after fixing validation errors. After mutation starts, re-running the same
request may continue or repeat the wipe/reinstall against the same stable target
disk until clean generation 0 is reached. A changed request digest, changed
target disk identity, partial state that cannot be matched to the accepted
request, or a request that asks to preserve old Kubernetes state must stop in a
repair-required state and require a new explicit destructive request.

Failure recovery is conservative:

```text
failure before mutation
  no disk state is changed; the operator may resubmit after fixing input

failure after disk wipe or repartition
  the old cluster is already discarded; recovery is to rerun the same accepted
  destructive reinstall or start a new destructive request after inspecting the
  checkpoint

failure after generation 0 boots but before re-bootstrap
  treat the node as a clean installed node if generation 0 invariants pass;
  otherwise rerun destructive reinstall, not kubeadm repair

failure during subsequent bootstrap
  follows the bootstrap mutation-boundary rules; host rollback does not restore
  the discarded cluster
```

Before this is user-facing, the VM proof must run a repeatable multi-node flow:

```text
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run TestWipeReinstallThreeControlPlane -count=1
```

The test must install and bootstrap three control-plane nodes, prove Kubernetes
state existed, submit explicit destructive wipe/reinstall intent, verify clean
generation 0 invariants on every node, then bootstrap again and prove the new
cluster is usable. The test must also prove stale generation records,
`/etc/kubernetes`, `/var/lib/kubelet`, `/var/lib/etcd`, CNI state, selected
extension links, and cached payload selections from the old cluster are gone or
recreated only as part of the new bootstrap.

## Certificate And Key Recovery

Katl distinguishes renewal from replacement:

```text
leaf certificate renewal
  possible through explicit kubeadm-aware renewal operations when CA material is
  intact

kubeconfig regeneration
  possible only when required CA material and identity context are available

service account key rotation
  possible only as an explicit cluster operation with documented token impact

cluster CA loss
  unsupported for same-cluster recovery without backup

etcd CA loss
  unsupported for same-etcd-cluster recovery without backup
```

Certificate operations must record redacted evidence, never normal-log private
keys, and never treat generation rollback as certificate rollback.

## Recovery Operation Names

Operation names document recovery boundaries. They do not imply implementation
support until a contract and tests exist.

```text
recover-control-plane
  replace or repair a failed control-plane node while preserving a cluster

replace-etcd-member
  remove, add, or replace a named stacked-etcd member with quorum checks

restore-etcd-snapshot
  restore a declared snapshot into a declared topology

kubeadm-reset
  perform an explicit destructive kubeadm/node reset surface
```

Each operation must be accepted by node-local `katlc`, persisted as an
`OperationRecord`, executed by the long-running `katlc` agent's internal
executor, and health-gated. Systemd may supervise the agent and normal KatlOS
services, but it is not the operation dispatcher. A `katlctl` command may
coordinate requests, but it must not become the recovery state store. General
wipe/reinstall uses the installer or a future destructive reset contract, not a
cluster recovery operation.

## Backup Requirements

Katl must report backup status explicitly when it can observe or manage it.
Until a Katl backup operation exists, backup status is either `unknown`,
`not-configured`, or `operator-managed`.

Recovery from total control-plane loss requires, at minimum:

```text
valid etcd snapshot and digest
cluster PKI backup, including CA private keys and service account keys
control-plane endpoint and kubeadm ClusterConfiguration intent
Kubernetes and etcd version compatibility information
intended recovery topology and node identities
```

If any required artifact is missing, Katl should refuse same-cluster recovery
and clearly distinguish that refusal from a host generation failure.

## Refusal Principles

Katl must refuse recovery when it cannot prove safety.

Refusal cases include:

```text
lost cluster CA without backup
lost etcd quorum without snapshot or operator-managed etcd recovery
stale /var/lib/etcd with unknown member identity
generation 0 bootstrap request on a node with existing kubeadm output
operation record missing mutation boundary for a mutating workflow
request digest mismatch between failed operation and retry
node identity mismatch
Kubernetes version incompatibility for the requested restore
data-loss acknowledgement missing for destructive reset or rebuild
```

The refusal output should name the state that blocked progress and the explicit
operation or external backup material required to proceed.

## Validation Gates

Before any recovery operation becomes supported, it needs:

```text
unit tests for operation planning and refusal cases
golden tests for operation records and redacted diagnostics
systemd unit verification where practical
VM tests for interrupted bootstrap/join and refused recovery paths
etcd snapshot restore tests before claiming snapshot recovery
multi-control-plane quorum and member replacement tests before claiming
  control-plane recovery
```

Documentation-only changes do not require VM gates. Implementation of these
operations does.
