# Access Installed KatlOS Nodes

Complete this runbook after generation 0 boots and before bootstrap, config
apply, host upgrade, or wipe operations.

## Security Boundary

The alpha `katlc` agent listens on TCP port `9443` and authenticates a bearer
token, but its gRPC transport is not encrypted. Do not expose it to the public
Internet, an untrusted LAN, or a shared production network. Use an isolated
evaluation management network and restrict port `9443` at the surrounding
firewall.

`katlctl cluster enroll` uses SSH once to retrieve each initial per-node token,
stores it with mode `0600`, and creates a workstation context. Treat the managed
files like private keys: never commit them, never put token values in
`ClusterConfig`, and redact them from logs and issues.

The installed system keeps an operator dashboard on VGA `tty1`. It reports the
node addresses, boot and generation health, installer handoff state, and a live
journal tail. Press `Ctrl+Alt+F2` for the local login console. The dashboard does
not replace SSH or `katlctl`; it is a read-only view of the same durable state.

## Confirm Generation 0

On each node:

```sh
systemctl is-active katl-boot-complete.target
systemctl is-active katlc-agent.service
systemctl status katl-runtime-handoff-status.service --no-pager
journalctl -b -u katl-runtime-handoff-status.service -u katlc-agent.service
```

Expected state before Kubernetes bootstrap:

- `katl-boot-complete.target` is active;
- `katlc-agent.service` is active;
- runtime handoff reports `waiting-for-cluster-bootstrap`; and
- `katl-kubeadm-ready.target` is not active yet.

## Enroll the Cluster

Use the same source used for installation:

```sh
katlctl cluster enroll ./cluster.yaml
```

The command connects as `root` using the workstation's normal SSH agent. Use
`--identity-file PATH` or `--ssh-user USER` when needed. For every node it:

- reads `/var/lib/katl/agent/token` without printing it;
- writes the configured `file:` credential with mode `0600`;
- verifies authenticated access to TCP port `9443`; and
- writes or updates the selected cluster in `katlctl.yaml`.

`katlctl config init` generates the matching credential references. In a
hand-authored `ClusterConfig`, use workstation paths rather than secret values:

```yaml
nodes:
  - name: cp-1
    systemRole: control-plane
    overrides:
      bootstrap:
        address: 192.0.2.11
        access:
          method: agent
            credentialRef: file:/home/operator/.config/katl/credentials/katl-lab/cp-1.token
  - name: worker-1
    systemRole: worker
    overrides:
      bootstrap:
        address: 192.0.2.21
        access:
          method: agent
            credentialRef: file:/home/operator/.config/katl/credentials/katl-lab/worker-1.token
```

Each freshly installed node generates its own token. Do not assume one fallback
token authenticates to the entire cluster. Enrollment refuses to overwrite a
different local token unless `--force` is explicit.

## Connectivity Check

Inspect the resolved context after enrollment:

```sh
katlctl config topology
katlctl operations list --node cp-1
```

Enrollment has already performed the authenticated agent health check. Normal
management commands now need only `--node`; `--context` selects a non-current
cluster. Explicit `--endpoint` and `--agent-token-file` remain expert overrides.

There is no supported alpha token-rotation workflow. If a token is exposed,
isolate the node and treat the evaluation identity as compromised; do not
silently replace the file and assume recovery is complete.
