# Upgrade Kubernetes

Use this procedure to move a Katl-bootstrapped cluster to a newer Kubernetes
patch or the next minor. Katl runs the upgrade as an explicit, non-interactive
cluster rollout: `katlctl` validates every pending node, then upgrades control
planes before workers, one node at a time, without rebooting the host. The
first control plane runs `kubeadm upgrade apply`; remaining control planes and
workers run `kubeadm upgrade node`.

## Before you start

- Every node in `cluster.yaml` must be reachable.
- Every node must report either the common source version or the selected
  target version on a committed, healthy generation.
- No other mutating Katl operation may be active.
- Workloads must tolerate a serial control-plane-first rollout.
- The target must be a newer patch or the next minor.
- Stacked etcd must be healthy and have no learners, alarms, or active
  downgrade.

The target kubeadm chooses the etcd version and generates its manifest; you do
not select an etcd version. Katl runs the target `kubeadm upgrade plan` before
the rollout, records its result and the observed etcd versions, and stops if
the plan fails. A successful plan is kubeadm's approval; Katl does not
separately certify etcd migration prerequisites.

## Choose a target version

Nodes fetch the Kubernetes bundle directly, so they need registry and CA access
to `ghcr.io`. Katl resolves the version to an immutable digest compatible with
this KatlOS release; you do not supply the Kubernetes bundle identity. An
unavailable version fails before any node operation is accepted.

New stable Kubernetes versions are built and published automatically. A CLI
with registry discovery can select newly published versions without another
KatlOS or CLI release, provided the Kubernetes bundle declares the installed
runtime compatible. Versions in the CLI's embedded snapshot remain resolvable
offline; new versions require workstation registry access during planning. See
[Kubernetes release delivery](../concepts/kubernetes-releases.md) for how
versions are published and kept available.

## Plan the Kubernetes upgrade

Set `spec.kubernetes.version` in `cluster.yaml` to the target version. Review
the change, then plan the rollout:

```sh
katlctl kubernetes upgrade --config ./cluster.yaml --plan
```

The command does not accept a positional target version, so the executed
rollout always uses `spec.kubernetes.version` from the reviewed configuration.
`--context NAME` can select a saved context, the workstation record of observed
node identities, but the command still requires `--config`.

The plan connects to every node, reads its current healthy generation and
Kubernetes bundle, derives the control-plane/worker order, and asks every
pending node to validate its operation. Control planes fetch and privately
mount the target Kubernetes bundle, have its kubeadm render the etcd Pod from
the live cluster configuration and selected native patches, and inspect etcd
directly.
The plan shows the source and target images and whether they differ. It does
not create a candidate generation, take a snapshot, renew certificates, or
replace control-plane manifests, although kubeadm may create temporary
health-check objects during its plan. An explicit local artifact is uploaded
for this inspection.

## Execute the rollout

Run the reviewed command without `--plan`:

```sh
katlctl kubernetes upgrade --config ./cluster.yaml
```

The command itself authorizes the rollout; there is no confirmation prompt. It
then proceeds in this order:

1. Validate every pending node operation.
2. On every pending control plane, save an etcd snapshot from the healthy
   leader. All preparation must succeed before the first mutating request.
3. For each node, control planes first: check the surviving etcd voters, then
   run the target `kubeadm` from the privately mounted target Kubernetes bundle
   while the source kubelet remains active.
4. Stop kubelet, switch to and refresh the target Kubernetes bundle, and
   restart kubelet.
5. Run the post-restart checks described below. Only when they all pass does
   Katl promote the live candidate as the persistent boot default and continue
   to the next node without rebooting.

Katl verifies each snapshot's embedded checksum, database status, revision, and
file digest, and records its location and the observed membership. Each
control plane keeps its snapshot at
`/var/lib/etcd/katl-snapshots/OPERATION_ID.db`. When a control plane's upgrade
succeeds, Katl keeps that node's two newest pre-upgrade snapshots, deletes
older ones, and reports the kept paths; see
[pre-upgrade snapshots](back-up-etcd.md#pre-upgrade-snapshots). Snapshots
taken on different nodes can have different revisions while writes continue,
so keeping several protects against losing the first upgraded node's copy; a
cluster restore uses one chosen snapshot rather than combining them. See
[etcd disaster recovery](https://etcd.io/docs/v3.6/op-guide/recovery/).

Kubeadm renews its control-plane certificates during the upgrade, because Katl
runs `kubeadm upgrade` with kubeadm's default certificate renewal. Refresh your
workstation kubeconfig afterward as described in
[Renew Kubernetes certificates](renew-certificates.md#refresh-your-kubeconfig).

Before restarting each voter, Katl checks every etcd endpoint directly with
authenticated, linearizable reads, verifies cluster and member identities,
checks alarms and replication progress, and requires enough healthy, caught-up
surviving voters for quorum; in a three-voter cluster both other voters must be
caught up. Kubernetes `/readyz` is an additional check. A single-control-plane
cluster has no availability guarantee during its local etcd restart. Kubeadm
owns the local etcd image, manifest, certificate, and data upgrade, including
its own failure handling. Running pod containers remain owned by containerd
while kubelet restarts.

After the restart, Katl checks the target kubelet, the local API, and direct
etcd health. The same etcd member must belong to the same cluster and be caught
up, and if the image changed Katl also requires a new container on the selected
image. These checks gate the promotion in step 5.

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
corrected retry. A failure after kubeadm, etcd, or Kubernetes API mutation
reports `recoveryRequired: true` and stops the rollout. Katl does not restore
the source Kubernetes bundle after control-plane mutation, and it refuses to
select a host generation with an older Kubernetes bundle after that boundary;
host rollback would not reverse the etcd, kubeadm, Kubernetes API, or workload
changes anyway (see
[rollback is not a backup](../concepts/generations.md#rollback-is-not-a-backup)).
Kubeadm can attempt its own local etcd manifest or data rollback; Katl records
what it observes afterward and still stops on kubeadm failure.

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
[one-node replacement](change-cluster-nodes.md#replace-one-node-without-renaming-it).
Do not restore a whole-cluster snapshot over a serving quorum. Katl does not
implement whole-cluster snapshot restore or authorize starting an older etcd
against potentially migrated data.

If the original quorum is permanently lost and you choose reconstruction,
follow [whole-cluster wipe and reinstall](wipe-reinstall.md#plan-a-whole-cluster-wipe),
[bootstrap Kubernetes](bootstrap-kubernetes.md), reapply your GitOps entrypoint,
and restore workload data with your backup tool. Verify those external
sources and backups before wiping: this path discards the original Kubernetes
API state. Katl prepares the nodes; you manage GitOps and workload recovery.
