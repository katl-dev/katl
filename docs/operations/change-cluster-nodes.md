# Add, replace, or remove cluster nodes

Use this guide to join, replace, or remove one node while keeping a healthy
cluster in service. To reinstall nodes before Kubernetes bootstrap or discard
the whole cluster, use [Wipe and reinstall KatlOS](wipe-reinstall.md).

For a control plane managed outside Katl, use the
[external worker workflow](external-workers.md). The coordinator-based procedure
below applies to Katl-managed control planes.

Katl changes cluster membership only through explicit bootstrap, join, and wipe
operations. Editing `spec.nodes` by itself is not authority to drain, delete,
power off, or erase a machine.

Start with a healthy cluster and the complete `cluster.yaml`. Verify the API,
node, control-plane, etcd, CNI, and workload state before changing membership:

```sh
katlctl cluster status --config ./cluster.yaml
kubectl --kubeconfig ./kubeconfig get nodes -o wide
kubectl --kubeconfig ./kubeconfig get pods -A
katlctl cluster etcd members --config ./cluster.yaml
```

The etcd check is required for control-plane work and optional for a worker-only
cluster change.

## Add one node

Add one uniquely named node with its final role, stable disk selector,
management address, and SSH access to `cluster.yaml`. Validate the whole file:

```sh
katlctl config validate ./cluster.yaml
```

Install that node through the normal ISO or PXE flow and verify healthy
generation 0. Do not rerun cluster bootstrap on an existing cluster. Join the
one fresh node with:

```sh
katlctl node join worker-1 --config ./cluster.yaml
```

Katl selects a ready surviving control plane, creates short-lived kubeadm join
material, joins the named worker or control plane without rerunning
`kubeadm init`, and verifies its generation and Kubernetes services. A healthy
committed join does not reboot the node, while a pending trial generation is
rebooted and checked before success. Repeating a completed join is a no-op, so
retry the same command after an interruption.

Use `--coordinator cp-1` to choose a ready surviving control plane explicitly.
Only the joining node and coordinator need to be reachable. Apply later
configuration changes with `katlctl cluster apply --config ./cluster.yaml`;
`cluster apply` never joins nodes.

Verify the new Kubernetes Node, and for a control plane verify the new stacked
etcd member and local static pods. Your CNI remains responsible for scheduling
its node components and making the Node Ready.

## Wipe one node

Replacement and permanent removal both start by wiping the node while it is
still listed in `cluster.yaml` under its original name. The wipe removes
Kubernetes membership first, then erases KatlOS boot artifacts and powers the
machine off, so the next boot must use installer media or PXE. Installer
formatting later erases the selected system disk. This is not a data backup or
rollback.

For an enrolled worker, Katl cordons and drains the node, then deletes its
Kubernetes Node before the node-local reset. For an enrolled control plane,
Katl drains the node, validates its exact stacked-etcd member against a healthy
surviving control plane, proves that removal preserves quorum, removes and
verifies the member, then deletes the Kubernetes Node. A control-plane node that
has not joined Kubernetes can be reset without a kubeconfig.

The drain runs `kubectl drain --ignore-daemonsets --delete-emptydir-data
--force`, bounded by the command's `--timeout`. It deletes emptyDir data and
force-deletes pods that no controller manages, which are not recreated. Drain
failures do not stop the wipe; Katl reports them as best-effort diagnostics.
Move workloads you care about first with
[workload maintenance](maintain-node.md).

Plan the wipe:

```sh
katlctl node wipe worker-1 --config ./cluster.yaml \
  --kubeconfig ./kubeconfig --plan
```

For an enrolled control plane, the plan also reports the coordinator, the
observed etcd member ID, and quorum safety. Katl refuses a missing or ambiguous
member, a coordinator that would remove itself, changed cluster or member
identity, or insufficient healthy quorum. Read every refusal and target, then
run the same command without `--plan`:

```sh
katlctl node wipe worker-1 --config ./cluster.yaml \
  --kubeconfig ./kubeconfig
```

Execution requires `--kubeconfig` for an enrolled node so Katl can remove the
Kubernetes Node first. If Kubernetes cleanup fails, Katl reports recovery
required and refuses the node-local wipe.

## Replace one node without renaming it

1. [Wipe the node](#wipe-one-node) and wait for the operation to succeed and
   power the machine off.
2. Boot the verified installer ISO or PXE path and apply `cluster.yaml` for the
   same node name and role.
3. Inspect the target disk again before authorizing the installer wipe.
4. Wait for generation 0.
5. Join the replacement:

   ```sh
   katlctl node join worker-1 --config ./cluster.yaml
   ```

Katl joins the named replacement without rerunning `kubeadm init`. Confirm etcd
membership, Node readiness after CNI convergence, and workload behavior before
replacing another machine.

## Remove one node permanently

Do not delete the `cluster.yaml` entry first. Keep the node listed while
planning and executing its Kubernetes- and etcd-aware [wipe](#wipe-one-node).

After the operation succeeds and the node is powered off, verify the Kubernetes
Node is gone and, for a control plane, that etcd is healthy without the member.
Only then remove the entry from `cluster.yaml` and run `katlctl cluster apply
--config ./cluster.yaml`.

Omitting a node merely stops `cluster apply` from targeting it. Katl deliberately
does not infer that omission means removal.

## Recover a failed control plane

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
with `node join`. Loss of etcd quorum and snapshot-based disaster recovery
remain outside the supported workflow.

## Change a node's address

A node has two addresses in `cluster.yaml`, and neither field changes the IP
address that the node configures. The node's own addressing comes from DHCP or
its [networkd host configuration](../reference/cluster-config.md#networkd-files).

| Change | Supported path |
| --- | --- |
| `management.address` | This field only changes where `katlctl` connects. After the node answers at the new address, update `cluster.yaml` and, if you use a saved context, [rebind it](access.md#use-a-context) with `katlctl context rebind`. |
| `kubernetes.address` | `cluster apply` refuses the change. [Wipe the node](#wipe-one-node), reinstall it with the new address, and join it with `katlctl node join`. |
| A worker's IP address | Re-addressing a worker in place is not yet a supported single workflow. Wipe, reinstall, and join it. |
| A control plane's IP address | Not supported. Katl cannot change a control plane's advertise address, etcd peer URLs, serving certificate SANs, or a control-plane endpoint derived from its address. |

When `spec.controlPlaneEndpoint` is unset, Katl derives the Kubernetes API
endpoint from the first control plane's `management.address` and port 6443, so
that node's address becomes part of the cluster. If node addresses might
change, set a stable VIP or DNS name as
[`spec.controlPlaneEndpoint`](../reference/cluster-config.md#kubernetes-api-endpoint)
before bootstrap.

## Refused transitions

Katl refuses enrolled-node rename and role change during `cluster apply`.
Changing a control plane into a worker, or the reverse, requires explicit wipe,
reinstall under the intended role, and join. Removing one node and adding a
different name is never inferred to be a rename.

See the [node lifecycle matrix](configure-nodes.md#node-lifecycle-matrix) for
the behavior of ordinary field changes and management-address changes.
