# What KatlOS Owns

KatlOS prepares and manages Kubernetes nodes. It is not a Kubernetes
distribution and does not take ownership of the platform you run on top of
kubeadm. This boundary is intentional: a successful Katl bootstrap leaves the
Kubernetes API and kubeconfig ready for your cluster-management workflow.

## Responsibility Map

| Area | Owner | What that means |
| --- | --- | --- |
| Installer and target-disk mutation | Katl | Katl validates a selected node, stable disk identity, image, and destructive boundary before writing generation 0. |
| Immutable host generations and boot health | Katl | Katl stages, trial-boots, promotes, or falls back between versioned host generations. |
| Host configuration declared in `ClusterConfig` | Katl | Katl applies the supported SSH, storage, kernel, native Linux, networking, and extension domains. |
| kubeadm init, join, and supported upgrades | Katl | Katl performs explicit, durable operations and writes the operator kubeconfig. |
| Katl node-management trust | Katl and operator | Katl automatically issues and uses node/operator mTLS identities; the operator backs up the reported cluster management identity for reinstall and workstation recovery. |
| Kubernetes trust identity | Operator | Create or import, protect, and independently back up the optional Katl identity file; Katl validates and installs it only for explicit bootstrap. |
| DHCP, TFTP, iPXE, Matchbox, firmware boot order | Operator | Katl publishes artifacts and consumes a selected `.katlcfg`; it does not operate provisioning infrastructure. |
| Stable DNS and external routing | Operator | Katl can health-gate and advertise a configured VIP, but the surrounding DNS, routers, peers, and network policy remain yours. |
| CNI | Operator | Choose, install, upgrade, diagnose, and remove cluster networking yourself. |
| Workloads and cluster add-ons | Operator | GitOps, ingress, storage classes, CSI, monitoring, policy, secrets, and applications are outside Katl. |
| Backups and disaster recovery | Operator | Keep independent etcd, workload, and persistent-data backups. Host generation fallback is not a cluster backup. |

## The Two Initial Handoffs

After installation, **generation 0** is healthy when the immutable host,
network, SSH, and `katlc` agent are usable. Kubernetes is deliberately absent.
This is the point at which the machine is ready for an explicit bootstrap.

After `katlctl cluster bootstrap`, kubeadm, the Kubernetes API, control-plane
components, and the operator kubeconfig are ready. Without a CNI, nodes normally
remain `NotReady` and CoreDNS remains pending. That is a successful Katl
handoff, not a failed bootstrap. The cluster becomes generally schedulable only
after you install compatible networking.

Katl's optional `--bootstrap-manifest` and wait flags can apply reviewed
operator manifests during the bootstrap command. They do not transfer
ownership of those resources to Katl.

## Persistent State and Rollback

Katl generations own the immutable runtime root, UKI, selected system
extensions, and compiled host configuration. Writable identity, container,
Kubernetes, etcd, and workload data live outside that generation.

The reusable [Kubernetes identity](../operations/kubernetes-identity.md) is an
operator secret outside both node generations and `ClusterConfig`. It preserves
shared kubeadm CA and signing keys across whole-cluster reprovisioning. It does
not preserve etcd contents or Kubernetes resources.

Falling back to a previous host generation therefore does **not** undo kubeadm
commands, etcd membership or data, Kubernetes API objects, CNI state, volumes,
or external infrastructure. Stop and inspect a `recoveryRequired` result rather
than assuming a host rollback restored the cluster.

## Trust Boundary

The beta installer handoff on TCP `8080` is intentionally unauthenticated HTTP
and belongs only on a trusted provisioning network. The installed-node API on
TCP `9443` requires automatic mTLS for every connection. SSH is key-only when
configured; the live installer exposes `root` only after its selected node keys
are handed off, while the installed runtime uses the `katl` account.

For the complete compatibility and security statement, read the
[support boundary](../support.md).
