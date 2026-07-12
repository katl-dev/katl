# KatlOS Operator Guide

These runbooks describe the implemented KatlOS alpha operating surface. Start
with the task that matches the current node state; do not skip directly to a
mutating command.

KatlOS is experimental. Read the [support boundary](../support.md) before using
these procedures. The current installer handoff and management API use bearer
authentication over unencrypted HTTP/TCP and are suitable only on isolated
evaluation networks.

## Lifecycle Map

| Current state | Operator goal | Runbook |
| --- | --- | --- |
| No KatlOS media downloaded | Authenticate a release | [Verify release artifacts](verify-release.md) |
| Bare or disposable machine | Install generation 0 | [Install KatlOS](../installing.md) |
| Generation 0 booted | Establish node management access | [Access installed nodes](access.md) |
| All intended nodes installed | Create the kubeadm cluster | [Bootstrap Kubernetes](bootstrap-kubernetes.md) |
| Installed or bootstrapped node | Change supported runtime configuration | [Apply node configuration](configure-nodes.md) |
| Healthy installed node | Stage a new KatlOS release | [Upgrade a KatlOS host](upgrade-host.md) |
| Cluster is intentionally being discarded | Reset boot state and reinstall | [Wipe and reinstall](wipe-reinstall.md) |
| A step failed or its state is unclear | Collect evidence and classify the failure | [Troubleshoot KatlOS](troubleshoot.md) |

## Operating Rules

The operator workstation needs the `katlctl` binary from the matching release,
`ssh`, `curl`, GNU `sha256sum`, and `jq`. Provenance verification additionally
uses GitHub CLI `gh`; cluster handoff checks use a `kubectl` version compatible
with the selected Kubernetes release. These tools run on the workstation, not
inside the KatlOS image.

Keep these artifacts together for the life of an evaluation:

- the exact KatlOS release URL, assets, checksums, and provenance result;
- the source `ClusterConfig` and compiled `.katlcfg` bundle;
- both the config bundle archive SHA-256 and its internal `bundleDigest`;
- the digest-pinned Kubernetes OCI reference;
- one protected agent token file per node;
- the kubeconfig, operation IDs, generation IDs, and relevant timestamps; and
- independent etcd, application, and persistent-data backups.

Treat command outcomes precisely:

- **validated/planned** means no operation was accepted;
- **accepted** means the node persisted an operation, not that it completed;
- **terminal succeeded** means the operation completed its current-boot work;
- **boot health passed** means a staged generation survived its trial boot; and
- **failed-needs-repair** means do not blindly retry or assume host rollback
  reverted Kubernetes state.

Use a unique, stable `--client-request-id` for each intended mutation. Reuse it
only when retrying the exact same request. Changing inputs requires a new ID.

For every accepted mutation, retain both `operationId` and `requestDigest`.
Use the generic status path for config apply, host upgrade, bootstrap, and
destructive reset:

```sh
katlctl operation status \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --operation-id "$OPERATION_ID" \
  --request-digest "$REQUEST_DIGEST" \
  --watch
```

Without `--watch`, the command returns one authoritative snapshot. With it,
progress is written to stderr and final structured status to stdout. A lost
watch automatically falls back to polling the durable node record. Use
`--diagnostics verbose` when the normal redacted status is insufficient. A
watched terminal failure still prints its final JSON status and exits nonzero.

## Boundaries That Matter During Operations

KatlOS generations own the immutable root, UKI, selected sysexts, and compiled
node configuration. They do not own or roll back etcd, kubeadm mutations,
Kubernetes API objects, CNI state, persistent volumes, or workloads.

Normal configuration apply currently covers hostname, SSH authorized keys, and
systemd-networkd files. Disk policy, system role, Kubernetes bundle selection,
and kubeadm lifecycle changes require a named lifecycle operation or reinstall.

There is no supported alpha workflow for automatic fleet rollout, Kubernetes
version upgrades, etcd disaster recovery, failed control-plane replacement,
agent-token rotation, or general cluster reconciliation.
