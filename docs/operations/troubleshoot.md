# Troubleshoot KatlOS

Troubleshooting starts by identifying the lifecycle boundary that failed. Do
not retry a mutating command until its durable state and mutation boundary are
known.

## First Classification

| Symptom | Primary evidence |
| --- | --- |
| Installer never becomes ready | installer console; `katlos-install.service` journal |
| Config bundle rejected | bundle command output; selected node; both bundle digests |
| Installed node does not complete boot | boot console; boot-health and handoff services |
| Agent cannot be reached | network path to TCP 9443; `katlc-agent.service`; token file mapping |
| Bootstrap or join fails | `katlctl` phase output; node operation record; kubelet/containerd/kubeadm journals |
| Config apply stalls or rolls back | `katlctl config apply status`; generation and operation records |
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

For one accepted operation, stream its durable record to `jq` on the operator
workstation:

```sh
OPERATION_ID=operation-id-from-katlctl
ssh root@affected-node \
  "cat '/var/lib/katl/operations/$OPERATION_ID/record.json'" | \
  jq '.payload.record | {
  operationID, operationKind, requestDigest, phase, completedPhases,
  terminal, result, externalMutationStarted, mutationScopes,
  recoveryRequired, failureReason, nextAction, diagnosticArtifacts
}'
```

The journal directory is authoritative when a snapshot looks stale:

```text
/var/lib/katl/operations/$OPERATION_ID/journal/
```

Do not edit operation, generation, or boot-selection records as a repair method.

## Installer Evidence

From the installer environment collect:

```sh
journalctl -b -u katlos-install.service --no-pager
find /var/lib/katl/install -maxdepth 2 -type f -print
```

Also retain the installer console, exact release filename and SHA-256, config
bundle, selected node, archive SHA-256, internal `bundleDigest`, and disk
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
ss -lntp | grep ':9443'
stat -c '%a %U:%G %n' /var/lib/katl/agent/token
```

Then confirm the workstation is using the token from that exact node. Do not
print the token into diagnostics. The alpha agent transport is unencrypted; do
not solve reachability by exposing port `9443` to an untrusted network.

## Reporting

Follow the [support-boundary reporting checklist](../support.md#reporting-a-problem).
Redact bearer tokens, private keys, kubeconfigs, join commands, certificate
material, registry credentials, and workload secrets. Use GitHub private
vulnerability reporting for security-sensitive findings.
