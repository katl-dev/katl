# Access installed KatlOS nodes

Use this guide after installation to confirm that each node reached generation
0, record the nodes on your workstation, and then reach them day to day through
SSH, the console, status, logs, reboot, shutdown, and kubeconfig retrieval.
To choose or manage how the management API authenticates, see
[Secure the management API](management-auth.md).

## Security boundary

The `katlc` management agent serves the management API on TCP `9443`. In the
default trusted-network mode, anyone who can reach that port can manage the
node, including destructive operations; opt-in mTLS authenticates and encrypts
the connection and must be [chosen before installation](management-auth.md#choose-an-access-mode).
[Network trust](../support.md#network-trust) describes what each port exposes.
Kubernetes kubeconfigs and SSH keys remain separate from management access.

## Confirm generation 0

Check every node from your workstation:

```sh
katlctl cluster status --config ./cluster.yaml
```

Before Kubernetes bootstrap, each node reports health `OK`, Kubernetes
`not-configured`, and generation `0`.

To inspect a node's units directly, log in as root over
[SSH](#connect-with-ssh), because reading the system journal requires root:

```sh
ssh root@192.0.2.11
systemctl is-active katl-boot-complete.target
systemctl is-active katlc-agent.service
systemctl status katl-boot-health.service --no-pager
journalctl -b -u katl-boot-health.service -u katlc-agent.service
```

Expected state before Kubernetes bootstrap:

- `katl-boot-complete.target` is active.
- `katlc-agent.service` is active.
- The console reports `waiting-for-cluster-bootstrap`.
- `katl-kubeadm-ready.target` is not active yet.

See [generations](../concepts/generations.md) for what generation 0 is and how
boot health decides whether a generation is kept.

## Save a context

A context is a workstation record, kept in `katlctl.yaml`, of the installed
nodes' identities and management addresses. It never holds desired state;
`cluster.yaml` does. Commands given `--config` confirm each node's identity
directly, so a context is optional. Save one when you want shorter commands or
want later commands to detect that an address now points to a different
machine. Use the same `cluster.yaml` you installed from:

```sh
katlctl context save --config ./cluster.yaml
```

`katlctl context save` connects to each node's management API with the
configured authentication mode, confirms that it is the node named in
`cluster.yaml`, and records its identity: the machine ID and an enrollment ID,
a random ID that each installation creates. It does not use SSH or alter the
node. Run it again after a reinstall to record the new identities.

## Use a context

Selecting the saved context is optional; it lets repeated commands omit
`--config`. Inspect the resolved context after saving it:

```sh
katlctl context show
katlctl context list
katlctl node status cp-1
```

With a current context, normal management commands need only the node, and
`--context` selects a non-current cluster. `katlctl context current` prints the
selection and `katlctl context use NAME` switches between saved clusters.
Commands run with `--config ./cluster.yaml` do not save or select a context,
and `katlctl cluster status --config ./cluster.yaml` summarizes every
configured node without requiring a saved context. Mutations verify the
answering node name and bind to its observed installation.

When a saved node's address changes, verify and save it explicitly:

```sh
katlctl context rebind --node cp-1 --endpoint 192.0.2.51
```

Rebind succeeds only when TLS authenticates the expected node name and the new
address reports the same node name, enrollment ID, and machine ID that the
context recorded.

Remove a context with `katlctl context delete NAME`. Deleting it does not delete
the referenced secrets, external cluster secrets, or nodes, and does not change
node trust; deleting the current context leaves no current selection. Use
`--config ./cluster.yaml` directly or save the context again.

## Connect with SSH

Use SSH for an interactive shell and arbitrary system administration. The
KatlOS management API intentionally exposes bounded lifecycle operations rather
than remote command execution.

The SSH keys in `cluster.yaml` authorize both the `katl` and `root` users on
installed nodes. Log in as `katl` for an unprivileged shell, or as `root` for
administration such as reading the system journal:

```sh
ssh root@192.0.2.11
```

`katlctl config init` reads supported public keys from the active SSH agent,
including keys available only through an agent such as 1Password. Use
`--ssh-authorized-key PATH` to authorize one key explicitly.

## Use the local console

The installed system keeps an operator dashboard on VGA `tty1`. It reports the
KatlOS and Kubernetes versions from the booted generation, node addresses,
generation health, and a live journal tail. The dashboard is a read-only view
of the same durable state and does not replace SSH or `katlctl`.

Press `Ctrl+Alt+F2` for the local login console. Kernel and direct system
console messages use `tty3` (`Ctrl+Alt+F3`) so they cannot scroll the dashboard
or the login console. Serial output remains available on `ttyS0`. Katl owns
the graphical console kernel argument; custom serial consoles remain configurable.

## Check status and logs

Show the current KatlOS version, generation, any staged next boot, health, and
whether the node is busy without exposing machine identity or operation IDs:

```sh
katlctl node status cp-1
```

On a control-plane node with a Katl-managed VIP, the same command also reports
the stable endpoint, local API readiness, and whether that node owns the VIP.
`katlctl cluster status` summarizes every configured node and adds a connection
check to the stable endpoint from the operator workstation, so local ownership
is not mistaken for end-to-end reachability. Use `--output json` for the
complete bounded status.

Read a unit's journal from the workstation:

```sh
katlctl node logs cp-1 --config ./cluster.yaml --unit kubelet --follow
```

## Reboot or shut down a node

For a Kubernetes node, first review
[workload and storage maintenance](maintain-node.md). Host lifecycle commands
leave workload drain and scheduling state under your control.

Reboot a node and wait for it to return healthy:

```sh
katlctl node reboot cp-1
```

Shut down a node and wait for its management API to stop:

```sh
katlctl node shutdown cp-1
```

Neither command requires a confirmation flag. Reboot honors the node's selected
boot target, including a generation already staged for next boot, and verifies
that a new agent instance returns on that generation with good boot health.
Shutdown can prove that KatlOS management is offline; without an out-of-band
power API, it cannot independently prove the machine's final hardware power
state. Use `--no-wait` only when intentionally detaching, and `--output json`
when a script needs structured output.

## Retrieve a kubeconfig

Retrieve a kubeconfig from a bootstrapped control plane:

```sh
katlctl cluster kubeconfig ./kubeconfig --config ./cluster.yaml
```

The command writes a private file without changing `~/.kube/config`; use
`--force` to replace an existing different file. Workstation commands otherwise
use the mode-0600 kubeconfig written by `katlctl cluster bootstrap`.

After successful Kubernetes bootstrap, an interactive root login on a node
automatically uses kubeadm's root-only `/etc/kubernetes/admin.conf`, so
`kubectl get nodes` needs no copy beneath immutable `/root`. An explicitly set
`KUBECONFIG`, including an empty diagnostic value, is preserved.
