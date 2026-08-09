# Troubleshoot KatlOS

Troubleshooting starts by identifying the lifecycle boundary that failed. Do
not retry a mutating command until its durable state and mutation boundary are
known.

On an attached VGA or ipKVM console, `tty1` is the KatlOS status dashboard and
includes current network addresses plus a live journal tail. Use
`Ctrl+Alt+F2` for a local shell. The last rendered dashboard is also available
at `/run/katl/console/rendered.txt` for collection over SSH.

## First Classification

| Symptom | Primary evidence |
| --- | --- |
| Installer never becomes ready | installer console; `katlos-install.service` journal |
| Config bundle rejected | bundle command output; selected node; validation error |
| Installed node does not complete boot | boot console; boot-health and handoff services |
| Agent cannot be reached | saved management identity/context; expected node name; `katlc-agent.service`; `katlc-management-firewall.service`; network path to TCP 9443 |
| Bootstrap or join fails | `katlctl` phase output; node operation record; kubelet/containerd/kubeadm journals |
| Config apply stalls or rolls back | `katlctl node status` and `katlctl operations list`; generation and operation records |
| Routed API endpoint is unavailable | `katlctl cluster status`; per-node local API, route, peer, and exchange state |
| Host upgrade does not stage or boot | host-upgrade operation; boot selection; boot-health journal |
| Wipe is refused | wipe JSON refusals; selected topology; Kubernetes cleanup diagnostics |

## Collect Installed-Node Evidence

Run on the affected node and preserve timestamps:

```sh
systemctl --failed --no-pager
systemctl status katl-boot-complete.target katl-boot-health.service --no-pager
systemctl status katl-runtime-handoff-status.service katlc-agent.service --no-pager
journalctl -b --no-pager
journalctl -b -u katl-boot-health.service -u katlc-agent.service --no-pager
cat /var/lib/katl/boot/selection.json
find /var/lib/katl/generations -maxdepth 2 -type f -print
find /var/lib/katl/operations -maxdepth 3 -type f -print
```

First discover current and recent durable operations through the agent:

```sh
katlctl operations list \
  --endpoint affected-node.example.test:9443
```

Use `katlctl operations status ID --diagnostics verbose` when an
exact record needs deeper inspection.

If the agent is unavailable, use SSH as a break-glass evidence path and read
the record without editing it. The journal directory is authoritative when a
snapshot looks stale:

```text
/var/lib/katl/operations/$OPERATION_ID/record.json
/var/lib/katl/operations/$OPERATION_ID/journal/
```

Do not edit operation, generation, or boot-selection records as a repair method.

## Installer Evidence

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

Also retain the installer console, exact release filename and SHA-256, config
bundle, selected node, and disk
identity. A failure before validation completes must not repartition the disk;
record disk state before attempting anything else.

## Interpret Operation State

- `terminal: false`: the operation may still be running or interrupted. Check
  the latest journal event and agent service before acting.
- `result: succeeded`: the operation completed its current phase; a staged
  generation may still need reboot and boot-health promotion.
- `result: failed-needs-repair` or `recoveryRequired: true`: preserve evidence
  and stop automatic retry.
- `externalMutationStarted: true`: assume the named mutation scopes may have
  changed even if the command returned an error.
- lost client connection: does not cancel the node-local operation.

Host rollback changes KatlOS artifacts around persistent state. It does not
prove kubeadm, etcd, Kubernetes API, CNI, or workload mutations were reverted.

## Agent Access Failures

Confirm the service and listener on the isolated management network:

```sh
systemctl status katlc-agent.service --no-pager
systemctl status katlc-management-firewall.service --no-pager
ss -lntp | grep ':9443'
nft list table inet katl_management
```

An error about missing management access means this workstation does not have
the cluster identity or a saved authenticated context. Restore the backup with
`katlctl management identity import IDENTITY`, then repeat `katlctl context save
--config cluster.yaml`. Do not create a new identity for already-installed
nodes: they will correctly reject it.

A certificate-name failure usually means the address answered as another node.
Do not override verification. Correct the address or use `katlctl context rebind
--node NODE --endpoint ADDRESS`, which verifies both TLS and enrollment before
saving it. Do not solve reachability by exposing port `9443` to an untrusted
network or disabling the firewall service.

## Reporting

Follow the [support-boundary reporting checklist](../support.md#reporting-a-problem).
Redact bearer tokens, private keys, kubeconfigs, join commands, certificate
material, registry credentials, and workload secrets. Use GitHub private
vulnerability reporting for security-sensitive findings.
