# Upgrade Kubernetes

Katl upgrades Kubernetes as an explicit, non-interactive cluster rollout.
`katlctl` validates every pending node, then upgrades control planes before
workers, one node at a time, without rebooting the host. The first control plane
runs `kubeadm upgrade apply`; remaining control planes and workers run
`kubeadm upgrade node`.

## Preconditions

- every node in the retained `ClusterConfig` is reachable;
- every node reports either the common source version or the selected target
  version on a committed, healthy generation;
- no other mutating Katl operation is active;
- workloads tolerate a serial control-plane-first rollout; and
- the selected Kubernetes version represents a newer patch or the next minor.

Stacked etcd must be healthy and have no learners, alarms, or active downgrade.
The selected target kubeadm owns etcd image selection, manifest generation, and
upgrade policy. Katl runs its upgrade plan before the rollout and carries
its results and the observed etcd versions as evidence. A successful plan is
kubeadm approval, not an independent certification of every etcd migration
prerequisite. Katl does not maintain an etcd release allowlist or require you to select an etcd version.

Nodes fetch the bundle directly. They need registry and CA access to `ghcr.io`.
Katl resolves the version to an immutable digest compatible with this KatlOS
release; operators do not supply the bundle identity.

New stable Kubernetes versions are built and published automatically. A CLI
with registry discovery can select newly published versions without another
KatlOS or CLI release, provided the bundle declares the installed runtime
compatible. Versions in the CLI's embedded snapshot remain resolvable offline;
new versions require workstation registry access during planning. See
[Kubernetes release delivery](../concepts/kubernetes-releases.md) for the
publication policy and the deferred Actions VM-validation boundary.

## Plan the Kubernetes upgrade

Set `spec.kubernetes.version` in the retained `ClusterConfig` to the target
version. Review the change, then plan the rollout:

```sh
katlctl kubernetes upgrade --config ./cluster.yaml --plan
```

`spec.kubernetes.version` is the rollout's desired version. The command
does not accept a positional target version, so the executed rollout uses the
version in the reviewed configuration.

The `ClusterConfig` supplies the desired version and topology. An optional
saved context supplies workstation shortcuts and observed node
identities. Use `--context NAME` to select a saved context when needed;
the command still requires `ClusterConfig`.

The plan connects to every node, reads its current healthy generation and
Kubernetes payload, derives the control-plane/worker order, and asks every
pending node to validate its operation. Control planes fetch and privately
mount the target bundle, ask its kubeadm to render the etcd Pod with the live
cluster configuration and selected native patches, and inspect etcd directly. The plan shows
the source and target images and whether they differ. It does not create a
candidate generation, take a snapshot, renew certificates, or replace
control-plane manifests. Kubeadm may create temporary health-check objects
during its plan. An explicit local artifact is uploaded for this inspection.

Operators declare the Kubernetes version in the cluster configuration. Katl
selects the release-owned compatible bundle and records its digest, sysext paths
and sizes, candidate generation IDs, operation IDs, and snapshot evidence
internally. An unavailable version fails before any node operation is accepted.

## Execute the rollout

Run the reviewed command without `--plan`:

```sh
katlctl kubernetes upgrade --config ./cluster.yaml
```

The command itself authorizes the rollout; there is no additional confirmation
prompt. It validates every pending operation, then processes every eligible
node serially. Before the first control-plane mutation, every pending control
plane saves a snapshot from the healthy etcd leader. Katl verifies the snapshot's
embedded checksum, database status, revision, and file digest, and records its
location and observed membership. All preparation must succeed before the first
mutating upgrade request.

