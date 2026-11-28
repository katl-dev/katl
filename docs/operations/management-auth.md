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

Choose the initial mode before installation.

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
configuration stating neither, and prints the line to add. Routine commands
use the configured mode.
A failed TLS handshake never falls back to trusted-network access.
During a management trust change, connection errors and expired deadlines
leave trust unconfirmed. The command stops and gives a retry or status
command. Rotation can confirm old-authority removal through the node's
explicit TLS issuer rejection.

Older mTLS configurations and nodes keep mTLS: an
existing configuration with a secrets reference and no explicit mode uses
mTLS, and installed nodes without a mode record keep mTLS after an OS upgrade.
An mTLS configuration without `spec.managementIdentity` must say
`managementAuthentication: mtls` explicitly; it then takes its credentials
from a saved context, so run `katlctl context save --config ./cluster.yaml`
before other commands.
A new trusted-network installation requires a release that supports this
setting on both the installer and the installed runtime.

<a id="change-the-access-mode"></a>

## Migrate to mTLS management

Start with an installed cluster whose `cluster.yaml` specifies
`spec.managementAuthentication: trusted-network`. Upgrade every node to a
KatlOS release that supports the migration, and use a version of `katlctl` that
provides `management enable-mtls`. Check that each node's `management.address`
is reachable from your workstation and finish any active node operations.
Use the migration command to change the mode on running nodes. Editing the
configuration or applying a generation cannot make that change.

1. Run the migration from your workstation.

   ```sh
   katlctl management enable-mtls --config ./cluster.yaml
   ```

   Preflight checks every node before creating secrets or starting a trial.
   If the command reports `preflight failed; no node was changed`, resolve
   the reported problem and rerun. A trusted-network node fails preflight if
   its clock is more than five minutes behind the workstation, since it can
   reject the new certificates as not yet valid. Correct the node's time source
   and rerun. If preflight reports `could not confirm the node accepts the
   replacement authority` or `could not confirm the node uses trusted-network
   management`, follow the next step the message gives. If it adds `the node
   did not answer`, the node is down or still booting; rerun once `katlctl node
   status` shows it up. If the node answered with a certificate from another
   authority, the message gives the authority remedy that `katlctl node status`
   gives. Otherwise check the address and node reachability, then rerun the
   command shown in the message.

2. Back up the secrets file as soon as the command reports its path. By default
   the command creates or reuses `management-secrets.yaml` beside `cluster.yaml`.
   Keep this same file throughout the migration.

   The command moves nodes one at a time in configuration order. For each node,
   it starts a trial, checks that the node answers with the new credentials,
   and then confirms the change over mTLS. Before moving on, it checks over
   mTLS that the same installation reports the replacement authority as
   committed and is healthy. The first of three nodes might report these lines.

   ```text
   cp-1: serving the new authority on trial; it returns to trusted-network by 14:05:00 unless confirmed
   cp-1: mTLS confirmed (1/3)
   ```

   Each trial lasts up to five minutes by default. Set `--trial-window` to
   change that per-node window. It accepts whole seconds from `30s` through
   `30m`, inclusive.

3. After every node is confirmed and passes its checks, the command sets
   `spec.managementAuthentication: mtls` in `cluster.yaml` and sets
   `spec.managementIdentity` to the secrets path relative to `cluster.yaml`.
   When it refreshes the saved context for a cluster named `lab`, it prints
   `Refreshed the saved context for lab.` If it reports `the saved context could
   not be refreshed:`, the nodes and configuration have already moved to mTLS.
   Resolve the reported error and run the `katlctl context save --config` command
   shown in the message. If you have no saved context for the cluster, a successful
   migration prints a `katlctl context save` command you can run to create one.
   Check that every node shows `mtls` in the `MANAGEMENT` column.

   ```sh
   katlctl cluster status --config ./cluster.yaml
   ```

### Check and resume an interrupted migration

If Ctrl-C, a closed terminal, or a lost connection interrupts the command,
a node with an unconfirmed trial reverts to trusted-network at its deadline.
Nodes that already confirmed mTLS keep it, even if the connection dropped
before the confirmation reply reached your workstation.

If the command reports `could not confirm the mTLS trial started; the node may
still trust the trusted network` or `could not confirm the node accepts the
replacement authority; the node may still trust the trusted network`, rerun
the command shown in the message with the same secrets. If it reports
`could not confirm mTLS confirmation; the node may still trust the replacement
authority`, use the status command it prints. The rollout stops at the
affected node and leaves the configuration and saved context as they were.
If verifying the saved context fails after the nodes confirm mTLS, the
configuration and saved context stay as they were until you rerun successfully.

Check the cluster or an individual node using the same configuration.

```sh
katlctl cluster status --config ./cluster.yaml
katlctl node status cp-1 --config ./cluster.yaml
```

When the endpoint closes a trusted-network status connection without answering
the initial status request within its deadline, these commands retry over mTLS.
If the request expires, status reports `management RPC outcome is inconclusive`
and skips the mTLS retry. When retrying over mTLS, the commands look for
secrets at the path in `spec.managementIdentity`, or at `management-secrets.yaml`
beside the config when it has no reference. A live trial shows
`trusted-network (mtls trial)` in the `MANAGEMENT` column and a detail such as
`management: mTLS trial until 14:05:00` in local time. A confirmed node shows
`mtls`. After a revert, the node shows `trusted-network` and reports the reason
after `management: last mTLS trial reverted:`. If the status command cannot
find the secrets, it reports the connection failure and says
`the node may be on an interrupted enable-mtls trial`. Status discovers a
custom `--output` path only if it matches the path status looks for.

