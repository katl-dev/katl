# Upgrade Kubernetes

Katl upgrades Kubernetes as an explicit, non-interactive cluster rollout.
`katlctl` validates every pending node, then upgrades control planes before
workers, one node at a time, without rebooting the host. The first control plane
runs `kubeadm upgrade apply`; remaining control planes and workers run `kubeadm
upgrade node`.

## Preconditions

- every node is reachable through the selected `katlctl` workstation context;
- every node reports either the common source version or the selected target
  version on a committed, healthy generation;
- per-node `credentialRef` values point to protected token files;
- no other mutating Katl operation is active;
- workloads tolerate a serial control-plane-first rollout; and
- the selected Kubernetes version represents a newer patch or the next minor.

Nodes fetch the bundle directly. They need registry and CA access to `ghcr.io`.
Katl resolves the version to an immutable digest compatible with this KatlOS
release; operators do not supply the bundle identity.

## Plan

```sh
katlctl cluster upgrade kubernetes \
  v1.36.1 --plan
```

The shorter top-level form is equivalent:

```sh
katlctl kubernetes upgrade v1.36.1 --plan
```

By default, `katlctl` uses the current context in its workstation configuration.
Use `--context NAME`, `--config PATH`, or `--inventory PATH` when needed.

The plan connects to every node, reads its current healthy generation and
Kubernetes payload, derives the control-plane/worker order, and asks every
pending node to validate its operation. It does not fetch a bundle, create a
candidate generation, take a snapshot, or run kubeadm.

Operators provide only the cluster selection and Kubernetes version. Katl
selects the release-owned compatible bundle and records its digest, sysext paths
and sizes, candidate generation IDs, operation IDs, and snapshot evidence
internally. An unavailable version fails before any node operation is accepted.

## Execute

Run the same command without `--plan`:

```sh
katlctl kubernetes upgrade v1.36.1
```

The command itself authorizes the rollout; there is no additional confirmation
prompt. It validates every pending operation, then processes every eligible
node serially. Each node fetches and verifies the OCI bundle before mutation.
Control-plane nodes capture a local pre-upgrade etcd snapshot and member-list
digest. The target sysext is mounted privately so target `kubeadm` runs while
the source kubelet remains active. After kubeadm succeeds, Katl stops kubelet,
switches and refreshes the Kubernetes sysext, then restarts kubelet. Running pod
containers remain owned by containerd during this short window. Katl checks the
target kubelet and local API health, promotes the live candidate as the
persistent boot default, and continues the rollout without rebooting.

The default path neither cordons nor drains nodes. To prevent new pods from
being scheduled onto the node during its upgrade, opt into temporary cordoning:

```sh
katlctl kubernetes upgrade v1.36.1 \
  --cordon --kubeconfig ./kubeconfig
```

This flag cordons before the node operation and uncordons afterward; it does not
evict running pods. [Kubernetes upstream recommends draining before minor
kubelet upgrades](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-upgrade/).
Katl's no-drain default deliberately favors uninterrupted home-lab workloads;
use an external maintenance workflow if your workloads or support policy
require the conservative upstream procedure.

The command stops immediately on the first failed or recovery-required node and
does not touch the remaining nodes.

After a successful rollout, check workload-level health with your normal
Kubernetes tooling, for example `kubectl get nodes` and `kubectl get pods -A`.

## Failure boundary

A failure before kubeadm mutation abandons the candidate and permits a corrected
retry. A failure after kubeadm or Kubernetes API mutation reports
`recoveryRequired: true` and stops the rollout. Host generation rollback does
not reverse etcd, kubeadm, Kubernetes API, or workload changes.

Preserve the command output and node diagnostics. Do not retry blindly. Follow
[Troubleshoot KatlOS](troubleshoot.md) and use the snapshot path recorded on the
affected control plane when planning manual recovery.
