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
- save the inventory, release assets, config source, bundle, kubeconfig, and
  operation evidence;
- ensure all selected nodes can boot the intended installer path; and
- stop if a control-plane or etcd member is expected to remain part of the same
  cluster.

The current wipe commands accept a low-level bootstrap inventory, not the
verified `.katlcfg` directly. This is an acknowledged alpha UX gap. The
inventory must describe the same installed nodes, addresses, roles, per-node
token references, Kubernetes version, and digest-pinned bundle recorded in the
compiled cluster intent. Do not use an older inventory merely because its nodes
are reachable. A two-node shape is:

```yaml
controlPlaneEndpoint: api.katl.test:6443
kubernetesVersion: v1.36.0
kubernetesBundle: ghcr.io/katl-dev/kubernetes:<version>@sha256:<digest>
nodes:
  - name: cp-1
    address: 192.0.2.11
    systemRole: control-plane
    access:
      method: agent
      credentialRef: file:/absolute/path/to/tokens/cp-1.token
    kubeadmConfig:
      ref: control-plane
      path: /etc/katl/kubeadm/control-plane/config.yaml
      intent: control-plane
    kubernetesVersion: v1.36.0
  - name: worker-1
    address: 192.0.2.21
    systemRole: worker
    access:
      method: agent
      credentialRef: file:/absolute/path/to/tokens/worker-1.token
    kubeadmConfig:
      ref: worker
      path: /etc/katl/kubeadm/worker/config.yaml
      intent: worker
    kubernetesVersion: v1.36.0
```

Compare it with the retained config source, bundle report, and live cluster
before planning. Native config-bundle input is tracked for a future release.

The required acknowledgement is intentionally exact:

```text
I understand this will remove KatlOS disk boot artifacts on the selected nodes so the next reboot must use installer media or PXE to reinstall with a new cluster identity.
```

## Plan a Whole-Cluster Wipe

```sh
katlctl cluster wipe \
  --plan \
  --inventory ./cluster-inventory.yaml \
  --all \
  --confirm-destructive-wipe \
  --acknowledge 'I understand this will remove KatlOS disk boot artifacts on the selected nodes so the next reboot must use installer media or PXE to reinstall with a new cluster identity.' \
  --client-request-id discard-katl-lab-1
```

Even a plan requires the acknowledgement so automation cannot casually turn a
review command into a destructive one. Review every target, address, role,
wiped surface, preserved surface, and refusal.

Run the identical command without `--plan` only when the cluster is intentionally
being discarded. The command submits node-local destructive-reset operations
and reports their operation IDs and terminal results.

## Plan One Worker Replacement

Single-node wipe coordinates Kubernetes Node cleanup before the node-local reset:

```sh
katlctl cluster wipe node \
  --plan \
  --inventory ./cluster-inventory.yaml \
  --node worker-1 \
  --confirm-destructive-wipe \
  --acknowledge 'I understand this will remove KatlOS disk boot artifacts on the selected nodes so the next reboot must use installer media or PXE to reinstall with a new cluster identity.' \
  --client-request-id replace-worker-1-1
```

Execution additionally requires `--kubeconfig ./kubeconfig`. If Kubernetes
cleanup fails, Katl reports recovery required and refuses the node-local wipe.
Single control-plane wipe is refused because etcd membership coordination is
not implemented as a supported operation.

## Reinstall

After every selected wipe operation succeeds:

1. boot the verified installer ISO or PXE path;
2. submit the intended config bundle and node selection;
3. inspect the target disk again before authorizing installer wipe;
4. complete generation 0 handoff; and
5. treat the result as a new cluster identity unless a future supported
   recovery operation explicitly says otherwise.

Do not claim preserved `/var/lib/etcd`, kubelet state, or Katl operation records
as recovered merely because they remained on disk between reset and reinstall.
