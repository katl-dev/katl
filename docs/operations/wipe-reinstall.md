# Wipe and reinstall KatlOS

Use this guide to reinstall nodes before Kubernetes bootstrap or to
deliberately discard a whole cluster. It is not backup, etcd recovery,
same-cluster repair, or rollback.

| Situation | Use |
| --- | --- |
| Before Kubernetes bootstrap | [Reinstall from the installer](#reinstall-from-the-installer) with `katlctl install apply`. |
| One node in a running cluster | [Change cluster membership](change-cluster-nodes.md), which wipes, reinstalls, and rejoins the node. |
| The whole cluster | [Plan a whole-cluster wipe](#plan-a-whole-cluster-wipe) with `katlctl cluster wipe`, then [reinstall](#reinstall). |

The wipe operation removes KatlOS disk boot artifacts so the next boot must use
installer media or PXE. Existing on-disk Kubernetes and Katl state remain until
the installer subsequently wipes the selected disk. Keep installer media ready
before accepting the operation.

## Reinstall from the installer

Before Kubernetes bootstrap, you can boot PXE or installer media and run:

```sh
katlctl install apply --config ./cluster.yaml --node NODE
```

This explicitly replaces the selected system disk without requiring the
installed agent or a separate wipe operation. Data-volume `wipe` settings still
apply. A PXE auto-install profile also authorizes system-disk replacement unless
its optional `katl.halt-if-installed=1` guard refuses it.

For nodes already in a cluster, use the membership-aware
[node wipe](change-cluster-nodes.md#wipe-one-node) before reinstalling.

## Before planning

- Preserve any required external backups and recovery material.
- Confirm which cluster identity is being discarded.
- Save `cluster.yaml`, release assets, any PXE configuration bundle, the
  kubeconfig, and the operation results.
- Ensure all selected nodes can boot the intended installer path.
- Stop if a control-plane or etcd member is expected to remain part of the same
  cluster.

`cluster.yaml` or the PXE/offline configuration bundle is the normal topology
input. A saved context is the workstation record of the installed nodes'
identities and addresses, not a second topology source; the hidden
`--inventory` input is reserved for recovery tooling.

## Plan a whole-cluster wipe

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

## Reinstall

After every selected wipe operation succeeds and powers off its node:

1. Select the verified installer ISO or PXE path and start the node.
2. Apply `cluster.yaml` and the intended node selection.
3. Inspect the target disk again before authorizing installer wipe.
4. Wait for the installed node to boot
   [generation 0](access.md#confirm-generation-0).
5. Save the reinstalled nodes with
   `katlctl context save --config ./cluster.yaml`; bootstrap refuses nodes
   that have not been saved on this workstation.
6. Run `katlctl cluster bootstrap --config ./cluster.yaml` for the newly
   installed cluster. A replacement in an existing cluster instead joins
   through [`katlctl node join`](change-cluster-nodes.md#replace-one-node-without-renaming-it).

Do not run `cluster bootstrap` again on a cluster that is already provisioned.

## Boot menu entries

Firmware selects disk boot versus PXE. Installed entries are named
`KatlOS VERSION (Sep 20 10:41:11)`, with the configuration creation time in UTC
to distinguish configurations of the same OS version. The
[generation ID](../concepts/generations.md#generation-ids) shown by
`katlctl node generations list` remains in entry filenames and kernel
arguments, not menu labels. Existing entries keep the names written by their
original release, such as `Katl 0` or a generation ID. Selecting a
configuration boots the installed OS.

New installations and upgrades keep their runtime images outside automatic
UKI discovery, so each generated entry includes its required root and generation
arguments. Existing generations retain their original image paths for rollback;
an older installation can therefore still show its legacy raw-runtime entry
until it is reinstalled. Select the explicitly named configuration entry.
