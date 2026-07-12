# Bootstrap Kubernetes on KatlOS

This procedure turns healthy generation 0 nodes into one kubeadm cluster. It is
an explicit mutation of node-local kubeadm state and the Kubernetes API.

## Prerequisites

- every intended node completed [generation 0 handoff](access.md);
- the same verified `.katlcfg` bundle used for installation is available;
- its internal `bundleDigest` is recorded;
- each node has a reachable address and protected per-node token file;
- the Kubernetes bundle reference is digest-pinned and compatible with the
  KatlOS runtime;
- the control-plane endpoint resolves or routes as designed; and
- independent recovery/backup expectations are understood.

Katl fetches the Kubernetes bundle during this operation. Nodes need registry
and CA access to `ghcr.io` unless the bundle is supplied through an explicitly
supported local mechanism.

## Review the Compiled Intent

Revalidate the source and, if needed, rebuild the bundle before any node has
been installed. Do not silently replace the bundle after installation:

```sh
katlctl config validate ./cluster.yaml
katlctl config bundle ./cluster.yaml --output ./katl-lab.katlcfg
sha256sum ./katl-lab.katlcfg
```

Use the `bundleDigest` printed by `config bundle`, not the archive SHA-256, in
the bootstrap command.

## Dry Run

Validate topology, node access, bundle selection, and bootstrap ordering without
running kubeadm:

```sh
BUNDLE_DIGEST='sha256:...'
katlctl cluster bootstrap \
  --dry-run \
  --config-bundle ./katl-lab.katlcfg \
  --config-bundle-digest "$BUNDLE_DIGEST" \
  --init-node cp-1 \
  --kubeconfig-out ./kubeconfig
```

When every node has a `file:` credential reference, do not add a common
`--agent-token-file`; `katlctl` reads the per-node files. Use
`--node-address node=address` only for an observed address that differs from the
compiled source.

Review the plan, selected init node, node order, control-plane endpoint,
Kubernetes version, and bundle reference. A dry run must not create generation
1 or invoke kubeadm.

## Execute Bootstrap

Run the same command without `--dry-run`:

```sh
BUNDLE_DIGEST='sha256:...'
katlctl cluster bootstrap \
  --config-bundle ./katl-lab.katlcfg \
  --config-bundle-digest "$BUNDLE_DIGEST" \
  --init-node cp-1 \
  --kubeconfig-out ./kubeconfig \
  --overwrite-kubeconfig
```

The normal sequence verifies and stages the Kubernetes sysext, initializes the
first control plane, creates join material, joins remaining nodes, checks
post-kubeadm health, commits generation 1, and writes the operator kubeconfig.
Save the command output and returned operation IDs.

## Establish Cluster Networking

Kubeadm nodes normally remain `NotReady` until a CNI is installed. Katl does not
choose or operate a CNI. Either apply it after bootstrap with your cluster
management workflow, or explicitly include reviewed manifests and readiness
conditions:

```sh
BUNDLE_DIGEST='sha256:...'
katlctl cluster bootstrap \
  --config-bundle ./katl-lab.katlcfg \
  --config-bundle-digest "$BUNDLE_DIGEST" \
  --init-node cp-1 \
  --kubeconfig-out ./kubeconfig \
  --bootstrap-manifest ./cni.yaml \
  --bootstrap-wait nodes-ready
```

Do not treat an arbitrary downloaded manifest as trusted merely because
`katlctl` can apply it.

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

Bootstrap is complete only when the command succeeds, expected generation and
operation records are terminal, the API is reachable through the intended
endpoint, and node readiness matches the chosen CNI stage.

## Failure Boundary

Do not assume rerunning bootstrap is safe. If kubeadm or API mutation began,
host generation rollback does not erase it. Preserve operation IDs and follow
[Troubleshoot KatlOS](troubleshoot.md). Additional-control-plane repair,
Kubernetes version upgrade, and general reconciliation are not supported alpha
operations.
