# KatlOS Operator Guide

These runbooks describe the implemented KatlOS alpha operating surface. Start
with the task that matches the current node state; do not skip directly to a
mutating command.

KatlOS is experimental. Read the [support boundary](../support.md) before using
these procedures. The ephemeral installer handoff is intentionally
unauthenticated HTTP, while the installed-node management API uses bearer
authentication over unencrypted TCP. Keep both on isolated evaluation
networks.

## Lifecycle Map

| Current state | Operator goal | Runbook |
| --- | --- | --- |
| No KatlOS media downloaded | Download a release | [Install KatlOS](../installing.md) |
| Bare or disposable machine | Install generation 0 | [Install KatlOS](../installing.md) |
| Generation 0 booted | Establish node management access | [Access installed nodes](access.md) |
| All intended nodes installed | Create the kubeadm cluster | [Bootstrap Kubernetes](bootstrap-kubernetes.md) |
| Installed node | Inspect or reboot one host | [Access installed nodes](access.md#routine-host-management) |
| Installed or bootstrapped node | Change supported runtime configuration | [Apply node configuration](configure-nodes.md) |
| Healthy installed node | Stage a new KatlOS release | [Upgrade a KatlOS host](upgrade-host.md) |
| Cluster is intentionally being discarded | Reset boot state and reinstall | [Wipe and reinstall](wipe-reinstall.md) |
| A step failed or its state is unclear | Collect evidence and classify the failure | [Troubleshoot KatlOS](troubleshoot.md) |

## Operating Rules

The operator workstation needs the `katlctl` binary from the matching release,
`ssh`, `curl`, and `jq`. Optional checksum and provenance inspection uses GNU
`sha256sum` and GitHub CLI `gh`; cluster handoff checks use a `kubectl` version
compatible with the selected Kubernetes release. These tools run on the
workstation, not inside the KatlOS image.

Keep these artifacts together for the life of an evaluation:

- the KatlOS release URL and assets used;
- the source `ClusterConfig` and any `.katlcfg` produced for PXE or offline use;
- the Kubernetes OCI reference;
- one enrolled workstation context with a protected agent token per node;
- the kubeconfig, command results, generation IDs, and relevant timestamps; and
- independent etcd, application, and persistent-data backups.

Treat command outcomes precisely:

- **validated/planned** means no operation was accepted;
- **accepted** means the node persisted an operation, not that it completed;
- **terminal succeeded** means the operation completed its current-boot work;
- **boot health passed** means a staged generation survived its trial boot; and
- **failed-needs-repair** means do not blindly retry or assume host rollback
  reverted Kubernetes state.

`katlctl` generates mutation idempotency keys and follows durable operations to
their terminal result. Operators do not need to create request IDs or retain
operation IDs. Progress is written to stderr and final structured status to
stdout.

Use `--no-wait` only when intentionally detaching a command. Discover current
or recent node work later with:

```sh
katlctl operations list \
  --node cp-1
```

`katlctl operation status --operation-id ID` remains an advanced diagnostic
path for one exact record. Use `--diagnostics verbose` when normal redacted
status is insufficient.

## Boundaries That Matter During Operations

KatlOS generations own the immutable root, UKI, selected sysexts, and compiled
node configuration. They do not own or roll back etcd, kubeadm mutations,
Kubernetes API objects, CNI state, persistent volumes, or workloads.

Normal configuration apply covers SSH authorized keys and systemd-networkd
files. It also carries operation-only system-role and kubeadm profile changes to
the planner so they produce an explicit lifecycle action instead of being
silently ignored. Disk policy and Kubernetes version selection use their named
install or upgrade workflows.

There is no supported alpha workflow for automatic host fleet rollout, etcd
disaster recovery, failed control-plane replacement, agent-token rotation, or
general cluster reconciliation.
