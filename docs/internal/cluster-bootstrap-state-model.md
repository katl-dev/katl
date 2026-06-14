# Cluster Bootstrap State Model

Status: working design.

This document defines where cluster state lives after `katlctl cluster
bootstrap` asks node-local `katlc` to create the first Kubernetes-capable
generation and run kubeadm. It makes explicit which artifacts are node-local,
which are cluster-global, and what Katl may recover.

## Decision

Generation 0 contains cluster intent only. It does not contain Kubernetes
cluster state.

Cluster bootstrap is an explicit operation. The init node's operation creates
the first Kubernetes-capable candidate generation, runs kubeadm init, and
records redacted evidence. Join operations create equivalent candidate
generations on joining nodes and run kubeadm join.

After kubeadm mutates state, the source of truth for Kubernetes cluster state is
kubeadm-owned output, Kubernetes API state, and etcd data. Katl owns the host
generation, projections, operation records, redacted evidence, and recovery
boundaries. Katl does not copy cluster PKI, tokens, kubeconfigs, or etcd
contents into generation metadata.

## State Layers

Bootstrap touches three different state layers:

```text
generation state
  Katl-owned desired host state under /var/lib/katl/generations

node-local kubeadm state
  kubeadm and kubelet output on one node, including projected /etc/kubernetes,
  /var/lib/kubelet, and local /var/lib/etcd on control-plane nodes

cluster-global state
  Kubernetes API objects, etcd cluster identity, CA/key material, service
  account signing keys, uploaded certificate material, and bootstrap tokens
```

Only the first layer is selected and rolled back by Katl generations.
Node-local kubeadm state and cluster-global state survive host generation
rollback unless an explicit destructive recovery or reset operation owns the
mutation.

## Cluster-Global Artifacts

| Artifact | Primary owner | Physical or API location | Regenerable | Backup stance |
| --- | --- | --- | --- | --- |
| Kubernetes cluster CA | kubeadm/Kubernetes | `/etc/kubernetes/pki/ca.{crt,key}` on control-plane nodes | No, not for the same cluster identity without trust disruption | Must be backed up by an explicit operator or future Katl backup workflow |
| Front-proxy CA | kubeadm/Kubernetes | `/etc/kubernetes/pki/front-proxy-ca.{crt,key}` | No, not transparently | Back up with cluster PKI |
| Etcd CA and peer/client certs | kubeadm/etcd | `/etc/kubernetes/pki/etcd/*` | Some leaf certs are renew/recreate candidates; CA loss is not transparent | Back up with cluster PKI and etcd recovery material |
| Service account signing keys | Kubernetes control plane | `/etc/kubernetes/pki/sa.{key,pub}` | Rotation is possible only as an explicit cluster operation with token impact | Back up with cluster PKI |
| Admin and component kubeconfigs | kubeadm/Kubernetes | `/etc/kubernetes/*.conf` | Rebuildable only when required CA/key material and kubeadm state are intact | Back up or regenerate through explicit kubeadm-aware repair |
| Cluster ID and API object identity | Kubernetes API/etcd | etcd data under `/var/lib/etcd` for stacked etcd | No; new etcd data means a different cluster history | Covered by etcd snapshots |
| Etcd cluster/member identity | etcd | `/var/lib/etcd` on control-plane nodes and etcd membership state | No safe implicit regeneration; replacement must be member-aware | Covered by etcd snapshots and member evidence |
| Bootstrap tokens | Kubernetes API secrets | `kube-system` token Secrets | Yes, while a healthy control plane exists | Do not back up as durable config; recreate when needed |
| Certificate key and uploaded certs | kubeadm/Kubernetes | kubeadm uploaded-certs Secret, encrypted with certificate key | Yes, while CA material and a healthy control plane exist | Treat as temporary join material, not durable Katl config |
| Control-plane endpoint | User intent rendered into kubeadm input and kubeconfig output | Stored cluster intent, rendered kubeadm config, kubeconfigs, and API clients | Changeable only through explicit endpoint/reconfiguration workflow | Back up as cluster intent and user infrastructure config |

Workers do not own cluster-global bootstrap artifacts. A worker stores only its
node-local kubelet state, kubeadm join output, and operation evidence.

## Node Ownership

The init control-plane node is the first writer of cluster-global state. That is
not permanent ownership.

After bootstrap:

```text
single control-plane cluster
  the only control-plane node holds the only local copy of cluster PKI and etcd
  data unless the operator has created external backups

multi-control-plane cluster
  control-plane nodes hold replicated API/etcd state and local copies of the
  kubeadm-managed PKI needed for their role

joining control-plane node
  receives or creates kubeadm-owned PKI, static pod manifests, kubeconfigs, and
  etcd membership as part of its bootstrap-join-control-plane operation
```

Katl records where kubeadm says these artifacts exist and records redacted
digests, presence, expiry, node identity, member identity, and endpoint evidence
in `OperationRecord`s. The records are recovery evidence, not secret backup
stores.

## Backup Boundary

Katl must not imply that a successful bootstrap is disaster-recoverable unless
there is an explicit backup story.

Day-one stance:

```text
automatic cluster PKI backup
  unsupported

automatic etcd snapshot backup
  unsupported

operation record evidence
  supported for diagnostics and retry classification, but not sufficient for
  cluster restore

operator-managed backup
  required for preserving a cluster after all control-plane state is lost
```

A future `BackupClusterState` or `CreateEtcdSnapshot` operation may add a
Katl-supported backup path. That design must define artifact paths, encryption
or external secret handling, retention, restore validation, and redaction before
Katl claims recoverability from backup.

Minimum backup material for same-cluster disaster recovery is expected to
include:

```text
etcd snapshot with revision and digest
/etc/kubernetes/pki CA material needed by kubeadm and Kubernetes
service account signing material
kubeadm ClusterConfiguration and endpoint intent
control-plane node/member inventory
Katl generation and operation records for participating nodes, when available
```

## Operation Records

Bootstrap and join operation records must identify cluster-global mutation
boundaries.

Required bootstrap-state evidence:

```text
clusterStateEvidence:
  controlPlaneEndpoint
  endpointSource: intent | cli-override | rendered-kubeadm | observed-api
  clusterName, when rendered or observed
  observedServerVersion, when API is reachable
  caCertDigest, public certificate only
  serviceAccountPublicKeyDigest, public key only
  adminKubeconfigPresent
  uploadedCertsObserved, redacted
  bootstrapTokenPresent, redacted
  etcdClusterID, when observable
  localEtcdMemberID, when applicable
  etcdMemberListDigest, when observable
  backupStatus: unknown | not-configured | operator-managed | katl-managed
```

The record must never store raw private keys, bootstrap tokens, certificate
keys, bearer tokens, kubeconfig client keys, or etcd snapshots by default.

## Failure Boundaries

Before kubeadm's first mutation, a failed bootstrap candidate can be abandoned
and the node can return to the previous host generation.

After kubeadm's first mutation, Katl must classify failure using live probes and
operation evidence. Host rollback may select an earlier generation, but it does
not remove:

```text
/etc/kubernetes backing state
/var/lib/kubelet state
/var/lib/etcd state
Kubernetes API objects
etcd member records
cluster CA or service account keys
```

Returning the host to generation 0 is safe only while the generation 0
cluster-state invariant still holds: no Kubernetes PKI, static pod manifests,
kubelet join state, etcd data, or kubeadm-created API state exists for that
cluster attempt. Once that invariant is broken, cleanup requires an explicit
reset, repair, or recovery operation.

## Node 1 Loss

If the first control-plane node dies:

```text
single control-plane, no backup
  the original cluster is unrecoverable by Katl; the supported path is a
  destructive wipe/reinstall followed by bootstrap of a new cluster identity

single control-plane, valid backup
  recovery requires a future explicit restore operation that validates PKI,
  etcd snapshot, version compatibility, and generation state before mutation

multi-control-plane with quorum
  a new control-plane node may join after the operator removes or replaces the
  failed etcd member through an explicit recovery operation

multi-control-plane without quorum
  recovery requires an etcd disaster recovery workflow; Katl must not infer a
  safe repair from generation state alone
```

`katlctl` may help submit recovery requests and display status, but it is not
the recovery database. Recovery must be reconstructable from node-local Katl
state, kubeadm/Kubernetes state, etcd state, and explicit backup artifacts.

## Links

Related decisions:

```text
docs/internal/cluster-bootstrap-cli.md
docs/internal/cluster-recovery-and-rebuild.md
docs/internal/generations-and-operations.md
docs/internal/persistent-state-inventory.md
docs/internal/stacked-etcd-bootstrap-data-policy.md
```
