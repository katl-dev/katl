# Access Installed KatlOS Nodes

Use the cluster configuration for installed node access from any workstation.
A saved workstation context is optional convenience.

```sh
katlctl node status cp-1 --config ./cluster.yaml
katlctl node logs cp-1 --config ./cluster.yaml --unit kubelet --follow
katlctl cluster kubeconfig ./kubeconfig --config ./cluster.yaml
```

These commands do not save or select a workstation context. Kubeconfig retrieval
writes a private file from a bootstrapped control plane without changing
`~/.kube/config`; use `--force` to replace an existing different file.
Remove a shortcut with `katlctl context delete NAME`. External cluster secrets
and nodes are untouched. Deleting the current context leaves no current selection.

## Security Boundary

The `katlc` agent listens on TCP `9443`. New configurations default to
`spec.managementAuthentication: trusted-network`: the API uses plaintext,
unauthenticated connections, and network access grants node-management access,
including destructive operations. Keep it on your trusted management network.
There are no management keys to create, copy, encrypt, or recover in this mode.
Kubernetes kubeconfigs and SSH keys remain separate.

Choose `spec.managementAuthentication: mtls` when the management connection needs
authentication and encryption. `katlctl config init --management-authentication
mtls` creates `management-secrets.yaml` beside the configuration and references
it through `spec.managementIdentity`. See [durable cluster secrets](#durable-cluster-secrets).

An existing configuration with a secrets reference and no explicit mode uses
mTLS. Existing installed nodes without a mode record also keep mTLS after an OS
upgrade. A failed TLS handshake never falls back to trusted-network access.
Changing this installation setting requires deliberate reprovisioning with the
chosen mode; editing the source or applying a generation does not change a
running node's authentication. A new trusted-network installation requires a
release supporting this setting on both the installer and installed runtime.

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

For every node, the command connects using the selected authentication mode,
confirms that the answering agent is enrolled under the requested
inventory node name, and records its immutable enrollment identity and machine
ID in `katlctl.yaml`. It does not use SSH or alter the node.

`katlctl config init` and `katlctl install discover CLUSTER_CONFIG` also read
supported public keys from the active SSH agent when creating the initial SSH
authorization. This works with agent-only keys such as 1Password. An explicit
`--ssh-authorized-key PATH` remains available when only one key should be
authorized.

## Durable Cluster Secrets

This section applies only to opt-in mTLS. Trusted-network clusters need only
their configuration on each workstation. Reinstalling a node refreshes the
observed installation identity automatically; an identity change during an
operation is still rejected.

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

A compiled mTLS `.katlcfg` contains the non-CA private server key for each selected
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
An explicit `--endpoint` uses the selected configuration or context's mode.
Without either, it uses trusted-network access and cannot connect to mTLS nodes.
Mutations verify the answering node name and bind to its observed installation.
When a saved
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

## Rotate a management authority

If an mTLS authority or operator key was exposed, create a replacement authority
and switch every installed node through `katlctl`. Encrypting the exposed file
does not revoke certificates signed by it. This workflow changes only Katl
management mTLS credentials; Kubernetes CA and service-account keys are separate.

Before rotation, upgrade every node to a KatlOS release that supports
`katlc agent rotate-management`. Keep the original secrets file and working
`ClusterConfig`. From the operator workstation, verify root SSH access to each
management address with a trusted, recorded SSH host key. The rotation command
requires strict host-key checking and refuses an unknown key. Finish or recover
any active node operation before starting.

Choose a new path that is not the original secrets path. To keep the replacement
in the repository, configure a matching SOPS creation rule and an available age
decryption key, then use a `.sops.yaml` filename. Katl encrypts the replacement
before contacting any node and refuses SOPS output that leaves a management
private key in plaintext.

```sh
katlctl management identity rotate \
  --config ./cluster.yaml \
  --output ./management-secrets-next.sops.yaml
```

The command preflights all nodes over root SSH, switches them one at a time,
and verifies that the new mTLS authority works and the old one is rejected.
Only after every node passes does it change `spec.managementIdentity` to the new
file. A saved workstation context still has the old client certificate; refresh
it after the command succeeds:

```sh
katlctl context save --config ./cluster.yaml
katlctl cluster status --config ./cluster.yaml
```

If rotation stops partway through, keep the new secrets file and rerun the exact
same command. Nodes already switched accept the same replacement again. The
source configuration continues to reference the original authority until all
nodes pass. If an agent fails to restart, use root SSH to inspect
`katlc-agent.service` and its journal, repair that failure, and rerun. Do not
generate another replacement during recovery.

After verification, remove exposed plaintext secrets from the tracked tree and
keep only protected recovery copies. Published Git history remains a separate
exposure even after the current tree is cleaned. Back up the new SOPS decryption
key outside the cluster.

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
