# Bootstrap Kubernetes on KatlOS

Use this procedure when your intended nodes already run KatlOS and you want
to create Kubernetes from the same configuration. Bootstrap starts the
Kubernetes API, joins the nodes, and gives you a kubeconfig.

## Prerequisites

- Every intended node has reached [generation 0](access.md#confirm-generation-0).
- The `cluster.yaml` used for installation is available.
- Every intended node is reachable through its host management interface on
  TCP port `9443` from the operator workstation.
- `spec.kubernetes.version` is available in this Katl release's compatibility
  catalog.
- The control-plane endpoint resolves or routes as designed.
- You understand which recovery and backup material you must keep
  independently of the nodes.

For any cluster you may rebuild, create and independently back up a
[Kubernetes identity](kubernetes-identity.md) before the first bootstrap. It
preserves kubeadm trust and signing keys; it is not an etcd or workload backup.

If you plan to use Cilium, its
[host settings](cilium.md#configure-cilium-host-settings) belong in
`cluster.yaml` before you install the nodes. Nodes installed without them need
`katlctl cluster apply` and the reboot that it reports.

Katl resolves and fetches the immutable Kubernetes bundle during this
operation, so the nodes need registry and CA access to `ghcr.io`. Katl does not
support air-gapped or registry-mirrored delivery of the Kubernetes bundle.

## Review ClusterConfig changes

If you edited `cluster.yaml` after installation, review the diff before
bootstrap and confirm that the ClusterConfig still describes the installed
nodes. `katlctl` compiles `cluster.yaml` internally; the normal path does not
require a separate configuration bundle file.

Do not rewrite the ClusterConfig merely to make bootstrap proceed.

## Plan bootstrap

Validate topology, node access, Kubernetes bundle selection, and bootstrap
ordering without running kubeadm:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --plan \
  --init-node cp-1
```

Planning uses the configured management mode without extra credential flags.
Trusted-network mode needs no secrets; mTLS uses the referenced secrets
file. Set `management.address` and, when different, `kubernetes.address` in
`cluster.yaml` before planning. The `--node-address` flag is hidden, and
bootstrap rejects it together with `--config`.

Review the plan, selected init node, node order, control-plane endpoint, and
Kubernetes version. Katl records the resolved Kubernetes bundle identity
internally. The plan does not create a generation or invoke kubeadm.

## Execute bootstrap

Run the same command without `--plan`:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --init-node cp-1
```

The normal sequence verifies and stages the Kubernetes bundle, initializes the
first control plane, creates join material, joins remaining nodes, checks
post-kubeadm health, commits the first Kubernetes-capable generation, and
writes the operator kubeconfig.
By default the kubeconfig is written to `./kubeconfig`. Use
`--kubeconfig-out` to choose another path, or `--overwrite-kubeconfig` when an
existing file is intentionally being replaced.

The command prints each node and operation phase as it changes. Add `--verbose`
to include operation IDs and the agent's current recovery guidance.

Bootstrap activates each node's generation live. After local Kubernetes health
checks pass, that generation becomes the persistent boot default without
rebooting the node. A later reboot uses the same generation.

The identity file is optional for disposable evaluations. Without it, kubeadm
generates the cluster CA and signing keys on the init node; losing all copies of
that PKI means a later fresh cluster cannot retain the same identity.

## If bootstrap fails

Bootstrap waits for its submitted operations, and their node-local records
remain queryable afterward. Rerun the same command with the same
`cluster.yaml` and identity file, and Katl decides from the recorded state how
to continue:

| Situation | What rerunning does |
| --- | --- |
| Matching bootstrap operations are still in flight. | Katl resumes observing them instead of submitting duplicates. |
| kubeadm succeeded, but local API configuration or the post-kubeadm health check failed. | After you correct the reported cause, Katl creates a continuation attempt that finishes configuration, checks health, and commits the prepared generation. It preserves the original failure record and does not rerun kubeadm or replace Kubernetes keys. |
| kubeadm itself failed, the node rebooted before recovery, or the prepared generation is unavailable. | Katl refuses automatic continuation and explains that the existing state needs inspection. Preserve that state while diagnosing the failure. |

Continuation also requires the same node, generation, bootstrap inputs, and
boot session, with no intervening mutating operation; otherwise Katl refuses it
as in the last row.

If a result is unclear, discover the affected node's
current and recent operations:

```sh
katlctl operations list \
  --config ./cluster.yaml --node cp-1
```

## Establish cluster networking

Kubeadm nodes normally remain `NotReady` and CoreDNS pending until a CNI is
installed. That is the expected result of a successful bootstrap: the API and
operator kubeconfig are ready for the user to install their chosen cluster
networking. Katl does not choose, install, operate, or verify a CNI.

Install it with your cluster management workflow. You may explicitly ask the
bounded bootstrap helper to apply reviewed manifests and wait for an outcome:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --init-node cp-1 \
  --manifest ./cni.yaml \
  --wait nodes-ready
```

Those options do not transfer ownership of the CNI to Katl and are not part of
the default bootstrap success contract.

Do not treat an arbitrary downloaded manifest as trusted merely because
`katlctl` can apply it.

Cilium needs one KatlOS-specific Helm value because `/etc` belongs to the
immutable host generation: set `sysctlfix.enabled=false`, and do not make
`/etc/sysctl.d` writable for its default init container. Follow
[Run Cilium on KatlOS](cilium.md) for the complete values.

## Verify the result

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

## Failure boundary

Rerunning the unchanged bootstrap command is the supported way to resume an
interrupted invocation. Do not change the ClusterConfig merely to bypass a
failed operation: if kubeadm or API mutation began, host generation rollback
does not erase it (see
[rollback is not a backup](../concepts/generations.md#rollback-is-not-a-backup)).
Preserve the command result and follow [Troubleshoot KatlOS](troubleshoot.md).

Kubernetes upgrades use the separate [Upgrade Kubernetes](upgrade-kubernetes.md)
workflow. Additional-control-plane addition or one-for-one replacement in a
healthy cluster uses the explicit [cluster membership](change-cluster-nodes.md)
workflow. General reconciliation, loss-of-quorum recovery, and arbitrary repair
after partial kubeadm mutation remain unsupported operations.
