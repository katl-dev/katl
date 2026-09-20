# Access Installed KatlOS Nodes

Use the cluster configuration and its referenced management secrets for installed
node access. A saved workstation context is optional convenience.

## Security Boundary

The `katlc` agent accepts mutually authenticated TLS on TCP port `9443`.
`katlctl config init` creates `management-secrets.yaml` beside the configuration
and sets `spec.managementIdentity` to its relative path. Katl installs a non-CA
server identity on each node; the secrets file retains cluster and operator
credentials. Routine commands have no certificate flags or enrollment prompts. The API can
remain reachable on the trusted network: callers without the cluster operator
certificate cannot query status or invoke any operation. Do not publish `9443`
to the Internet.

The installed system keeps an operator dashboard on VGA `tty1`. It reports the
KatlOS and Kubernetes versions from the booted generation, node addresses,
generation health, and a live journal tail. Press `Ctrl+Alt+F2` for the local
login console. The dashboard does not replace SSH or `katlctl`; it is a
read-only view of the same durable state. Kernel and direct system console
messages use `tty3` (`Ctrl+Alt+F3`) so they cannot scroll the dashboard or
the login console. Serial output remains available on `ttyS0`. Katl owns
the graphical console kernel argument; custom serial consoles remain configurable.

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

## Save a Workstation Shortcut

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

## Durable Cluster Secrets

Keep the file referenced by `spec.managementIdentity` across reinstalls. Paths
are relative to the configuration, so the project can move between workstations.
Plaintext secrets must have mode 0600. Never commit plaintext private keys.

For an existing cluster using the older workstation key store, export its
original authority and update the configuration in one command:

```sh
katlctl management identity export --config ./cluster.yaml
```

For a hand-written configuration of a **new** cluster, create its secrets explicitly:

```sh
katlctl management identity create --config ./cluster.yaml
```

These commands refuse to overwrite a secrets file. Bundle compilation, context
refresh, and routine commands never create a missing authority. Generating a new
authority cannot recover access to nodes installed with a different one.

The file can be encrypted with SOPS and committed alongside the configuration:

```sh
sops encrypt --in-place management-secrets.yaml
katlctl node status cp-1 --config ./cluster.yaml
```

Configure SOPS recipients first and make its decryption key available on each
operator workstation. Katl invokes `sops` only for encrypted files and does not
write a decrypted copy of the secrets file. A saved workstation context retains
the operator client certificate and private key in its mode-0600 file for
shortcut commands, but never the authority private key. Ordinary operations do
not rewrite the project secrets file. Back up the SOPS decryption key independently. An encrypted secrets file
without a usable decryption key is not a recoverable backup.

`katlctl management identity inspect FILE` reports public identity information
without printing private material. The management identity is separate from the
optional Kubernetes identity used to preserve kubeadm CAs across reprovisioning.

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

A deliberate reinstall with the same secrets preserves management trust.
Commands authenticate the expected node name and observe its current installation;
no replacement flag is required. `context save --config ./cluster.yaml` updates
the optional shortcut, including new enrollment and machine identities. A change
of installation during an operation is still rejected. A different authority or
a certificate for another node is never accepted automatically.

Deleting workstation context does not delete the referenced secrets or change
node trust. Use `--config ./cluster.yaml` directly or save the context again.

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

On a control-plane node with a Katl-managed VIP, the same command also reports
the stable endpoint, local API readiness, and whether that node owns the VIP.
`katlctl cluster status` adds a connection check to the stable endpoint from
the operator workstation, so local ownership is not mistaken for end-to-end
reachability. Use `--output json` for the complete bounded status.

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

If the secrets file is missing, restore it at the path referenced by the
configuration. For legacy backups, `management identity import FILE` restores
the old workstation key store; `management identity export --config CONFIG`
then moves it beside the configuration.

If only the saved context remains, it can still authorize routine operations,
but cannot issue server certificates for a reinstall. Preserve it and the
existing nodes while locating the original secrets. A provisioning bundle
contains neither the authority private key nor the operator key and cannot
recover them. If both secrets and context are lost, Katl cannot recover trust
through the management API. Privileged SSH or console access is a separate
recovery boundary; there is currently no automated trust-recovery command.
Do not delete working state or generate another authority as a diagnostic step.
