# ADR-018: Native kubelet shutdown integration

Status: accepted.

Date: 2026-10-16.

## Decision

Let kubelet manage its native graceful node shutdown inhibitor. Preserve the
operator's native `KubeletConfiguration`, including disabled shutdown handling
and priority-based budgets. Katl does not choose workload shutdown budgets.

Kubelet writes `/etc/systemd/logind.conf.d/99-kubelet.conf` when its configured
shutdown budget exceeds logind's delay ceiling. On KatlOS, make that single file
a symlink to `/run/systemd/logind.conf.d/99-kubelet.conf`. Create the runtime
directory through tmpfiles and start logind before kubelet. Keep the enclosing
logind configuration directory immutable. Kubelet owns writing the derived
ceiling, reloading logind, and acquiring the delay inhibitor.

The runtime file is ephemeral. Kubelet reconstructs it from its active native
configuration after boot. Reducing a budget can leave a larger logind ceiling
until reboot; the ceiling bounds a delay lock rather than imposing a minimum
shutdown wait.

## Ownership

Operators own workload maintenance, including cordon, drain, disruption
budgets, and storage-specific teardown. Kubernetes and the CSI driver own pod
termination and volume teardown. Katl owns the host integration that lets those
components use native shutdown facilities.

Host reboot, shutdown, and upgrade neither orchestrate nor require a drain.
They do not change the node's scheduling state. A cordoned node can still pass
the host lifecycle's Kubernetes Ready check. These commands do not force
unmounts, unmap devices, select workloads, or wait on a Katl storage barrier.

## Alternatives

A fixed logind delay ceiling would duplicate workload policy and could be too
small for an operator's configuration. A Katl configuration parser and reload
controller would duplicate the policy and lifecycle already implemented by
kubelet. Making the whole logind configuration directory writable would expose
more immutable configuration than kubelet needs. Delegate one generated file
instead.

## Limits

An acquired inhibitor proves that kubelet participates in native shutdown. It
does not prove that every CSI `NodeUnstageVolume` call, staging mount, or device
cleanup finishes before the driver, backend, or network stops. Pod termination
and a successful workload drain are not universal proofs of completed storage
teardown. Keep Ceph RBD maintenance guidance explicit about this limitation;
do not turn it into Katl-owned workload orchestration.
