# Add, Replace, or Remove Cluster Nodes

Katl changes cluster membership only through explicit install, apply, and wipe
operations. Editing `spec.nodes` by itself is not authority to drain, delete,
power off, or erase a machine.

Start with a healthy cluster and the complete retained `cluster.yaml`. Verify
the API, node, control-plane, etcd, CNI, and workload state before changing
membership:

```sh
katlctl cluster status --config ./cluster.yaml
kubectl --kubeconfig ./kubeconfig get nodes -o wide
kubectl --kubeconfig ./kubeconfig get pods -A
katlctl cluster etcd members --config ./cluster.yaml
```

The etcd check is required for control-plane work and optional for a worker-only
cluster change.

## Add One Node

Add one uniquely named node with its final role, stable disk selector,
management address, and SSH access to `cluster.yaml`. Validate the whole source:

```sh
katlctl config validate ./cluster.yaml
```

Install that node through the normal ISO or PXE flow and verify healthy
generation 0. Do not rerun cluster bootstrap on an existing cluster. Join the
one fresh node with:

```sh
katlctl cluster apply --config ./cluster.yaml
```

Katl selects a ready surviving control plane, creates short-lived kubeadm join
material, joins the fresh worker or control plane, trial-boots its Kubernetes
generation, and then reconciles supported configuration. Repeating the
unchanged apply is a no-op.

Verify the new Kubernetes Node, and for a control plane verify the new stacked
etcd member and local static pods. Your CNI remains responsible for scheduling
its node components and making the Node Ready.

## Replace One Node Without Renaming It

While the old node is still listed under its original name, plan its removal:

```sh
katlctl node wipe worker-1 --config ./cluster.yaml \
  --kubeconfig ./kubeconfig --plan
```

For an enrolled control plane, the plan also identifies a healthy coordinator,
the exact etcd member, and quorum safety. Read every refusal and target before
executing the same command without `--plan`.

The wipe workflow removes Kubernetes membership first, then erases KatlOS boot
artifacts and powers the machine off. Installer formatting later erases the
selected system disk. This is not a data backup or rollback.

Reinstall the replacement under the same node name and role, verify generation
0, and run:

```sh
katlctl cluster apply --config ./cluster.yaml
```

Katl treats it as one fresh replacement and joins it without rerunning
`kubeadm init`. Confirm etcd membership, Node readiness after CNI convergence,
and workload behavior before replacing another machine.

## Remove One Node Permanently

Do not delete the config entry first. Keep the node listed while planning and
executing its Kubernetes/etcd-aware wipe:

```sh
katlctl node wipe worker-1 --config ./cluster.yaml \
  --kubeconfig ./kubeconfig --plan
katlctl node wipe worker-1 --config ./cluster.yaml \
  --kubeconfig ./kubeconfig
```

After the operation succeeds and the node is powered off, verify the Kubernetes
Node is gone and, for a control plane, that etcd is healthy without the member.
Only then remove the entry from `cluster.yaml` and apply the retained desired
state.

Omitting a node merely stops `cluster apply` from targeting it. Katl deliberately
does not infer that omission means removal.

## Recover a Failed Control Plane

If a failed control plane cannot complete a coordinated wipe, inspect membership
through a healthy survivor:

```sh
katlctl cluster etcd members --config ./cluster.yaml
```

When the failed machine's stale member is unambiguous and removal preserves
quorum, explicitly supply the observed hexadecimal member ID:

```sh
katlctl cluster etcd remove cp-3 --member-id MEMBER_ID \
  --config ./cluster.yaml
```

This command removes only stacked-etcd membership. Delete any remaining
Kubernetes Node through the Kubernetes API, then reinstall and join the machine
with `cluster apply`. Loss of etcd quorum and snapshot-based disaster recovery
remain outside the supported beta workflow.

## Refused Transitions

Katl refuses enrolled-node rename and role change during `cluster apply`.
Changing a control plane into a worker, or the reverse, requires explicit wipe,
reinstall under the intended role, and join. Removing one node and adding a
different name is never inferred to be a rename.

See the [node lifecycle matrix](configure-nodes.md#node-lifecycle-matrix) for
the behavior of ordinary field changes and management-address changes, and the
[wipe runbook](wipe-reinstall.md) for complete destructive boundaries.
