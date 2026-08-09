# Access Installed KatlOS Nodes

Complete this runbook after generation 0 boots and before bootstrap, node
configuration, node upgrade, or wipe operations.

## Security Boundary

The `katlc` agent accepts mutually authenticated TLS on TCP port `9443`.
`katlctl` creates the cluster management identity automatically while preparing
the first config or install bundle, installs a non-CA server identity on each
node, and retains the operator identity in the mode-0600 workstation context.
Routine commands have no certificate flags or enrollment prompts.

KatlOS also admits port `9443` only through interfaces present after host
networking comes online and before containerd or kubelet starts. Interfaces
created later by a CNI are not added when the service restarts. This is a
defence-in-depth boundary for a trusted home-lab management network, not a
production or multi-tenant firewall policy. Do not publish `9443` to the
Internet.

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
systemctl is-active katlc-management-firewall.service
systemctl status katl-runtime-handoff-status.service --no-pager
journalctl -b -u katl-runtime-handoff-status.service -u katlc-agent.service
```

Expected state before Kubernetes bootstrap:

- `katl-boot-complete.target` is active;
- `katlc-agent.service` is active;
- `katlc-management-firewall.service` is active;
- runtime handoff reports `waiting-for-cluster-bootstrap`; and
- `katl-kubeadm-ready.target` is not active yet.

## Enroll Nodes on the Workstation

Use the same source used for installation:

```sh
katlctl context save --config ./cluster.yaml
```

For every node, the command authenticates the node name before making an API
request, confirms that the answering agent is enrolled under the requested
inventory node name, and records its immutable enrollment identity and machine
ID in `katlctl.yaml`. It does not use SSH or alter the node.

`katlctl config init` and `katlctl install discover CLUSTER_CONFIG` also read
supported public keys from the active SSH agent when creating the initial SSH
authorization. This works with agent-only keys such as 1Password. An explicit
`--ssh-authorized-key PATH` remains available when only one key should be
authorized.

`ClusterConfig` remains sufficient for installation. Planning, bootstrap, and
mutating commands additionally require the automatically retained management
identity and saved enrollment, so an unknown caller or stale/swapped address
cannot target another machine.

When Katl first prints `Created management identity`, back up the reported
`.katlkey` file separately from `cluster.yaml`. The normal path discovers it
automatically. It is needed to reinstall nodes with the same management trust
or to add another operator workstation. Restore it before compiling or
installing with:

```sh
katlctl management identity import ./homelab.katlkey
```

`katlctl management identity path homelab` prints the active backup location.
`katlctl management identity inspect IDENTITY` validates a backup and reports
its fingerprint and expiry without printing private material.
These recovery commands are not part of routine node operation. This management
identity is separate from the optional Kubernetes identity that preserves
kubeadm CAs across whole-cluster reprovisioning.

A compiled `.katlcfg` contains the non-CA private server key for each selected
node, so Katl writes it mode 0600. Treat it as short-lived provisioning
material: publish it only on the trusted installer/PXE network, and remove the
published copy after installation. Exposure does not grant caller access to an
installed agent, but it can let an attacker impersonate that node to an
operator who is also redirected to the attacker.

## Connectivity Check

Inspect the resolved context after saving it:

```sh
katlctl context show
katlctl context list
katlctl node status cp-1
```

The save command has already performed the agent health check. Normal management
commands now need only `--node`; `--context` selects a non-current cluster.
An explicit `--endpoint` still requires matching saved management access; it is
not an authentication bypass. Mutations use the saved address. When an enrolled
node's address changes, verify and save it explicitly:

```sh
katlctl context rebind --node cp-1 --endpoint 192.0.2.51
```

Rebind succeeds only when TLS authenticates the expected node name and the new
address reports the same inventory node, enrollment identity, and machine ID.

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

On a control-plane node with Katl-managed VIP advertisement, the same command
also reports the stable endpoint, local API readiness, route origination, BGP
peer state, and bounded route-exchange counts. `katlctl cluster status` adds a
connection check to the stable endpoint from the operator workstation, so a
locally originated route is not mistaken for end-to-end reachability. Use
`--output json` for the complete bounded status.

Reboot a node and wait for it to return healthy:

```sh
katlctl node reboot cp-1
```

Shut down a node and wait for its management API to stop:

```sh
katlctl node shutdown cp-1
```

The reboot command does not require a confirmation flag. It honors the node's
selected boot target, including a generation already staged for next boot, and
verifies that a new agent instance returns on that generation with good boot
health.
Neither command requires confirmation. Use `--no-wait` only when intentionally
detaching, and `--output json` when a script needs structured output. Shutdown
can prove that KatlOS management is offline; without an out-of-band power API,
it cannot independently prove the machine's final hardware power state.

Use SSH for an interactive shell and arbitrary system administration. The
KatlOS management API intentionally exposes bounded lifecycle operations rather
than remote command execution.

After successful Kubernetes bootstrap, an interactive root login automatically
uses kubeadm's root-only `/etc/kubernetes/admin.conf`; `kubectl get nodes` needs
no copy beneath immutable `/root`. An explicitly set `KUBECONFIG`, including an
empty diagnostic value, is preserved. Workstation commands continue to use the
mode-0600 kubeconfig written by `katlctl cluster bootstrap`.

If the management identity backup is lost but the saved context remains, that
workstation can continue routine operations but cannot issue server material
for a reinstall. Preserve the existing nodes and restore the backup; creating a
different identity does not grant access to them. If both copies are lost,
recovery requires deliberately reinstalling the affected nodes under a new
cluster management identity.
