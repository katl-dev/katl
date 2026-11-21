# KatlOS for Talos users

KatlOS and Talos Linux both install an immutable, API-managed node OS for
Kubernetes, so most Talos workflows have a direct KatlOS counterpart. The main
differences follow from KatlOS being a systemd-native Linux: kubeadm builds the
cluster, systemd and systemd-networkd configuration is native rather than
translated, and SSH is available alongside the management API, which the
`katlc` management agent serves on each node. Use these tables to find the
KatlOS equivalent of a familiar Talos concept or command.

## Install and configure

| Talos | KatlOS | Key difference |
| --- | --- | --- |
| Maintenance mode waiting for a machine config | The live [installer](../installing.md) waiting for configuration on port 8080 | The installer is a separate live ISO or PXE environment. It accepts unauthenticated handoff requests, so keep it on a trusted provisioning network. |
| `talosctl apply-config --insecure` | `katlctl install apply --config ./cluster.yaml --node cp-1` | Install writes the system disk and generation 0 only; Kubernetes is set up by a later, explicit bootstrap. |
| `talosctl gen config` role configs (`controlplane.yaml`, `worker.yaml`) plus per-node patches | One `ClusterConfig` (`cluster.yaml`) for all nodes, with `spec.defaults` and per-node overrides | `katlctl config resolve ./cluster.yaml --node cp-1` shows the effective result for one node. |
| `talosctl gen secrets` | [Kubernetes identity](../operations/kubernetes-identity.md) from `katlctl kubernetes identity create`, plus [management secrets](../operations/management-auth.md#durable-cluster-secrets) only for mTLS | The Kubernetes identity holds the shared kubeadm CAs and service-account keys and is optional. Management secrets are separate and exist only when you choose mTLS. |
| `talosctl apply-config` on a running node | `katlctl cluster apply --config ./cluster.yaml` | Apply changes live where possible and stages a next-boot generation otherwise; it never reboots, joins nodes, or upgrades Kubernetes. |
| `--mode auto` | `--mode auto` (the default) | Talos reboots when a change requires it; Katl stages the change and reports that the node needs a reboot. |
| `--mode no-reboot` | `--mode live` | Both refuse changes that need a reboot. |
| `--mode staged` | `--mode next-boot` | Host changes take effect on the next boot. |
| UserVolumeConfig | [`storage.volumes`](../reference/cluster-config.md#data-volumes) | Each volume uses one whole disk or one existing partition, mounted at `/var/mnt/<name>`. Removing a volume never erases it. |
| Image Factory schematic with system extensions | [`systemExtensions`](../reference/cluster-config.md#system-extensions) in `ClusterConfig` | Extensions are native sysexts selected per node by OCI reference and changed by apply (next boot) or host upgrade, not by building a new installer image. |
| Shared VIP | [`controlPlaneEndpoint.advertisement.vip`](../reference/cluster-config.md#kubernetes-api-endpoint) | A control-plane node holds the VIP only while its local kube-apiserver is healthy. |

## Access and observe

| Talos | KatlOS | Key difference |
| --- | --- | --- |
| `talosconfig` context | `katlctl` [context](../operations/access.md#save-a-context) from `katlctl context save` | A context is a workstation record of node identities and addresses. It never holds desired state, so you can recreate it from `cluster.yaml` with `katlctl context save`. |
| apid on port 50000, always mTLS | The management API served by the `katlc` management agent on port 9443 | The default trusted-network mode is weaker than Talos: connections are unauthenticated plaintext, so network access grants node management. mTLS is opt-in and must be [chosen before installation](../operations/management-auth.md#choose-an-access-mode). See [network trust](../support.md#network-trust). |
| No SSH or shell | Key-only SSH and a local console | Use SSH for arbitrary administration. The management API exposes bounded lifecycle operations, not remote command execution. |
| `talosctl dashboard` | `katlctl cluster status` and `katlctl node logs` remotely; a read-only dashboard on the local `tty1` console | The KatlOS dashboard is local only. `node logs` streams the native systemd journal. |
| `talosctl logs` | `katlctl node logs` | Reads or follows the node's systemd journal. |
| `talosctl health` | `katlctl cluster status` | Reports the state of every node in the configuration. |
| `talosctl kubeconfig` | `katlctl cluster kubeconfig ./kubeconfig --config ./cluster.yaml` | Writes a separate mode-0600 file; it does not merge into `~/.kube/config`. |
| KubePrism on `127.0.0.1:7445` | Katl's node-local API proxy on `127.0.0.1:7445` | Forwards TLS to a healthy control-plane backend; see [Cilium node-local API access](../operations/cilium.md#install-with-helm). |

## Lifecycle

| Talos | KatlOS | Key difference |
| --- | --- | --- |
| `talosctl bootstrap` | `katlctl cluster bootstrap --config ./cluster.yaml --init-node cp-1` | Bootstrap runs kubeadm, joins the configured nodes, and writes the operator kubeconfig. Nodes do not join on their own; add a node later with `katlctl node join`. |
| A/B system image | [Generations](generations.md) over two root slots | Configuration changes, Kubernetes upgrades, and host upgrades all create generations, and boot health falls back to the previous healthy generation once. |
| `talosctl upgrade` | `katlctl node upgrade cp-1 --config ./cluster.yaml --version VERSION` | Talos cordons and drains the node; Katl upgrades one node at a time and does not drain or cordon it. [Prepare the node](../operations/maintain-node.md) first. |
| `talosctl rollback` | `katlctl node generations select GENERATION cp-1 --config ./cluster.yaml`, then `katlctl node reboot` | Selection changes only the next boot; `--one-shot` selects a generation for one boot. |
| `talosctl upgrade-k8s` | `katlctl kubernetes upgrade --config ./cluster.yaml` | The target version comes from `spec.kubernetes.version` in `cluster.yaml`, not a command-line flag. The upgrade saves an etcd snapshot on each control plane first. |
| `talosctl etcd members` | `katlctl cluster etcd members --config ./cluster.yaml` | Shows stacked etcd members and endpoint health. |
| `talosctl etcd remove-member` | `katlctl cluster etcd remove` | Removes one explicitly identified stale member. There is no standalone etcd snapshot command. |
| `talosctl reboot` and `talosctl shutdown` | `katlctl node reboot` and `katlctl node shutdown` | Reboot waits for the node to return healthy. |
| `talosctl reset` | `katlctl node wipe` or `katlctl cluster wipe` | Wipe erases KatlOS boot artifacts and leaves the node powered off; boot installer media or PXE to reinstall. |

## Defaults that differ

- KatlOS ships no CNI. Talos installs Flannel by default; on KatlOS you install
  the CNI after bootstrap, for example [Cilium](../operations/cilium.md).
- Control-plane nodes accept workloads by default, whereas Talos keeps them
  unschedulable unless you allow it.
- The generated kubeadm profile keeps kube-proxy enabled; disable it in a
  native kubeadm configuration when your CNI replaces it.
- Management API access defaults to trusted-network mode rather than mTLS.

To try the workflow end to end, follow [Get started](../getting-started.md). For
what a generation contains and what host rollback restores, read
[Generations and boot health](generations.md).
