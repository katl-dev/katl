# What KatlOS owns

Use this page to decide what to manage through `katlctl` and what to handle
through your own Kubernetes tools. KatlOS manages host releases and
configuration. Katl bootstrap sets up the Kubernetes API and gives you a
kubeconfig. You manage the cluster network, add-ons, workloads, and backups.

## Responsibility map

| Area | Owner | What that means |
| --- | --- | --- |
| Installer and target-disk mutation | Katl | Katl validates a selected node, stable disk identity, image, and destructive boundary before writing generation 0. |
| Immutable host generations and boot health | Katl | Katl stages, trial-boots, promotes, or falls back between versioned host generations. |
| Host configuration declared in `ClusterConfig` | Katl | Katl applies the supported SSH, storage, kernel, native Linux, networking, and extension domains. |
| kubeadm init, join, and supported upgrades | Katl | Katl performs explicit, durable operations and writes the operator kubeconfig. |
| Management API access | Operator | Trusted-network access is the default; optional mTLS uses management secrets kept with the project across reinstalls. A context is a workstation record that you can recreate from `cluster.yaml` with `katlctl context save`. |
| Kubernetes identity | Operator | Create or import, protect, and independently back up the optional Katl identity file; Katl validates and installs it only for explicit bootstrap. |
| DHCP, TFTP, iPXE, Matchbox, firmware boot order | Operator | Katl publishes artifacts and consumes a selected `.katlcfg`; it does not operate provisioning infrastructure. |
| Stable DNS and external routing | Operator | Katl can health-gate and advertise a configured VIP, but the surrounding DNS, routers, peers, and network policy remain yours. |
| CNI | Operator | Choose, install, upgrade, diagnose, and remove cluster networking yourself. |
| Workloads and cluster add-ons | Operator | GitOps, ingress, storage classes, CSI, monitoring, policy, secrets, and applications are outside Katl. |
| Backups and disaster recovery | Operator | Keep independent etcd, workload, and persistent-data backups. Host generation fallback is not a cluster backup. |

## What install and bootstrap leave ready

After installation, [generation 0](generations.md) is healthy when the
immutable host, network, SSH, and the `katlc` management agent are usable.
Kubernetes is deliberately absent, so the machine is ready for an explicit
bootstrap.

After `katlctl cluster bootstrap`, kubeadm, the Kubernetes API, control-plane
components, and the operator kubeconfig are ready. Without a CNI, nodes normally
remain `NotReady` and CoreDNS remains pending. That is a successful bootstrap,
not a failure: after bootstrap you install the CNI, and the cluster becomes
generally schedulable only once that networking is in place.

Katl's optional `--manifest` and wait flags can apply reviewed
operator manifests during the bootstrap command. They do not transfer
ownership of those resources to Katl.

## Persistent state and rollback

A Katl [generation](generations.md) owns the immutable runtime root, UKI,
selected system extensions, and compiled host configuration. Writable identity,
container, Kubernetes, etcd, and workload data live outside it, so falling back
to a previous host generation does not undo kubeadm, etcd, Kubernetes, CNI,
volume, or external-infrastructure changes; see
[rollback is not a backup](generations.md#rollback-is-not-a-backup).

The reusable [Kubernetes identity](../operations/kubernetes-identity.md) is an
operator secret outside both node generations and `ClusterConfig`. It preserves
shared kubeadm CA and signing keys across whole-cluster reprovisioning, but not
etcd contents or Kubernetes resources.

## Trust boundary

[Network trust](../support.md#network-trust) states what the installer on port
8080 and the management API on port 9443 expose, and how to choose between
trusted-network access and mTLS. SSH accepts public keys only, so it is usable
only when `ClusterConfig` supplies keys. The live installer authorizes
`root` only after the selected node's keys are handed off to it. On an installed
node, the SSH keys from `ClusterConfig` authorize both the `katl` account and
`root`, which accepts public-key logins only.
