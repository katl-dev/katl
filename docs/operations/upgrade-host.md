# Upgrade a KatlOS Host

KatlOS host upgrades are explicit, one-node-at-a-time, next-boot operations.
They stage a verified root and UKI into the inactive slot and arm a bounded trial
boot. They do not upgrade Kubernetes or orchestrate fleet availability.

## Preconditions

- the node is healthy on a known-good generation;
- no other mutating node operation is active;
- the exact upgrade SquashFS, metadata, checksum, and provenance are verified;
- the upgrade declares a compatible architecture and runtime interface;
- the node can fetch the HTTPS image URL, or the image is already in its local
  artifact store;
- an operator controls the reboot window; and
- Kubernetes and workload availability have been handled outside Katl.

Follow [Verify release artifacts](verify-release.md) first. Record the exact
image SHA-256 and size:

```sh
TAG=v2026.7.0-alpha.2
VERSION=${TAG#v}
IMAGE="katlos-upgrade-$VERSION-x86_64.squashfs"
IMAGE_SHA256=$(sha256sum "$IMAGE" | awk '{print $1}')
IMAGE_SIZE=$(stat -c %s "$IMAGE")
```

## Plan

```sh
katlctl host upgrade \
  --plan \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --candidate-generation "katlos-$VERSION" \
  --client-request-id "cp-1-katlos-$VERSION" \
  --image-url "https://github.com/katl-dev/katl/releases/download/$TAG/$IMAGE" \
  --image-sha256 "$IMAGE_SHA256" \
  --image-size-bytes "$IMAGE_SIZE"
```

A plan response has dry-run status and no durable mutation. Review the image,
candidate generation, resource locks, and refusal diagnostics.

## Stage the Trial

Run the identical command without `--plan`. Save the returned `operationId` and
`requestDigest`. The response means accepted, not staged successfully.

Until a general remote operation-status command is exposed, inspect the durable
record on the node over SSH:

```sh
OPERATION_ID=host-upgrade-...
ssh root@cp-1.example.test \
  "cat '/var/lib/katl/operations/$OPERATION_ID/record.json'" | \
  jq '.payload.record | {operationID, operationKind, phase, terminal, result, candidateGenerationID, bootHealthPending, recoveryRequired, failureReason, nextAction}'
```

Do not reboot until the operation is terminal with `result: succeeded` and
`nextAction` says to reboot into the bounded candidate trial. A terminal staging
success still has `bootHealthPending: true`.

## Reboot and Verify

Reboot one node during the controlled window:

```sh
ssh root@cp-1.example.test systemctl reboot
```

After it returns:

```sh
ssh root@cp-1.example.test systemctl is-active katl-boot-complete.target
ssh root@cp-1.example.test systemctl status katl-boot-health.service --no-pager
ssh root@cp-1.example.test cat /var/lib/katl/boot/selection.json
```

Require the candidate generation to be the booted/default generation with no
pending health validation before moving to another node. Also verify kubelet,
the Kubernetes Node, and workload availability.

## Failure Boundary

Boot health may select the previous known-good host generation. That does not
undo Kubernetes, etcd, workload, or external-infrastructure changes. If the
operation record says `recoveryRequired: true`, or the node fails to return,
stop the rollout and collect the evidence in [Troubleshoot KatlOS](troubleshoot.md).
