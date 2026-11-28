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

Boot the new machine from the installer ISO or PXE so that it waits for
configuration, then add it to `cluster.yaml` from its installer:

```sh
katlctl config add-node ./cluster.yaml --installer 192.0.2.22
```

The command reads the installer's address and its selectable system disk and
appends a worker entry named after the next free `worker-N`. Pass `--name` to
choose the name and `--role control-plane` to add a control plane. When the
installer reports more than one selectable disk, choose one with
`--disk /dev/disk/by-id/...`. The entry inherits `spec.defaults`, such as SSH
keys, and the rest of `cluster.yaml`, including comments, is left as written.

The command validates the result as `katlctl config validate` does. It leaves
`cluster.yaml` unchanged and explains the refusal when the result would be
invalid, the name is taken, or the address is already listed under another
name or role. Running it again for an installer that is already listed reports
the entry unchanged. Add per-node settings, such as
[networkd files](../reference/cluster-config.md#networkd-files), by editing
the entry, then run `katlctl config validate ./cluster.yaml`.

A cluster with more than one control plane needs `spec.controlPlaneEndpoint`.
If the cluster was bootstrapped with one control plane and without that field,
its API endpoint uses port 6443 and that node's `kubernetes.address`, falling
back to the `management.address` host if `kubernetes.address` is unset. Set
`spec.controlPlaneEndpoint.host` to the host the cluster already uses before
adding a control plane. Apply the updated configuration to the existing nodes.

```sh
katlctl cluster apply --config ./cluster.yaml --plan
katlctl cluster apply --config ./cluster.yaml
```

Naming the address the cluster already uses does not change the endpoint, so
apply updates only node-local API access. Any other host or port is refused
after bootstrap. The endpoint stays tied to that node's address; see
[Change a node's address](#change-a-nodes-address).

Install the node and wait for healthy generation 0. Do not rerun cluster
bootstrap on an existing cluster. Join the one fresh node with:

```sh
katlctl install apply --config ./cluster.yaml --node worker-1
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
| `management.address` | This field changes where `katlctl` connects and, on a control plane, where its Kubernetes API proxy listens for `katlctl`. When `kubernetes.address` is set, nodes never use the management address to reach each other; without it, the management address is also the node's Kubernetes address. After the node answers at the new address, update `cluster.yaml` and, if you use a saved context, [rebind it](access.md#use-a-context) with `katlctl context rebind`. On a control plane, `cluster apply` then moves the proxy listener by restarting the proxy; if your kubeconfig used the proxy at the old address, run `katlctl cluster kubeconfig ./kubeconfig --config ./cluster.yaml --force` to select a reachable endpoint again. |
| `kubernetes.address` | If you explicitly specify the same Kubernetes IP the node already uses, `cluster apply` reports the node unchanged. To change the IP, [wipe the node](#wipe-one-node), reinstall it with the new address, and join it with `katlctl node join`. |
| A worker's IP address | Change its network configuration and `management.address` together, then [re-address the worker](#re-address-a-worker). |
| A control plane's IP address | Not supported. Katl cannot change a control plane's advertise address, etcd peer URLs, serving certificate SANs, or a control-plane endpoint derived from its address. `cluster apply --endpoint` refuses the move after the node answers as itself at the address given; at an address where it does not answer, apply asks you to check the address instead. |

When `spec.controlPlaneEndpoint` is unset, Katl derives the Kubernetes API
endpoint from the first control plane's `management.address` and port 6443, so
that node's address becomes part of the cluster. If node addresses might
change, set a stable VIP or DNS name as
[`spec.controlPlaneEndpoint`](../reference/cluster-config.md#kubernetes-api-endpoint)
before bootstrap.

### Re-address a worker

Use this procedure to move a joined worker to a new IP address. The worker keeps
its name, machine, enrollment, and Kubernetes membership. Katl applies the
change at the next boot and never reloads the network while connected, so the
management connection is not cut partway through an operation. `cluster apply`
first checks that the node answers as itself at the address passed with
`--endpoint`; only then does it compare that address's host with
`management.address`, so a mistyped address is never treated as a move and a
different port on the same host is not one. After the node answers, Katl
refuses the move for a control plane, and for a worker whose
`nodes[].kubernetes.address` fixes kubelet's node IP. For those nodes, [wipe the node](#wipe-one-node),
reinstall it with the new addresses, and join it.

In this example `worker-1` moves from `192.0.2.31` to `192.0.2.41`.

1. In `cluster.yaml`, change the worker's networkd configuration and set its
   `management.address` to the new address in the same edit:

   ```yaml
   - name: worker-1
     management:
       address: 192.0.2.41
     hostConfiguration:
       fileSets:
         management-network:
           files:
             - path: /etc/systemd/network/10-worker-1.network
               source: host-files/10-worker-1.network
   ```

   Change the `Address=` line in `host-files/10-worker-1.network` to
   `192.0.2.41/24`. If the new address comes from a changed DHCP reservation
   instead, change only `management.address`.

2. Apply the configuration to the worker. Pass the address it answers at now
   with `--endpoint`:

   ```sh
   katlctl cluster apply --config ./cluster.yaml --node worker-1 --endpoint 192.0.2.31
   ```

   Apply connects at `192.0.2.31` and stages the network change for the next
   boot, skipping the Kubernetes component phases, then prints the reboot
   command. A changed DHCP reservation stages nothing; apply runs the usual
   component phases at the current address and still prints the reboot
   command.
   `--endpoint` requires exactly one `--node`, and `--mode live` is refused.
   If you omit `--endpoint`, apply cannot reach the worker at its new address
   and prints this command, with the current address filled in when a saved
   context records it.

   A cluster reached through a saved context or an mTLS `.katlcfg` bundle uses
   the saved address instead. If you omit `--endpoint` there, apply still
   connects at the saved address and prints a warning that names both addresses
   and these steps, so the move is not mistaken for a finished change.

3. Reboot the worker:

   ```sh
   katlctl node reboot worker-1 --config ./cluster.yaml --endpoint 192.0.2.31
   ```

   The command schedules the reboot at `192.0.2.31` and waits for the worker
   at both addresses. It accepts only the same installation after a new boot,
   and it requires the staged generation to pass boot health. If the
   Kubernetes Node was Ready before the reboot, it must return Ready. On
   success it prints `worker-1 rebooted successfully at 192.0.2.41; health OK`.

4. If you use a saved context, rebind it to the new address first, because
   commands that reach nodes through it still use the previous address:

   ```sh
   katlctl context rebind --node worker-1 --endpoint 192.0.2.41
   ```

   Then rerun apply without `--endpoint`. This finishes any Kubernetes
   configuration at the new address:

   ```sh
   katlctl cluster apply --config ./cluster.yaml --node worker-1
   ```

5. Check the Node's address. Without `kubernetes.address`, kubelet detects the
   node's address when it starts:

   ```sh
   kubectl get node worker-1 -o wide
   ```

6. Check that your CNI has picked up the new address. A short reboot can leave
   the Node Ready while the CNI agent on the worker is still restarting, so
   wait for the agent and the worker's pods before moving workloads back. With
   Cilium's default VXLAN tunnelling, for example, `cilium status --wait`
   reports every agent ready and `cilium-health status` in another node's agent
   lists the worker at its new address. No Cilium-specific step is needed.

If the reboot does not finish at the new address, use the following table:

| Result | Meaning and recovery |
| --- | --- |
| The node `still answers at its previous address` | The new network configuration did not take effect. Boot health may have rejected the generation, or DHCP may still assign the old address. Correct the network files and repeat from step 2, or restore `management.address` to the old address. |
| The address `answered as a different KatlOS installation` | Another host uses the new address. Choose a free address and repeat from step 1. |
| The node answers at neither address | Boot health checks only that the required links come online and are routable, so a wrong but routable address can pass and become the default. Use the [local console](access.md#use-the-local-console) to inspect the network, or [select the previous generation](../concepts/generations.md#staged-trial-and-committed) from the boot menu. Then correct `cluster.yaml`. |
| Apply warns that a node `is configured at management.address` another address than the saved context reaches it at | The configuration names a new address, but the saved context still reaches the node at its previous one. To complete a planned move, stage it and reboot with `--endpoint` at the previous address (steps 2 and 3). If the node already answers at the new address, run `katlctl context rebind` (step 4). The warning does not appear after you rebind without editing `cluster.yaml`. If it appears because `cluster.yaml` still names an address the node has since left, update `management.address` and run `katlctl context save --config ./cluster.yaml`. |
| Apply with `--endpoint` says the node `did not answer at` the address, or that the endpoint `answered as node` another node or as `a different KatlOS installation` | The address passed with `--endpoint` does not reach this node, so apply decided nothing about a move and changed no node. Check the address and rerun. If the error says the saved context and `cluster.yaml` both place the node at its configured address, omit `--endpoint`. |
| A rerun of step 2 or 3 cannot reach the worker at the address passed with `--endpoint` | The worker may already answer at its new `management.address`, for example after an interrupted reboot wait. The error names that address. Continue with step 4. |

## Refused transitions

Katl refuses enrolled-node rename and role change during `cluster apply`.
Changing a control plane into a worker, or the reverse, requires explicit wipe,
reinstall under the intended role, and join. Removing one node and adding a
different name is never inferred to be a rename.

See the [node lifecycle matrix](configure-nodes.md#node-lifecycle-matrix) for
the behavior of ordinary field changes and management-address changes.
