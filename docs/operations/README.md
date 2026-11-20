# Operate and recover KatlOS

Use these guides when KatlOS is installed or when an installation needs
attention. To set up a new lab from the release ISO, start with
[Build your first KatlOS cluster](../getting-started.md).

## Bring installed nodes into service

- [Access installed nodes](access.md) to check a fresh install, connect through
  SSH, and manage access from your workstation.
- [Bootstrap Kubernetes](bootstrap-kubernetes.md) when all the intended hosts
  are installed and reachable.
- [Preserve Kubernetes identity](kubernetes-identity.md) before bootstrap if
  you need to keep the cluster's trust keys across a rebuild.
- [Run Cilium on KatlOS](cilium.md) to connect workloads, starting with the
  host settings you need before installation.

## Change or maintain a running cluster

- [Apply cluster configuration](configure-nodes.md) to change supported host
  and kubeadm settings. Use the [configuration reference](../reference/cluster-config.md)
  when you need to look up a field.
- [Change cluster membership](change-cluster-nodes.md) to add, replace, or
  remove a node from a healthy cluster.
- [Prepare a node for maintenance](maintain-node.md) to protect workloads
  before a reboot or shutdown.
- [Maintain a node that uses Rook/Ceph](rook-ceph-maintenance.md) to check
  storage teardown before taking it offline.
- [Upgrade a KatlOS host](upgrade-host.md) to change the host OS version.
- [Upgrade Kubernetes](upgrade-kubernetes.md) to change the Kubernetes version
  across the cluster.

## Diagnose and recover

- [Troubleshoot KatlOS](troubleshoot.md) to identify what failed and what you
  can safely retry.
- [Wipe and reinstall](wipe-reinstall.md) to plan a one-node replacement or
  deliberately start over.
- [Understand host rollback](../concepts/ownership.md#persistent-state-and-rollback)
  to see what a previous generation can restore.

## Keep the files you need for recovery

Keep `cluster.yaml`, its referenced files, and the release tag. Store copies
of the kubeconfig, any Kubernetes identity and management secrets, and your
independent etcd, workload, and data backups away from the nodes. A KatlOS
host rollback restores a host generation. Use your backups to recover
cluster state and data.

If an operation fails after it begins changing a node, keep its result and
check the node's recorded operation state before retrying. See
[operation semantics](../reference/katlctl.md#operation-semantics) for the
difference between an accepted operation, a completed operation, and a
generation that has passed its boot checks.
