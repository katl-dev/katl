# Prepare a Kubernetes node for host maintenance

You control workload maintenance before rebooting, shutting down, or upgrading
a KatlOS host. Katl's host lifecycle commands do not drain the node, require a
successful drain, or uncordon it afterward.

## Manage workload availability

Check cluster capacity, disruption budgets, application replication, and your
storage driver's maintenance procedure before taking a node offline. For an
ordinary Kubernetes drain, run:

```sh
kubectl drain cp-1 --ignore-daemonsets
```

A drain can be blocked by workload policy or local data. Resolve that condition
according to the application's maintenance procedure. Katl does not bypass
disruption budgets or retry evictions on your behalf. DaemonSets and static
control-plane pods remain on the node.

Run the intended [host upgrade](upgrade-host.md) or
[reboot or shutdown](access.md#reboot-or-shut-down-a-node) when you are ready.
After the node returns, verify workload and storage health before restoring
scheduling:

```sh
kubectl get nodes
kubectl uncordon cp-1
```

Katl does not perform that final uncordon. Its host health check accepts a
Ready node that remains cordoned.

## Configure native graceful shutdown

Kubelet can delay a host shutdown to terminate pods through
[Kubernetes' native graceful node shutdown handling](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/).
Choose budgets that fit your workloads; Katl
does not enable or size them automatically.

For example, save this native configuration as `kubelet.yaml` alongside your
`ClusterConfig`:

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
shutdownGracePeriod: 45s
shutdownGracePeriodCriticalPods: 15s
```

Reference it through the existing node-local kubelet input:

```yaml
spec:
  defaults:
    kubernetes:
      kubelet:
        configFile: kubelet.yaml
```

Merge this reference into your complete configuration, then follow
[Apply cluster configuration](configure-nodes.md). Kubelet owns the shutdown
budget and logind delay inhibitor; KatlOS provides the writable runtime file
that kubelet needs while keeping `/etc` immutable. Native priority-based
shutdown configuration is also available through the same kubelet input.

## Check storage teardown separately

Graceful pod termination does not guarantee that a CSI driver's staging mounts
or mapped devices have been released. A successful drain alone does not prove
that teardown is complete. Follow your storage driver's procedure and verify
the node's actual mounts and devices before maintenance.

## Rook-Ceph

A mounted Ceph RBD filesystem can block shutdown if its storage backend,
driver, or network goes away before the unmount finishes, because pod
termination, CSI staging cleanup, device unmapping, and host service shutdown
are separate steps. Follow the maintenance procedure for your deployed Rook,
Ceph, and Ceph-CSI versions; Rook documents shutdown and restart ordering in its
[node maintenance guide](https://rook.io/docs/rook/latest/Upgrade/node-maintenance/).

### Before reboot, shutdown, or host upgrade

1. Check Ceph health and workload availability, determine whether the node
   hosts storage clients, Ceph daemons, or both, and follow the application and
   storage maintenance procedures for that role.
2. Quiesce or move workloads as those procedures require. A drain evicts pods,
   but verify that CSI has also completed volume teardown before stopping the
   dependencies needed to release those volumes.
3. On the node, check for remaining RBD devices and CSI mounts with these
   read-only commands:

   ```sh
   findmnt -rn -o SOURCE,TARGET,FSTYPE
   lsblk -o NAME,TYPE,FSTYPE,MOUNTPOINTS
   ```

   Correlate what remains with the volumes involved in the maintenance, and use
   Ceph-CSI and kubelet diagnostics to investigate incomplete teardown. Ceph
   describes RBD mappings in the
   [RBD command reference](https://docs.ceph.com/en/latest/man/8/rbd/).
4. Perform the [host upgrade](upgrade-host.md), reboot, or shutdown only after
   the required storage teardown completes. After the node returns, verify Ceph
   health, application access to volumes, and Kubernetes readiness before
   restoring scheduling.

### If shutdown stalls

Preserve the shutdown journal and console output. Check kubelet and Ceph-CSI
logs, remaining mounts and mappings, and connectivity to the Ceph cluster, then
follow the storage driver's recovery procedure for the observed state. Leave
CSI volume cleanup under the driver's control: a longer kubelet
[shutdown budget](#configure-native-graceful-shutdown) gives pods more
termination time, but storage teardown still needs its dependencies until it
completes.
