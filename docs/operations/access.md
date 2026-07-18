# Access Installed KatlOS Nodes

Complete this runbook after generation 0 boots and before bootstrap, node
configuration, node upgrade, or wipe operations.

## Security Boundary

The alpha `katlc` agent listens on TCP port `9443` without authentication or
transport encryption. This is deliberate for Katl's trusted home-lab network:
routine management must not require credential enrollment. Do not expose it to
the public Internet, an untrusted LAN, or a shared production network. Restrict
port `9443` at the surrounding firewall when the network boundary is broader
than the supported path.

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

## Save a Workstation Context (Optional)

Use the same source used for installation:

```sh
katlctl context save --config ./cluster.yaml
```

For every node, the command verifies access to TCP port `9443` and writes or
updates the selected cluster topology in `katlctl.yaml`. It does not use SSH,
retrieve secrets, or alter the node.

`katlctl config init` and `katlctl install discover CLUSTER_CONFIG` also read
supported public keys from the active SSH agent when creating the initial SSH
authorization. This works with agent-only keys such as 1Password. An explicit
`--ssh-authorized-key PATH` remains available when only one key should be
authorized.

`ClusterConfig` remains sufficient for installation and Kubernetes bootstrap;
saving a context is only a convenience for repeated day-two commands.

## Connectivity Check

Inspect the resolved context after saving it:

```sh
katlctl context show
katlctl context list
katlctl node status cp-1
```

The save command has already performed the agent health check. Normal management
commands now need only `--node`; `--context` selects a non-current cluster.
An explicit `--endpoint` remains available when operating without a saved
context.

Use `katlctl context current` to print the selection and `katlctl context use
NAME` to switch between saved clusters. `katlctl cluster status --config
./cluster.yaml` summarizes every configured node without requiring a saved
context.

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

There is no node credential to rotate in the alpha management path. If the
trusted management network is exposed, isolate the node and restore the network
boundary before resuming lifecycle operations.
