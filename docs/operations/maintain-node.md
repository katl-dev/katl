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
[reboot or shutdown](access.md#routine-host-management) when you are ready.
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
[Apply node configuration](configure-nodes.md). Kubelet owns the shutdown
budget and logind delay inhibitor; KatlOS provides the writable runtime file
that kubelet needs while keeping `/etc` immutable. Native priority-based
shutdown configuration is also available through the same kubelet input.

## Check storage teardown separately

Graceful pod termination does not guarantee that a CSI driver's staging mounts
or mapped devices have been released. A successful drain alone does not prove
that teardown is complete. Follow your storage driver's procedure and verify
the node's actual mounts and devices before maintenance.

Mounted Ceph RBD filesystems can still hang host shutdown if the driver,
backend, or network stops before teardown finishes. A longer kubelet budget
does not establish an ordering guarantee for those dependencies. Katl does not
force unmounts or unmap customer devices to work around that limitation.
