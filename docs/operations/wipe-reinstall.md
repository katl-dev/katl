# Wipe and Reinstall KatlOS

This is destructive cluster-discard or node-replacement preparation. It is not
backup, etcd recovery, same-cluster repair, or rollback.

The wipe operation removes KatlOS disk boot artifacts so the next boot must use
installer media or PXE. Existing on-disk Kubernetes and Katl state remain until
the installer subsequently wipes the selected disk. Keep installer media ready
before accepting the operation.

## Before Planning

- preserve any required external backups and recovery material;
- confirm which cluster identity is being discarded;
- save the inventory, release assets, config source, any PXE bundle,
  kubeconfig, and operation evidence;
- ensure all selected nodes can boot the intended installer path; and
- stop if a control-plane or etcd member is expected to remain part of the same
  cluster.

The retained `ClusterConfig` or PXE/offline config bundle is the normal topology
source. A saved workstation context is optional shorthand for repeated work;
the lower-level inventory input is reserved for recovery tooling.

## Plan a Whole-Cluster Wipe

```sh
katlctl cluster wipe \
  --config ./cluster.yaml \
  --plan \
  --all
```

Planning is non-mutating. Review every target, address, role, wiped surface,
preserved surface, and refusal.

Execute only when the cluster is intentionally being discarded:

```sh
katlctl cluster wipe --config ./cluster.yaml --all
```

The command follows every node-local destructive reset, reports each terminal
result, and leaves every successfully wiped node powered off.

Do not proceed to reinstall until every intended reset reports `terminal: true`
and `result: succeeded`, then confirm the nodes are off. Treat
`recoveryRequired: true` as a stop condition.

## Plan One Node Replacement

For an enrolled worker, single-node wipe coordinates Kubernetes Node cleanup
before the node-local reset:

```sh
katlctl node wipe worker-1 --config ./cluster.yaml --plan
```

After saving it, the workstation context supplies topology, so
the source can be omitted:

```sh
katlctl node wipe worker-1 --config ./cluster.yaml --kubeconfig ./kubeconfig
```

Execution requires `--kubeconfig` for an enrolled node so Katl can remove the
Kubernetes Node first. If Kubernetes cleanup fails, Katl reports recovery
required and refuses the node-local wipe.

A control-plane node that has not joined Kubernetes can be reset without a
kubeconfig. For an enrolled control plane, Katl first drains the node, validates
its exact stacked-etcd member against a healthy surviving control plane, proves
that removal preserves quorum, removes and verifies the member, then deletes
the Kubernetes Node before the node-local reset:

```sh
katlctl node wipe cp-3 --config ./cluster.yaml --plan
katlctl node wipe cp-3 --config ./cluster.yaml \
  --kubeconfig ./kubeconfig
```

The plan reports the coordinator and observed member ID. Katl refuses a missing
or ambiguous member, a coordinator that would remove itself, changed cluster or
member identity, or insufficient healthy quorum.

If the node failed before a clean wipe, inspect membership from a survivor and
explicitly confirm the stale member ID:

```sh
katlctl cluster etcd members --config ./cluster.yaml
katlctl cluster etcd remove cp-3 --member-id MEMBER_ID \
  --config ./cluster.yaml
```

This recovery command only removes etcd membership. Delete a remaining
Kubernetes Node object if necessary, then reinstall the failed machine.

## Reinstall

After every selected wipe operation succeeds and powers off its node:

1. select the verified installer ISO or PXE path and start the node;
2. apply the intended `ClusterConfig` source and node selection;
3. inspect the target disk again before authorizing installer wipe;
4. wait for generation 0 handoff; and
5. run `katlctl cluster apply --config ./cluster.yaml`.

For a single replacement in a healthy existing cluster, `cluster apply`
recognizes the one fresh node, uses a ready surviving control plane to mint
short-lived join material, joins the replacement without rerunning `kubeadm
init`, and then reconciles the complete cluster config online. Repeating the
unchanged apply after success is a no-op. Do not run `cluster bootstrap` again
on the provisioned cluster.
