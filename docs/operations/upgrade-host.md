# Upgrade a KatlOS Host

KatlOS host upgrades are explicit, one-node-at-a-time, next-boot operations.
They stage a root and UKI into the inactive slot and arm a bounded trial
boot. They do not upgrade Kubernetes or orchestrate fleet availability.

## Preconditions

- the node is healthy on a known-good generation;
- no other mutating node operation is active;
- the selected upgrade SquashFS is from the intended KatlOS release;
- the upgrade declares a compatible architecture and runtime interface;
- the node can fetch the HTTPS image URL, or the image is already in its local
  artifact store;
- an operator controls the reboot window; and
- Kubernetes and workload availability have been handled outside Katl.

Choose the release and image name:

```sh
TAG=v2026.7.0-alpha.2
VERSION=${TAG#v}
IMAGE="katlos-upgrade-$VERSION-x86_64.squashfs"
```

## Plan

```sh
katlctl host upgrade \
  --plan \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --candidate-generation "katlos-$VERSION" \
  --client-request-id "cp-1-katlos-$VERSION" \
  --image-url "https://github.com/katl-dev/katl/releases/download/$TAG/$IMAGE"
```

A plan response has dry-run status and no durable mutation. Review the image,
candidate generation, resource locks, and refusal diagnostics.

During staging, the node downloads or opens the image, calculates its SHA-256
and size, records that resolved identity in the operation, and checks the image's
component metadata before changing the inactive slot.

## Stage the Trial

Run the identical command without `--plan`. Save the returned `operationId`.
The response means accepted, not staged successfully.

Follow the accepted operation through the node agent:

```sh
katlctl operation status \
  --endpoint cp-1.example.test:9443 \
  --agent-token-file ./tokens/cp-1.token \
  --operation-id "$OPERATION_ID" \
  --watch
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