An etcd snapshot contains the replicated cluster keyspace. Snapshots on different
nodes can have different revisions while writes continue; coordinating their
creation does not create a stronger consistency boundary. Keeping pre-upgrade
snapshots on several nodes protects against losing the first upgraded node's
local copy. A cluster restore uses one chosen snapshot consistently, rather
than combining node snapshots. See [etcd disaster recovery](https://etcd.io/docs/v3.6/op-guide/recovery/).

Before restarting each voter, Katl checks every etcd endpoint directly with
authenticated, linearizable reads, verifies cluster and member identities,
checks alarms and replication progress, and requires enough healthy surviving
voters for quorum. In a three-voter cluster, both other voters must be caught
up. Kubernetes `/readyz` is an additional check. A single-control-plane cluster
has no availability guarantee during its local etcd restart.

The target sysext is mounted privately so target `kubeadm` runs while the
source kubelet remains active. Kubeadm owns the local etcd image, manifest,
certificate, and data upgrade behavior, including its own failure handling.
Katl checks the surviving voters immediately before invoking it. After kubeadm
succeeds, Katl stops kubelet, switches and refreshes the Kubernetes sysext, then
restarts kubelet. Running pod containers remain owned by containerd during this
short window. Katl checks target kubelet, the local API, and direct etcd health.
If the image changed, it also requires a new container on the selected image.
The same member must belong to the same cluster and be caught up before Katl
promotes the live candidate as the persistent boot default and advances without
rebooting.

The default path neither cordons nor drains nodes. To prevent new pods from
being scheduled onto the node during its upgrade, opt into temporary cordoning:

```sh
katlctl kubernetes upgrade --config ./cluster.yaml \
  --cordon --kubeconfig ./kubeconfig
```

This flag temporarily cordons a schedulable node and restores scheduling
afterward. A node that you already cordoned remains cordoned, including when
the upgrade fails. The flag does not evict running pods.
[Kubernetes upstream recommends draining before minor
kubelet upgrades](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-upgrade/).
Katl's no-drain default deliberately favors uninterrupted home-lab workloads;
use an external maintenance workflow if your workloads or support policy
require the conservative upstream procedure.

The command stops immediately on the first failed or recovery-required node and
does not send mutating upgrade requests to the remaining nodes. Their
pre-upgrade snapshot preparation has already completed.

To resume an interrupted rollout, restore the same configuration revision
and rerun the command. Nodes already at `spec.kubernetes.version` are
skipped. Every pending node must still report the rollout's common source
version, so Katl cannot silently resume with a different source/target pair.
Do not change `spec.kubernetes.version` while a rollout is incomplete.

After a successful rollout, check workload-level health with your normal
Kubernetes tooling, for example `kubectl get nodes` and `kubectl get pods -A`.

## Failure boundary

A failure before control-plane mutation abandons the candidate and permits a
corrected retry. A failure after kubeadm, etcd, or Kubernetes API mutation reports
`recoveryRequired: true` and stops the rollout. Host generation rollback does
not reverse etcd, kubeadm, Kubernetes API, or workload changes. Katl does not
restore a source Kubernetes sysext after control-plane mutation. Kubeadm can
attempt its own local etcd manifest or data rollback; Katl records what it
observes afterward and still stops on kubeadm failure. Selecting a host
generation with an older Kubernetes payload is refused after that mutation
boundary.

Preserve the command output and node diagnostics. Do not retry blindly. Follow
[Troubleshoot KatlOS](troubleshoot.md) and use the snapshot path recorded on the
affected control plane when planning manual recovery. The operation and JSON
report retain member and cluster IDs, server, cluster and storage versions,
applied indexes, snapshot evidence, and observations from surviving peers.
An interrupted request is uncertain until this evidence is inspected.

If cp-1 cannot start etcd, keep cp-2 and cp-3 serving. Inspect membership from
a survivor:

```sh
katlctl cluster etcd members --config ./cluster.yaml --coordinator cp-2
```

Repair cp-1 explicitly, or follow
[one-node replacement](wipe-reinstall.md#plan-one-node-replacement). Do not
restore a whole-cluster snapshot over a serving quorum. Katl does not implement
whole-cluster snapshot restore or authorize starting an older etcd against
potentially migrated data.

If the original quorum is permanently lost and you choose reconstruction,
follow [whole-cluster wipe and reinstall](wipe-reinstall.md#plan-a-whole-cluster-wipe),
[bootstrap Kubernetes](bootstrap-kubernetes.md), reapply your GitOps entrypoint,
and restore workload data with your VolSync backups. Verify those external
sources and backups before wiping: this path discards the original Kubernetes
API state. Katl prepares the nodes; you manage GitOps and workload recovery.
