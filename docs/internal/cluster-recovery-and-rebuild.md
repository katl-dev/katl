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
`OperationRecord`, executed under systemd supervision, and health-gated. A
`katlctl` command may coordinate requests, but it must not become the recovery
state store. General wipe/reinstall uses the installer or a future destructive
reset contract, not a cluster recovery operation.

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