Use `katlctl node status cp-1 --config ./cluster.yaml --output json` to
inspect `management.servedClientTrust.authorities`, the CA fingerprints the
answering listener trusts. A plaintext listener reports an empty list. During
a live trial, `servedClientTrust` also identifies the authority and trial
that the listener serves. Older agents omit `servedClientTrust`.

Rerun the migration with the same config and secrets file. If you used
`--output` to select the file, pass the same path. The command resumes a
reachable trial before its deadline and starts a fresh trial after a revert.
It rechecks nodes that already confirmed the same authority without starting
another trial on them. If the node reports a live trial past its deadline,
wait for it to return to trusted-network before rerunning.

### Cancel a live trial

To deliberately cancel an unconfirmed trial, run this as root on the node's
console.

```sh
katlc agent management-trial revert
```

### If you lose the secrets during migration

Restore the original file from backup at the path the migration used and rerun
the command. If you have no backup and every node still uses trusted-network
or returns to it when its trial ends, rerun once all live trials have reverted.
The command can then create a new secrets file.

A confirmed node keeps trusting the authority from the lost file. The console
revert applies only to a live trial, and a newly generated secrets file cannot
authenticate to a confirmed node. If every node has confirmed mTLS and your
release supports SSH recovery, run
`katlctl management identity recover --config ./cluster.yaml` to replace the
lost authority over host-key-verified root SSH. If the migration stopped
before updating the config, set `spec.managementAuthentication: mtls` first.
See [Recover lost secrets](#recover-lost-secrets) for the prerequisites.

The recovery command requires an mTLS config and mTLS on every node it lists.
If some nodes still use trusted-network, a config listing the whole cluster
fails recovery preflight even if its mode is set to mTLS.

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
and uses the replacement credentials to verify that each node reports only
the new authority. If a node answers status but omits its trust report, the
command also presents a fresh client certificate signed by the old authority
and requires a timely TLS `unknown_ca` alert from the authenticated node
on that probe's connection.
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
pass. If an agent fails to reload its credentials, use root SSH to inspect
`katlc-agent.service` and its journal, repair that failure, and rerun. Do not
generate another replacement during recovery.

If rotation reports `could not confirm the node stopped trusting the old
authority; the node may still trust the old authority` or `old authority still
has management access`, check the affected agent's client CA trust and journal
over root SSH. Check that the management address reaches that agent directly or
through TLS passthrough, then rerun with the same `--output`.

If a TLS failure, lost connection, or expired deadline prevents verification
with the replacement credentials, rotation reports `could not confirm the
node accepts the replacement authority; the node may still trust the old
authority`. When `cluster.yaml` already references the replacement, it
reports `could not confirm the node accepts the configured authority; the node
may still trust the previous authority`. Keep the replacement file, check
the endpoint, and rerun the same command.

Rotation and recovery stop preflight when an SSH result arrives after
`--timeout`. If the command reports that it could not confirm preflight on a
node, check SSH access to that node and rerun the same command. Keep any
replacement secrets the command has already written.

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

If the secrets file is lost and no backup remains, replace the authority the
nodes trust with a new one over root SSH:

```sh
katlctl management identity recover --config ./cluster.yaml
```

Before running it, make sure that:

1. Every node runs a KatlOS release that supports `katlc agent
   management-authority`.
2. Root SSH works to each management address with a trusted, recorded host
   key, as for [rotation](#rotate-a-management-authority). Pass
   `--ssh-key` and `--ssh-known-hosts` when the defaults do not apply. An
   unknown host key is refused, and with `--ssh-known-hosts` only that file is
   trusted. Neither the installer nor `katlctl install apply` prints host keys,
   so record them yourself, preferably at install time: run
   `ssh-keyscan -t ed25519 <node>` over a network path you trust, and compare
   each fingerprint (`ssh-keygen -lf <file>`) with the node's own, read on the
   console or an existing SSH session with
   `ssh-keygen -lf /var/lib/katl/ssh/host-keys/ssh_host_ed25519_key.pub`. Keep
   the verified keys in the known-hosts file for later recovery. Do not take
   keys from a first, unverified connection.
3. No node operation is active.

The command reads the authority each node trusts, and changes nothing until
every node has been reached. It then writes the new secrets to the path that
`spec.managementIdentity` references, or to `--output` (a `.sops.yaml` name
encrypts it first), installs the new authority on each node, and checks that
the new credentials manage the node. When the node reports its listener's
trusted CAs, recovery requires the replacement authority to be the only one
in that set. Nodes stay in mTLS mode throughout. Back up the new secrets file, then
refresh saved contexts with `katlctl context save --config ./cluster.yaml`.

If recovery reports `could not confirm the node accepts the replacement
authority; the node may still trust the previous authority`, keep the
replacement file and rerun the same command. Recovery verifies access to each
node before continuing, including nodes that already report the replacement
authority over SSH. A node whose recovery stopped after it installed the
replacement but before its agent reloaded, including a node that still runs
v2026.11.5, is reloaded with the replacement it already holds; the rerun
prints `Reloading the installed management authority on <node>` and creates
no other authority. The reload refuses while a node operation is active, so
wait for that operation to finish and rerun. Recovery updates the
configuration only after all nodes pass.
To recover with a different authority, move the new secrets file aside first.
A file that already exists at the target path is used as the replacement.

If SSH to a node is unavailable, no remote path can recover it, because the
management API requires the lost credentials. Use the node console to restore
SSH access, then rerun the command.
