# Upgrade Kubernetes

Katl upgrades Kubernetes as an explicit, non-interactive cluster rollout.
`katlctl` validates every pending node, then upgrades and reboots control planes
before workers, one node at a time. The first control plane runs `kubeadm
upgrade apply`; remaining control planes and workers run `kubeadm upgrade node`.

## Preconditions

- every node is reachable through the selected `katlctl` workstation context;
- every node reports either the common source version or the selected target
  version on a committed, healthy generation;
- per-node `credentialRef` values point to protected token files;
- no other mutating Katl operation is active;
- workloads tolerate a serial control-plane-first rollout; and
- the selected bundle represents a newer patch or the next Kubernetes minor.

Nodes fetch the bundle directly. They need registry and CA access to `ghcr.io`.
An immutable `@sha256:` suffix is recommended but not required.

## Plan

```sh
katlctl cluster upgrade kubernetes \
  v1.36.1-katl.1 --plan
```

The shorter top-level form is equivalent:

```sh
katlctl kubernetes upgrade v1.36.1-katl.1 --plan
```

By default, `katlctl` uses the current context in its workstation configuration.
Use `--context NAME`, `--config PATH`, or `--inventory PATH` when needed.

The plan connects to every node, reads its current healthy generation and
Kubernetes payload, derives the control-plane/worker order, and asks every
pending node to validate its operation. It does not fetch a bundle, create a
candidate generation, take a snapshot, or run kubeadm.

Operators provide only the cluster selection and bundle version. Katl derives
and records bundle digests, sysext paths and sizes, candidate generation IDs,
operation IDs, and snapshot evidence internally.

## Execute

Run the same command without `--plan`:

```sh
katlctl kubernetes upgrade v1.36.1-katl.1
```

The command itself authorizes the rollout; there is no additional confirmation
prompt. It validates every pending operation, then processes every eligible
node serially. Each node fetches and verifies the OCI bundle before mutation.
Control-plane nodes capture a local pre-upgrade etcd snapshot and member-list
digest. The target sysext is mounted privately so target `kubeadm` runs while
the source kubelet remains active. Katl releases the target kubelet only after
kubeadm succeeds, performs local health checks, and commits the candidate
generation.

After each node-local operation succeeds, `katlctl` requests a reboot through
the authenticated agent, waits for the agent to restart, and requires the
target generation and Kubernetes payload to pass boot health before continuing.
The command stops immediately on the first failed, rolled-back, or
recovery-required node and does not touch the remaining nodes.

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
