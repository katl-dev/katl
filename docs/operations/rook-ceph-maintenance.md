# Maintain nodes that use Rook/Ceph storage

This guidance applies to Rook/Ceph deployments on Linux Kubernetes nodes,
including KatlOS. Host maintenance must account for both application pods and
the network storage they use.

A mounted Ceph RBD filesystem can block shutdown if its storage backend,
driver, or network becomes unavailable before unmounting finishes. Pod
termination, CSI staging cleanup, device unmapping, and host service shutdown
are separate operations. Check the maintenance procedure for your deployed
Rook, Ceph, and Ceph-CSI versions. Rook documents cluster shutdown and restart
ordering in its [node maintenance guide](https://rook.io/docs/rook/latest/Upgrade/node-maintenance/).

## Before reboot, shutdown, or host upgrade

1. Check Ceph health and workload availability. Determine whether the node
   hosts storage clients, Ceph daemons, or both. Follow the application and
   storage maintenance procedures for that role.
2. Quiesce or move workloads as required by those procedures. A Kubernetes
   drain handles pod eviction. Verify that CSI has also completed volume
   teardown before stopping the dependencies needed to release those volumes.
3. Inspect the node's mounts and block devices. For example, run these
   read-only commands on the node and check for RBD devices and CSI mounts.

   ```sh
   findmnt -rn -o SOURCE,TARGET,FSTYPE
   lsblk -o NAME,TYPE,FSTYPE,MOUNTPOINTS
   ```

   Correlate remaining mounts and mappings with the volumes involved in the
   maintenance operation. Use Ceph-CSI and kubelet diagnostics to investigate
   teardown that has not completed. Ceph describes RBD mappings in the
   [RBD command reference](https://docs.ceph.com/en/latest/man/8/rbd/).
4. Perform the host operation after the required storage teardown completes.
   After the node returns, verify Ceph health, application access to volumes,
   and Kubernetes readiness before restoring scheduling.

## If shutdown stalls

Preserve the shutdown journal and console output. Check kubelet and Ceph-CSI
logs, remaining mounts and mappings, and connectivity to the Ceph cluster.
Follow the storage driver's recovery procedure for the observed state.

A longer kubelet shutdown budget gives pods more termination time. Storage
teardown still needs access to its dependencies until it completes. Keep CSI
volume cleanup under the driver's control when recovering a stalled operation.

For KatlOS workload maintenance and shutdown-budget configuration, follow
[Prepare a Kubernetes node for host maintenance](maintain-node.md). The
[host upgrade procedure](upgrade-host.md) covers the KatlOS lifecycle operation.
