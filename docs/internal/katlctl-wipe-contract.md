# katlctl Wipe Command Contract

This document defines the v0.1 user-facing contract for destructive wipe flows.
It is implementation guidance for `katlctl wipe node` and
`katlctl wipe cluster`; it does not make either command supported until the
implementation and VM gates named here exist.

## Shared Contract

Both commands are explicit operator actions. They are not automatic repair,
retry, rollback, or reconciliation flows.

Required destructive acknowledgement text:

```text
I understand this will erase KatlOS, Kubernetes, kubelet, etcd, CNI, operation, and generation state on the selected nodes and bootstrap a new cluster identity.
```

Required common flags:

```text
--inventory PATH
  Cluster inventory containing node names, addresses, roles, and node-local
  katlc access credentials or credential references.

--confirm-destructive-wipe
  Required boolean flag. Short flags such as --yes or --force are not aliases.

--acknowledge TEXT
  Must exactly match the acknowledgement text above after shell parsing.

--client-request-id ID
  Idempotency key recorded in every accepted node-local operation request.

--output json
  v0.1 status and plan output format. Other formats are unsupported.
```

Optional common flags:

```text
--plan
  Validate targets, credentials, acknowledgement, and visible cluster state, then
  print the planned per-node actions without accepting node-local operations.

--timeout DURATION
  Upper bound for client-side waits. Timeout stops waiting but does not cancel an
  accepted node-local wipe operation.
```

Authentication and authorization:

- `katlctl` authenticates to each node-local `katlc` agent using the inventory
  access material for that node.
- `katlc` is the node-local authority for accepting and executing the wipe
  operation. It revalidates node identity, request identity, target disk
  identity, and operation locks before mutation.
- Kubernetes API access, when used, comes only from an explicit `--kubeconfig`
  flag. Node-local wipe must not depend on SSH or remote shell execution.

Plan and status output:

- `--plan` prints JSON with `command`, `targets`, `acknowledgementAccepted`,
  `kubernetesCleanup`, `nodeLocalOperations`, `wipedState`, `preservedState`,
  and `refusals`.
- Executing commands print JSON with one entry per selected node, including the
  accepted node-local operation ID, current phase, terminal status when known,
  and the evidence paths or redacted diagnostics available to the client.
- Aggregate success means every selected node reached installer-ready or clean
  generation 0 state. Partial success exits non-zero and reports each node's
  terminal or last observed status.

State wiped on selected nodes:

- Katl target disk partitions selected by the reinstall plan, including
  ESP/XBOOTLDR, root slots, writable state, selected Katl-owned extra data disks
  that explicitly opt into destructive wipe, generated confext output, selected
  sysext/confext links, payload caches, install and operation records, and boot
  entries.
- Kubernetes and kubelet local state on the selected target, including
  `/etc/kubernetes` backing state, `/var/lib/kubelet`, `/var/lib/etcd`,
  container runtime state, CNI state, bootstrap tokens, kubeconfigs, node object
  identity, machine ID, and SSH host keys.

State preserved:

- Off-node artifact repositories, operator workstations, external backups,
  external load balancer configuration, and non-target disks unless selected by a
  Katl extra-disk wipe plan.
- Kubernetes resources for workloads may remain in an external backup or GitOps
  source, but `katlctl wipe` does not preserve them from the discarded cluster.

Requests are refused before node-local mutation when:

- The acknowledgement flag or exact acknowledgement text is missing.
- Target selection is empty, duplicated, or ambiguous.
- The inventory lacks a node address, role, or usable node-local credential for a
  selected node.
- A selected node reports a different node identity or target disk stable ID
  than the inventory or stored install intent expects.
- A selected node already has an active Katl operation lock.
- `--plan` is set.

Failure behavior:

- After a node-local operation is accepted, `katlctl` may lose connectivity
  without changing the operation's authority. Operators must use status polling
  against node-local `katlc` to resume observation.
- If mutation may have started, retry is not automatic. A later command must use
  the same `--client-request-id` or an explicit recovery flow once one exists.
- Diagnostics must redact bearer tokens, kubeconfigs, bootstrap tokens,
  certificate private keys, and etcd secrets.

## `katlctl wipe node`

Command:

```text
katlctl wipe node --inventory PATH --node NAME --kubeconfig PATH \
  --confirm-destructive-wipe --acknowledge TEXT --client-request-id ID
```

