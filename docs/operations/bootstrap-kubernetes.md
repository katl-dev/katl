# Bootstrap Kubernetes on KatlOS

This procedure turns healthy generation 0 nodes into one kubeadm cluster. It is
an explicit mutation of node-local kubeadm state and the Kubernetes API.

## Prerequisites

- every intended node completed [generation 0 handoff](access.md);
- the same `.katlcfg` bundle used for installation is available;
- each node has a reachable address and protected per-node token file;
- the Kubernetes bundle reference names a version compatible with the KatlOS
  runtime;
- the control-plane endpoint resolves or routes as designed; and
- independent recovery/backup expectations are understood.

Katl fetches the Kubernetes bundle during this operation. Nodes need registry
and CA access to `ghcr.io` unless the bundle is supplied through an explicitly
supported local mechanism.

## Rebuild Changed Intent

If the source changed before any node was installed, rebuild the bundle. Do not
silently replace the bundle after installation:

```sh
katlctl config bundle ./cluster.yaml --output ./katl-lab.katlcfg
```

Katl maintains the bundle's internal consistency metadata; it is not an
operator input.

## Dry Run

Validate topology, node access, bundle selection, and bootstrap ordering without
running kubeadm:

```sh
katlctl cluster bootstrap \
  --dry-run \
  --config-bundle ./katl-lab.katlcfg \
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
katlctl cluster bootstrap \
  --config-bundle ./katl-lab.katlcfg \
  --init-node cp-1 \
  --kubeconfig-out ./kubeconfig \
  --overwrite-kubeconfig
```

The normal sequence verifies and stages the Kubernetes sysext, initializes the
first control plane, creates join material, joins remaining nodes, checks
post-kubeadm health, commits generation 1, and writes the operator kubeconfig.
Save the command output and resulting kubeconfig.

Bootstrap waits for its submitted operations, and their node-local records
remain queryable afterward. If the workstation disconnects or a result is
unclear, discover the affected node's current and recent operations:

```sh
katlctl operations list \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token
```

## Establish Cluster Networking

Kubeadm nodes normally remain `NotReady` until a CNI is installed. Katl does not
choose or operate a CNI. Either apply it after bootstrap with your cluster
management workflow, or explicitly include reviewed manifests and readiness
conditions:

```sh
katlctl cluster bootstrap \
  --config-bundle ./katl-lab.katlcfg \
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
host generation rollback does not erase it. Preserve the command result and follow
[Troubleshoot KatlOS](troubleshoot.md). Kubernetes upgrades use the separate
[Upgrade Kubernetes](upgrade-kubernetes.md) workflow. Additional-control-plane
repair and general reconciliation remain unsupported alpha operations.
