# Access Installed KatlOS Nodes

Complete this runbook after generation 0 boots and before bootstrap, config
apply, host upgrade, or wipe operations.

## Security Boundary

The alpha `katlc` agent listens on TCP port `9443` and authenticates a bearer
token, but its gRPC transport is not encrypted. Do not expose it to the public
Internet, an untrusted LAN, or a shared production network. Use an isolated
evaluation management network and restrict port `9443` at the surrounding
firewall.

SSH is the supported way to retrieve the initial per-node token. Treat token
files like private keys: store them with mode `0600`, never commit them, never
put token values in `ClusterConfig`, and redact them from logs and issues.

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

## Collect One Token Per Node

Create a protected workstation directory, then copy each token over SSH using
the node address selected during installation:

```sh
install -d -m 0700 ./tokens
umask 077
ssh root@192.0.2.11 'cat /var/lib/katl/agent/token' > ./tokens/cp-1.token
ssh root@192.0.2.21 'cat /var/lib/katl/agent/token' > ./tokens/worker-1.token
chmod 0600 ./tokens/*.token
```

Each freshly installed node generates its own token. Do not assume one fallback
token authenticates to the entire cluster.

In `ClusterConfig`, use a reference to each workstation token path, not secret
material. `file:` paths are read by `katlctl` on the workstation:

```yaml
nodes:
  - name: cp-1
    systemRole: control-plane
    overrides:
      bootstrap:
        address: 192.0.2.11
        access:
          method: agent
          credentialRef: file:/absolute/path/to/tokens/cp-1.token
  - name: worker-1
    systemRole: worker
    overrides:
      bootstrap:
        address: 192.0.2.21
        access:
          method: agent
          credentialRef: file:/absolute/path/to/tokens/worker-1.token
```

The paths may exist after the bundle is compiled, but they must contain the
matching node tokens before `katlctl cluster bootstrap` runs.

## Connectivity Check

From the operator workstation, confirm only that the intended isolated path is
reachable:

```sh
nc -vz 192.0.2.11 9443
nc -vz 192.0.2.21 9443
```

A TCP connection is not proof of authenticated agent health. The first
plan/dry-run command in each lifecycle runbook is the authenticated preflight.

There is no supported alpha token-rotation workflow. If a token is exposed,
isolate the node and treat the evaluation identity as compromised; do not
silently replace the file and assume recovery is complete.