Target selection:

- Exactly one `--node NAME` is required.
- The node name must exist in the inventory and resolve to one node-local katlc
  endpoint.

Graceful Kubernetes cleanup:

- `--kubeconfig` is required for execution. `--plan` may run without contacting
  the Kubernetes API only if it reports Kubernetes cleanup as `unknown`.
- `katlctl` first attempts to cordon the Kubernetes Node.
- It then attempts a bounded drain that evicts ordinary workload pods and ignores
  DaemonSet-managed pods. Mirror/static pods are not deleted through the API.
- It deletes the Kubernetes Node object after drain attempts complete or time
  out.
- For a control-plane node, it removes the matching stacked-etcd member when the
  remaining control plane has quorum. If quorum cannot be proven, the command is
  refused before node-local wipe unless the target set is the whole cluster,
  which belongs to `katlctl wipe cluster`.

Node-local wipe trigger:

- After graceful cleanup either succeeds or reaches a declared best-effort
  terminal result, `katlctl` submits a destructive-reset operation to the target
  node-local `katlc`.
- The node-local operation wipes selected target state and returns the machine to
  installer-ready media handoff or a clean generation 0 boot.
- The command waits for clean generation 0 evidence by default: generation 0
  selected and booted, no Kubernetes sysext selected, no kubeadm output, empty or
  absent kubelet/etcd state, and only new install/wipe operation history.

Result:

- Success leaves the wiped node ready for first install/bootstrap again and
  leaves the remaining cluster without that Kubernetes Node and, for a
  control-plane target, without that etcd member.
- The command does not automatically bootstrap the wiped node back into the
  cluster. Rejoin is a later explicit install/bootstrap action.

## `katlctl wipe cluster`

Command:

```text
katlctl wipe cluster --inventory PATH --all \
  --confirm-destructive-wipe --acknowledge TEXT --client-request-id ID
```

Target selection:

- `--all` selects every node in the inventory.
- `--node NAME` may be repeated to select a subset only for test and recovery
  workflows. A partial target set must be printed clearly as a partial cluster
  wipe and exits non-zero unless `--allow-partial-cluster` is also supplied.
- Empty target sets are refused.

Cluster identity:

- The command explicitly discards the Kubernetes cluster identity. It does not
  preserve or reattach the previous cluster CA, service account keys, bootstrap
  tokens, kubeconfigs, etcd member IDs, node object UIDs, CNI state, or Katl
  operation history.
- It does not require graceful Kubernetes API or etcd coordination before wiping
  selected nodes. If the API is reachable, diagnostics may report observed state,
  but API failure is not a refusal condition.

Node-local wipe trigger:

- `katlctl` submits destructive-reset operations to all selected nodes after
  validating inventory and acknowledgements.
- Ordering is best-effort parallel for worker nodes and conservative for control
  planes: requests may be submitted to all selected control planes without etcd
  quorum checks because the cluster identity is being discarded.
- The command waits until every reachable selected node reports installer-ready
  or clean generation 0. Unreachable accepted nodes are reported as unknown and
  make the aggregate command fail.

Result:

- Success leaves selected nodes ready for a fresh `katlctl cluster bootstrap`
  that creates a new cluster identity.
- The command never attempts to repair or merge the old cluster.

## VM Gates

Implementation of `katlctl wipe node` must add and pass:

```text
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios -run TestInstalledRuntimeTwoNodeWipeNodeBootstrapSmoke -count=1
```

The gate must start from a bootstrapped Kubernetes cluster with real Kubernetes
and etcd state, run `katlctl wipe node` against one node, prove Kubernetes Node
cleanup and etcd member cleanup when applicable, prove node-local stale state is
gone, then reinstall/bootstrap the node and prove remote kubectl/workload
health.

Implementation of `katlctl wipe cluster` must add and pass:

```text
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios -run TestInstalledRuntimeTwoNodeWipeClusterBootstrapSmoke -count=1
```

The gate must start from a bootstrapped Kubernetes cluster, run
`katlctl wipe cluster`, prove selected nodes return to installer-ready or clean
generation 0 without depending on graceful Kubernetes or etcd coordination, then
bootstrap a new usable cluster identity and prove remote kubectl/workload health.
