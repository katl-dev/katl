# Secure the management API

Use this guide to choose how workstations authenticate to the KatlOS
management API and, for mTLS clusters, to keep, rotate, and recover the
management secrets. The `katlc` management agent serves the management API on
TCP `9443`; [Network trust](../support.md#network-trust) states what that port
and the installer port `8080` expose.

The management secrets covered here are separate from the optional
[Kubernetes identity](kubernetes-identity.md), which preserves kubeadm CAs
across reprovisioning. Kubernetes kubeconfigs and SSH keys are also separate.

## Choose an access mode

Choose the mode before installation, because it is fixed when a node is
installed.

New configurations default to `spec.managementAuthentication: mtls`.
`katlctl config init` creates `management-secrets.yaml` beside the configuration
and references it through `spec.managementIdentity`, so the management
connection is authenticated and encrypted. See
[durable cluster secrets](#durable-cluster-secrets).

Choose `spec.managementAuthentication: trusted-network` with
`katlctl config init --management-authentication trusted-network` only when the
management network is the boundary you trust. The API then uses plaintext,
unauthenticated connections, and network access grants node-management access,
including destructive operations. Cluster workloads count as being on that
network, because pods can reach node addresses (see
[network trust](../support.md#network-trust)). There are no management keys to
create, copy, encrypt, or recover in this mode.

`spec.managementAuthentication` is required unless `spec.managementIdentity` is
set, which implies `mtls`. `katlctl` refuses every command that reads a
configuration stating neither, and prints the line to add. It never infers the
mode, never changes an installed node, and never falls back to another mode.

A failed TLS handshake never falls back to trusted-network access.

### Change the access mode

Editing `cluster.yaml` or applying a generation does not change a running
node's authentication, because the mode is fixed when the node is installed.
To change it, set the new `spec.managementAuthentication` (for `mtls`, run
`katlctl management identity create --config ./cluster.yaml` first) and
reprovision each node with a [wipe and reinstall](wipe-reinstall.md). A node
that serves `trusted-network` management refuses an `mtls` configuration with a
message naming the mismatch; set `managementAuthentication: trusted-network` to
keep managing it.

Older mTLS configurations and nodes keep mTLS: an
existing configuration with a secrets reference and no explicit mode uses
mTLS, and installed nodes without a mode record keep mTLS after an OS upgrade.
An mTLS configuration without `spec.managementIdentity` takes its credentials
from a saved context, so run `katlctl context save --config ./cluster.yaml`
before other commands.
A new trusted-network installation requires a release that supports this
setting on both the installer and the installed runtime.

## Durable cluster secrets

This section applies to mTLS clusters, the default. Trusted-network clusters need only
their configuration on each workstation.

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

These commands refuse to overwrite a secrets file. Configuration bundle
compilation, context refresh, and routine commands never create a missing
authority, and generating a new authority cannot recover access to nodes
installed with a different one.

`katlctl management identity inspect FILE` reports public identity information
without printing private material.

A compiled mTLS `.katlcfg` contains the non-CA private server key for each selected
node, so Katl writes it mode 0600. Treat it as short-lived provisioning
material: publish it only on the trusted installer/PXE network, and remove the
published copy after installation. Exposure does not grant caller access to an
installed agent, but it can let an attacker impersonate that node to an
operator who is also redirected to the attacker.

### Encrypt the secrets with SOPS

Configure SOPS recipients first and make its decryption key available on each
operator workstation. The file can then be encrypted and committed alongside
the configuration:

```sh
sops encrypt --in-place management-secrets.yaml
katlctl node status cp-1 --config ./cluster.yaml
```

Katl invokes `sops` only for encrypted files and does not write a decrypted copy
of the secrets file. Ordinary operations do not rewrite the project secrets
file. A saved [context](access.md#save-a-context) retains the operator client
certificate and private key in its mode-0600 file for shortcut commands, but
never the authority private key.

Back up the SOPS decryption key independently. An encrypted secrets file
without a usable decryption key is not a recoverable backup.

## Check management connectivity

`katlctl context save` performs the agent health check for every node, and
[using a context](access.md#use-a-context) covers routine connections after
that. An explicit `--endpoint` uses the selected configuration or context's
mode. Without either, it uses trusted-network access and cannot connect to mTLS
nodes.

A deliberate reinstall with the same secrets preserves management trust.
Commands authenticate the expected node name and observe its current
installation, so no replacement flag is required. `katlctl context save --config
./cluster.yaml` updates the saved context, including the new enrollment and
machine IDs.
A change of installation during an operation is still rejected, and a different
authority or a certificate for another node is never accepted automatically.

## Rotate a management authority

If an mTLS authority or operator key was exposed, create a replacement authority
and switch every installed node through `katlctl`. Encrypting the exposed file
does not revoke certificates signed by it. This workflow changes only Katl
management mTLS credentials; Kubernetes CA and service-account keys are separate.

Before rotation:

1. Upgrade every node to a KatlOS release that supports
   `katlc agent rotate-management`.
2. Keep the original secrets file and working `cluster.yaml`.
3. From the operator workstation, verify root SSH access to each management
   address with a trusted, recorded SSH host key. The rotation command requires
   strict host-key checking and refuses an unknown key.
4. Finish or recover any active node operation.

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
same command. Nodes already switched accept the same replacement again, and
`cluster.yaml` continues to reference the original authority until all nodes
pass. If an agent fails to restart, use root SSH to inspect
`katlc-agent.service` and its journal, repair that failure, and rerun. Do not
generate another replacement during recovery.

After verification, remove exposed plaintext secrets from the tracked tree and
keep only protected recovery copies. Published Git history remains a separate
exposure even after the current tree is cleaned. Back up the new SOPS decryption
key outside the cluster.

## Recover lost secrets

Do not delete working state or generate another authority as a diagnostic step.

If the secrets file is missing, restore it at the path referenced by the
configuration. For legacy backups, `management identity import FILE` restores
the old workstation key store; `management identity export --config CONFIG`
then moves it beside the configuration.

If only the saved context remains, it can still authorize routine operations,
but cannot issue server certificates for a reinstall. Preserve it and the
existing nodes while locating the original secrets. A configuration bundle
contains neither the authority private key nor the operator key and cannot
recover them.

If both secrets and context are lost, Katl cannot recover trust through the
management API. Privileged SSH or console access is a separate recovery
boundary; there is currently no automated trust-recovery command.
