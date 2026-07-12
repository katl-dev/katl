# Upgrade Kubernetes

Katl upgrades Kubernetes as an explicit cluster operation. `katlctl` validates
every pending node, then upgrades one node per invocation. Rerunning the same
command advances control planes before workers. The first control plane runs
`kubeadm upgrade apply`; remaining control planes and workers run `kubeadm
upgrade node`.

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
  --bundle ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1 \
  --plan
```

By default, `katlctl` uses the current context in its workstation configuration.
Use `--context NAME`, `--config PATH`, or `--inventory PATH` when needed.

The plan connects to every node, reads its current healthy generation and
Kubernetes payload, derives the control-plane/worker order, and asks every
pending node to validate its operation. It does not fetch a bundle, create a
candidate generation, take a snapshot, or run kubeadm.

Operators provide only the cluster selection and bundle reference. Katl derives
and records bundle digests, sysext paths and sizes, candidate generation IDs,
operation IDs, and snapshot evidence internally.

## Execute

Run the same command without `--plan`:

```sh
katlctl cluster upgrade kubernetes \
  --bundle ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1
```

The command validates every pending operation but executes only the next
eligible node. That node fetches and verifies the OCI bundle before mutation.
Control-plane nodes capture a local pre-upgrade etcd snapshot and member-list
digest. The target sysext is mounted privately so target `kubeadm` runs while
the source kubelet remains active. Katl releases the target kubelet only after
kubeadm succeeds, performs local health checks, and commits the candidate
generation.

The command stops immediately on the first failed or recovery-required node. It
does not continue through the remaining cluster.

## Reboot and verify

After the node operation succeeds, reboot the named node. Wait for the node and
its workloads to become healthy:

```sh
ssh root@cp-1.example.test systemctl reboot
kubectl --kubeconfig ./kubeconfig get nodes -o wide
kubectl --kubeconfig ./kubeconfig get pods -A
```

Also require `katl-boot-complete.target` to be active on the rebooted node. The
upgrade is not fully proven until the committed candidate has passed boot
health. Then rerun the same `katlctl cluster upgrade kubernetes` command. It
recognizes nodes already on the target version and advances the next control
plane or worker. Repeat the execute, reboot, and verify loop until the command
reports that every node already runs the selected version.

## Failure boundary

A failure before kubeadm mutation abandons the candidate and permits a corrected
retry. A failure after kubeadm or Kubernetes API mutation reports
`recoveryRequired: true` and stops the rollout. Host generation rollback does
not reverse etcd, kubeadm, Kubernetes API, or workload changes.

Preserve the command output and node diagnostics. Do not retry blindly. Follow
[Troubleshoot KatlOS](troubleshoot.md) and use the snapshot path recorded on the
affected control plane when planning manual recovery.
