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

Use the retained `ClusterConfig` as the normal input. `katlctl` compiles the
internal inventory itself. `--config-bundle` remains available for PXE/offline
material and `--inventory` for expert recovery tooling. Add `--context NAME`
when the enrolled workstation context should supply management addresses and
credentials independently of the source.

The required acknowledgement is intentionally exact:

```text
I understand this will remove KatlOS disk boot artifacts on the selected nodes so the next reboot must use installer media or PXE to reinstall with a new cluster identity.
```

## Plan a Whole-Cluster Wipe

```sh
katlctl cluster wipe \
  ./cluster.yaml \
  --plan \
  --all
```

Planning is non-mutating and does not require destructive acknowledgement.
Review every target, address, role, wiped surface, preserved surface, and
refusal.

Execute only when the cluster is intentionally being discarded:

```sh
katlctl cluster wipe ./cluster.yaml \
  --all \
  --confirm-destructive-wipe \
  --acknowledge 'I understand this will remove KatlOS disk boot artifacts on the selected nodes so the next reboot must use installer media or PXE to reinstall with a new cluster identity.'
```

The command follows every node-local destructive reset and reports each
terminal result.

Do not proceed to reinstall until every intended reset reports `terminal: true`
and `result: succeeded`. Treat `recoveryRequired: true` as a stop condition.

## Plan One Worker Replacement

Single-node wipe coordinates Kubernetes Node cleanup before the node-local reset:

```sh
katlctl cluster wipe node \
  ./cluster.yaml \
  --plan \
  --node worker-1
```

Execution additionally requires `--kubeconfig ./kubeconfig`. If Kubernetes
cleanup fails, Katl reports recovery required and refuses the node-local wipe.
Single control-plane wipe is refused because etcd membership coordination is
not implemented as a supported operation.

## Reinstall

After every selected wipe operation succeeds:

1. boot the verified installer ISO or PXE path;
2. apply the intended `ClusterConfig` source and node selection;
3. inspect the target disk again before authorizing installer wipe;
4. complete generation 0 handoff; and
5. treat the result as a new cluster identity unless a future supported
   recovery operation explicitly says otherwise.

Do not claim preserved `/var/lib/etcd`, kubelet state, or Katl operation records
as recovered merely because they remained on disk between reset and reinstall.
