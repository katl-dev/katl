# Access Installed KatlOS Nodes

Complete this runbook after generation 0 boots and before bootstrap, node
configuration, node upgrade, or wipe operations.

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
KatlOS and Kubernetes versions from the booted generation, node addresses,
generation health, and a live journal tail. Press `Ctrl+Alt+F2` for the local
login console. The dashboard does not replace SSH or `katlctl`; it is a
read-only view of the same durable state.

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
katlctl cluster enroll --config ./cluster.yaml
```

The command connects as `root` using the workstation's normal SSH agent. Use
`--identity-file PATH` or `--ssh-user USER` when needed. For every node it:

- reads `/var/lib/katl/agent/token` without printing it;
- writes the configured `file:` credential with mode `0600`;
- verifies authenticated access to TCP port `9443`; and
- writes or updates the selected cluster in `katlctl.yaml`.

`katlctl config init` and `katlctl install discover CLUSTER_CONFIG` also read
supported public keys from the active SSH agent when creating the initial SSH
authorization. This works with agent-only keys such as 1Password. An explicit
`--ssh-authorized-key PATH` remains available when only one key should be
authorized.

ClusterConfig contains node addresses but never credentials or workstation
paths. Enrollment creates and maintains those references in the workstation
context instead.

Each freshly installed node generates its own token. Do not assume one fallback
token authenticates to the entire cluster. Enrollment refuses to overwrite a
different local token unless `--force` is explicit.

## Connectivity Check

Inspect the resolved context after enrollment:

```sh
katlctl context show
katlctl node status cp-1
```

Enrollment has already performed the authenticated agent health check. Normal
management commands now need only `--node`; `--context` selects a non-current
cluster. Explicit `--endpoint` and `--agent-token-file` remain expert overrides.

## Routine Host Management

Show the current KatlOS version, generation, any staged next boot, health, and
whether the node is busy without exposing machine identity or operation IDs:

```sh
katlctl node status cp-1
```

Reboot a node and wait for it to return healthy:

```sh
katlctl node reboot cp-1
```

The reboot command does not require a confirmation flag. It honors the node's
selected boot target, including a generation already staged for next boot, and
verifies that a new agent instance returns on that generation with good boot
health.
Use `--no-wait` only when intentionally detaching, and `--output json` when a
script needs structured output.

Use SSH for an interactive shell and arbitrary system administration. The
KatlOS management API intentionally exposes bounded lifecycle operations rather
than remote command execution.

There is no supported alpha token-rotation workflow. If a token is exposed,
isolate the node and treat the evaluation identity as compromised; do not
silently replace the file and assume recovery is complete.
