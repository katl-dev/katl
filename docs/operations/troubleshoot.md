# Troubleshoot KatlOS

Use node status, logs, and operation records to find which step failed and
whether it changed the node. Check the operation state before you retry.

## First classification

| Symptom | Primary evidence |
| --- | --- |
| Installer never becomes ready | installer console; `katlos-install.service` journal |
| Configuration bundle rejected | `katlctl config bundle` output; selected node; validation error |
| Installed node does not complete boot | boot console; boot-health and management services |
| Agent cannot be reached | saved context or mTLS management secrets; expected node name; `katlc-agent.service`; network path to TCP 9443 |
| Bootstrap or join fails | `katlctl` phase output; node operation record; kubelet/containerd/kubeadm journals |
| Config apply stalls or rolls back | `katlctl node status` and `katlctl operations list`; generation and operation records |
| Routed API endpoint is unavailable | `katlctl cluster status`; per-node local API, route, peer, and exchange state |
| Host upgrade does not stage or boot | host-upgrade operation; boot selection; boot-health journal |
| Wipe is refused | wipe JSON refusals; selected nodes; Kubernetes cleanup diagnostics |

## Check status from your workstation

When the management API is reachable, start from your workstation:

```sh
katlctl cluster status --config ./cluster.yaml
katlctl node logs cp-1 --config ./cluster.yaml -u katlc-agent.service -u kubelet
katlctl node logs cp-1 --config ./cluster.yaml --boot=-1 --lines 200
katlctl operations status --config ./cluster.yaml --node cp-1 --watch
```

Use `--follow` to stream new journal entries and Ctrl+C to stop. JSON logs use
one native journal object per line. SSH and console access remain available
when the agent or network is unavailable.

On an attached VGA or ipKVM console, `tty1` is the KatlOS status dashboard and
includes current network addresses plus a live journal tail. Use
`Ctrl+Alt+F2` for a local shell. On installer media the dashboard also shows what the installer is waiting
for or why it stopped. The last rendered dashboard is also available at
`/run/katl/console/rendered.txt` for collection over SSH.

## Collect installed-node evidence

First discover current and recent durable operations through the agent:

```sh
katlctl operations list --config ./cluster.yaml --node cp-1
```

Add `--endpoint ADDRESS` to override the node's address from `cluster.yaml`.
Use `katlctl operations status ID --diagnostics verbose` when an exact record
needs deeper inspection.

Then run these commands on the affected node and preserve timestamps:

```sh
systemctl --failed --no-pager
systemctl status katl-boot-complete.target katl-boot-health.service katlc-agent.service --no-pager
systemctl status katl-boot-recovery.service katl-boot-deadman.timer --no-pager
journalctl -b --no-pager
journalctl -b -u katl-boot-health.service -u katl-boot-recovery.service -u katlc-agent.service --no-pager
journalctl -b -1 -u katl-boot-health.service -u katl-boot-recovery.service --no-pager
cat /var/lib/katl/boot/selection.json
find /var/lib/katl/generations -maxdepth 2 -type f -print
find /var/lib/katl/operations -maxdepth 3 -type f -print
```

If the agent is unavailable, use SSH as a break-glass evidence path and read
the record without editing it. The journal directory is authoritative when a
snapshot looks stale:

```text
/var/lib/katl/operations/$OPERATION_ID/record.json
/var/lib/katl/operations/$OPERATION_ID/journal/
```

Do not edit operation, generation, or boot-selection records as a repair method.

After a failed trial, the previous-boot journal shows why boot health failed and
whether `katl-boot-recovery.service` requested the single automatic fallback
reboot. If the fallback also failed, or `recoveryRequired` is true, Katl does not
reboot again. Preserve the journal and use the console to select or repair a
healthy generation. If no healthy generation remains, reinstall the node
without treating host rollback as a Kubernetes or data backup.

## Installer evidence

While the installer is still waiting for configuration, enable its ephemeral
key-only SSH access without starting an install:

```sh
katlctl install ssh --config ./cluster.yaml --node cp-1
ssh root@192.0.2.11
```

The command uses only the selected node's configured public keys. From the
installer environment collect:

```sh
journalctl -b -u katlos-install.service --no-pager
find /var/lib/katl/install -maxdepth 2 -type f -print
```

Also retain the installer console, exact release filename and SHA-256,
configuration bundle, selected node, and disk identity. A failure before
validation completes must leave the partition table intact. Record the state of
the disk before attempting recovery.

| Symptom | What to inspect |
| --- | --- |
| Configuration rejected | Run `katlctl config validate ./cluster.yaml` and review the selected node, SSH key, and disk. For PXE, check that the configuration bundle was compiled with the intended image URL and descriptor. |
| Disk missing or ambiguous | Compare the installer disk inventory with the stable ID in the configuration. If the disk is absent, check the firmware and storage controller. |
| Automatic install refused | Check whether the PXE profile enabled automatic install and whether the installed-disk guard refused it. See [Keep PXE first safely](../install-pxe-matchbox.md#keep-pxe-first-safely). |
| Image digest mismatch | Use the image and its published descriptor from the same release. Replace any altered copy with the published file. |

## Interpret operation state

| State | Meaning and action |
| --- | --- |
| `terminal: false` | The operation may still be running or interrupted. Check the latest journal event and agent service before acting. |
| `result: succeeded` | The operation completed its current phase; a staged generation may still need reboot and boot-health promotion. |
| `result: failed-needs-repair` or `recoveryRequired: true` | Preserve evidence and stop automatic retry. |
| `externalMutationStarted: true` | Assume the named mutation scopes may have changed even if the command returned an error. |
| Lost client connection | Does not cancel the node-local operation. |

A successful host rollback does not prove that kubeadm, etcd, Kubernetes API,
CNI, or workload mutations were reverted; see
[rollback is not a backup](../concepts/generations.md#rollback-is-not-a-backup).

## Agent access failures

Confirm the service and listener on the isolated management network:

```sh
systemctl status katlc-agent.service --no-pager
ss -lntp | grep ':9443'
```

For missing mTLS management secrets, restore the original file at
`spec.managementIdentity` (relative to the cluster configuration), including
access to its SOPS decryption key if encrypted. Do not generate new secrets for
already-installed nodes: a new authority does not restore their trust.
Deleting the workstation context cannot reset the nodes and is unnecessary; rebuild
it with `katlctl context save --config PATH_TO_YOUR_CONFIG`. See
[Recover lost secrets](management-auth.md#recover-lost-secrets) for legacy migration
and the limits of lost-key recovery.

A trusted reinstall using the same secrets is accepted automatically. Katl
still refuses a node that changes installation during an operation. Inspect
that node before retrying the command.

A certificate-name failure usually means the address answered as another node.
Do not override verification. Correct the address or use `katlctl context rebind
--node NODE --endpoint ADDRESS`, which verifies both TLS and enrollment before
saving it. Do not solve reachability by exposing port `9443` to an untrusted
network or disabling TLS verification.

## Required link reconfigured

If systemd-networkd reports no carrier on a required link for 20 seconds while
sysfs reports carrier up (`carrier=1`, `operstate` up), the link gets no DHCP
lease. KatlOS then reconfigures that link once per boot, and the boot-health
diagnostic in `katlctl node status` reports the attempt for the rest of the
boot, for example:

```text
Required link enp1s0 was reconfigured once in this boot after systemd-networkd reported no carrier while sysfs reported carrier up. No action is needed while the node is healthy
```

If the node is healthy, no action is needed. If the line says the link could
not be reconfigured, or that the result was not recorded, read the
link-recovery journal:

```sh
katlctl node logs cp-1 --config ./cluster.yaml -u katl-link-recovery.service
```

If the agent is unreachable, run `journalctl -b -u katl-link-recovery` on the
console instead. If the link stays offline, reboot the node with
`katlctl node reboot cp-1 --config ./cluster.yaml`, or from the console when
the link carries the only management path. KatlOS makes no second attempt in
the same boot.

## Reporting

Follow the [support-boundary reporting checklist](../support.md#report-a-problem).
Redact bearer tokens, private keys, kubeconfigs, join commands, certificate
material, registry credentials, and workload secrets. Use GitHub private
vulnerability reporting for security-sensitive findings.
