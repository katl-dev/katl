# Bootstrap Kubernetes on KatlOS

This procedure turns healthy generation 0 nodes into one kubeadm cluster. It is
an explicit mutation of node-local kubeadm state and the Kubernetes API.

## Prerequisites

- every intended node completed [generation 0 handoff](access.md);
- the same `ClusterConfig` source used for installation is available;
- every intended node is reachable through its host management interface on
  TCP port `9443` from the operator workstation;
- `spec.kubernetes.version` is available in this Katl release's compatibility
  catalog;
- the control-plane endpoint resolves or routes as designed; and
- independent recovery/backup expectations are understood.

For any cluster you may rebuild, create and independently back up a
[Kubernetes identity](kubernetes-identity.md) before the first bootstrap. It
preserves kubeadm trust and signing keys; it is not an etcd or workload backup.

Katl resolves and fetches the immutable Kubernetes bundle during this
operation. Nodes need registry and CA access to `ghcr.io` unless the bundle is
supplied through an explicitly supported local mechanism.

## Review Changed Intent

If the source changed after installation, review the diff before bootstrap and
make sure it still describes the installed nodes. `katlctl` compiles the source
internally; the normal path does not require a separate bundle file.

Do not silently replace the cluster intent merely to make bootstrap proceed.

## Dry Run

Validate topology, node access, bundle selection, and bootstrap ordering without
running kubeadm:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --dry-run \
  --init-node cp-1
```

The automatically retained management identity authenticates this read-only
planning access; there are no extra credential flags. Use `--node-address
node=address` only for an observed address that differs from the compiled
source.

Review the plan, selected init node, node order, control-plane endpoint, and
Kubernetes version. Katl records the resolved bundle identity internally. A dry
run must not create generation 1 or invoke kubeadm.

## Execute Bootstrap

Run the same command without `--dry-run`:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --init-node cp-1
```

The normal sequence verifies and stages the Kubernetes sysext, initializes the
first control plane, creates join material, joins remaining nodes, checks
post-kubeadm health, commits generation 1, and writes the operator kubeconfig.
By default the kubeconfig is written to `./kubeconfig`. Use
`--kubeconfig-out` to choose another path, or `--overwrite-kubeconfig` when an
existing file is intentionally being replaced.

The command prints each node and operation phase as it changes. Add `--verbose`
to include operation IDs and the agent's current recovery guidance.

The identity file is optional for disposable evaluations. Without it, kubeadm
generates the cluster CA and signing keys on the init node; losing all copies of
that PKI means a later fresh cluster cannot retain the same identity.

Bootstrap waits for its submitted operations, and their node-local records
remain queryable afterward. Rerunning the same command with the same config
resumes observation of matching in-flight bootstrap operations instead of
submitting duplicates. If a result is unclear, discover the affected node's
current and recent operations:

```sh
katlctl operations list \
  --config ./cluster.yaml --node cp-1
```

## Establish Cluster Networking

Kubeadm nodes normally remain `NotReady` and CoreDNS pending until a CNI is
installed. That is the expected successful bootstrap handoff: the API and
operator kubeconfig are ready for the user to install their chosen cluster
networking. Katl does not choose, install, operate, or verify a CNI.

Install it with your cluster management workflow. You may explicitly ask the
bounded bootstrap helper to apply reviewed manifests and wait for an outcome:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --init-node cp-1 \
  --bootstrap-manifest ./cni.yaml \
  --bootstrap-wait nodes-ready
```

Those options do not transfer ownership of the CNI to Katl and are not part of
the default bootstrap success contract.

Do not treat an arbitrary downloaded manifest as trusted merely because
`katlctl` can apply it.

Cilium requires one KatlOS-specific upstream Helm value because `/etc` belongs
to the immutable host generation. Follow [Run Cilium on
KatlOS](cilium.md) and set `sysctlfix.enabled=false`; do not make
`/etc/sysctl.d` writable for its default init container.

## Verify Handoff

```sh
kubectl --kubeconfig ./kubeconfig get nodes -o wide
kubectl --kubeconfig ./kubeconfig get pods -A
```

On each node, confirm the agent and kubelet state:

```sh
systemctl is-active katlc-agent.service
systemctl status kubelet --no-pager
systemctl status katl-kubeadm-ready.target --no-pager
```

Bootstrap is complete when the command succeeds, expected generation and
operation records are terminal, and the API is reachable through the intended
endpoint. Node and CoreDNS readiness are post-bootstrap outcomes of the
user-selected CNI.

## Failure Boundary

Rerunning the unchanged bootstrap command is the supported way to resume an
interrupted invocation. Do not change cluster intent merely to bypass a failed
operation: if kubeadm or API mutation began, host generation rollback does not
erase it. Preserve the command result and follow [Troubleshoot
KatlOS](troubleshoot.md). Kubernetes upgrades use the separate
[Upgrade Kubernetes](upgrade-kubernetes.md) workflow. Additional-control-plane
addition or one-for-one replacement in a healthy cluster uses the explicit
[cluster membership](change-cluster-nodes.md) workflow. General reconciliation,
loss-of-quorum recovery, and arbitrary repair after partial kubeadm mutation
remain unsupported beta operations.
