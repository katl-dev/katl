# Upgrade a KatlOS Host

KatlOS host upgrades are one-node-at-a-time operations. The normal command
resolves a release, stages its root and UKI into the inactive slot, reboots into
a bounded trial, and waits for boot health. It does not upgrade Kubernetes or
orchestrate availability across several hosts.

## Preconditions

- the node is healthy on a known-good generation;
- no other mutating node operation is active;
- the selected upgrade SquashFS is from the intended KatlOS release;
- the upgrade declares a compatible architecture and runtime interface;
- the node can fetch release artifacts from GitHub;
- the current workstation context contains the node and its enrolled agent
  credential;
- the command is run during the intended reboot window; and
- Kubernetes and workload availability have been handled outside Katl.

## Plan

```sh
katlctl node upgrade v2026.7.0-alpha.9 --node cp-1 --plan
```

A plan response has no durable mutation and does not reboot the node.

During staging, the node downloads or opens the image, calculates its SHA-256
and size, records that resolved identity in the operation, and checks the image's
component metadata before changing the inactive slot.

## Upgrade

Run the command without `--plan`:

```sh
katlctl node upgrade v2026.7.0-alpha.9 --node cp-1
```

`katlctl` follows staging progress, asks the authenticated node agent to reboot,
waits for the agent to restart, and requires the selected generation to be
committed, booted, and healthy. A successful JSON result has `rebooted: true`
and `bootHealth: healthy`. Check workload availability before upgrading another
host.

## Failure Boundary

Boot health may select the previous known-good host generation. `katlctl`
reports that rollback and stops. It does not
undo Kubernetes, etcd, workload, or external-infrastructure changes. If the
operation record says `recoveryRequired: true`, or the node fails to return,
stop the rollout and collect the evidence in [Troubleshoot KatlOS](troubleshoot.md).
