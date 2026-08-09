# Preserve Kubernetes Identity Across Reprovisioning

A Kubernetes identity is an operator-held secret that lets kubeadm build a
newly provisioned cluster with the same trust roots and signing identity as a
previous installation. Create or import it before the first bootstrap, store it
away from the nodes, and supply it explicitly whenever the whole cluster is
rebuilt.

This is identity continuity, not a cluster backup. Reusing it does not restore
etcd data, Kubernetes API objects, workload secrets, persistent volumes, CNI
state, or applications.

## What the identity contains

Katl carries the standard shared kubeadm PKI files:

| Purpose | Files installed before `kubeadm init` |
| --- | --- |
| Kubernetes API trust | `ca.crt`, `ca.key` |
| Aggregation/front-proxy trust | `front-proxy-ca.crt`, `front-proxy-ca.key` |
| Stacked-etcd trust | `etcd/ca.crt`, `etcd/ca.key` |
| Service-account token signing | `sa.key`, `sa.pub` |

Katl deliberately excludes node-specific API server, etcd peer/server,
front-proxy client, and kubelet certificates. Kubeadm generates those leaf
certificates for the names and addresses of each newly provisioned node. Join
tokens and the temporary kubeadm certificate-upload key are also short-lived
operation material, not cluster identity.

The identity is separate from `ClusterConfig` and `.katlcfg`. A `.katlcfg` may
be published from Matchbox or another provisioning server; it must never
contain cluster CA private keys.

## Create an identity before first bootstrap

Use the exact `metadata.name` from `cluster.yaml`:

```sh
katlctl kubernetes identity create \
  --cluster-name homelab \
  --output ./homelab-kubernetes-identity.katlkey
```

Katl creates a new file with mode `0600` and refuses to overwrite an existing
one. Move a protected backup to storage that survives loss of the operator
workstation and every cluster node. Anyone with this file can mint trusted
Kubernetes and etcd certificates and service-account tokens.

Inspection validates all certificate/key pairs and prints only public
fingerprints:

```sh
katlctl kubernetes identity inspect \
  ./homelab-kubernetes-identity.katlkey
```

Record the overall fingerprint with the backup. `katlctl` refuses an identity
whose cluster name differs from `ClusterConfig`, whose key pairs do not match,
whose CAs are not currently valid, or whose file permissions allow group or
other access.

## Import an existing kubeadm identity

On a trusted machine with a complete standard kubeadm PKI directory:

```sh
katlctl kubernetes identity create \
  --cluster-name homelab \
  --from-kubeadm-pki ./kubeadm-pki \
  --output ./homelab-kubernetes-identity.katlkey
```

The input directory must contain all eight shared files listed above. Katl
does not import node leaf certificates or `admin.conf`. Securely erase any
temporary copy after verifying and backing up the resulting identity.

Import before destroying the final healthy copy of the old PKI. Katl does not
offer a credential-free remote private-key export through the node-management
API.

## Bootstrap or rebuild with the identity

Dry-run validates the local identity and its cluster binding without sending
it or mutating nodes:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --init-node cp-1 --dry-run
```

Then use the same inputs for bootstrap:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --init-node cp-1
```

Katl sends the secret only in the explicit init operation, records only its
digest and public fingerprint, installs the eight files before `kubeadm init`,
and removes its operation-scoped staging copy. Kubeadm distributes the required
shared material to additional control planes through its normal short-lived
join mechanism.

The node-management API on TCP `9443` encrypts this transfer with automatic
mTLS and authenticates both the operator and expected node. KatlOS also blocks
management ingress from interfaces created later by workload networking. Keep
the operation on the supported trusted-management LAN; this is not a general
production or multi-tenant security claim.

If shared PKI already exists on the init node, Katl proceeds only when every
existing file is byte-for-byte identical. A different identity, or incomplete
shared identity beside node-specific leaf certificates, fails before kubeadm
runs. Inspect and preserve that state; do not replace individual keys to force
bootstrap.

## What remains separate

Keep these independently:

- etcd snapshots or another Kubernetes API-state backup;
- workload and persistent-volume backups;
- encryption-at-rest provider keys, if configured by your platform;
- the source `ClusterConfig` and release information; and
- operator kubeconfigs or other client credentials that you intend to retain.

Reusing the identity makes the new control plane use the same trust anchors. It
does not recreate the old cluster's object UIDs, RBAC changes, data, or running
workloads. Follow [Bootstrap Kubernetes](bootstrap-kubernetes.md) for the normal
handoff and install your chosen CNI afterward.
