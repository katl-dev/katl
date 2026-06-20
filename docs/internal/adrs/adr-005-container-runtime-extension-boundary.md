# ADR-005: Container runtime is a versioned extension boundary

Status: accepted.

Date: 2026-06-20.

## Context

KatlOS is the OS that runs Kubernetes, but Kubernetes itself is delivered as a
versioned payload rather than being bundled into the install image. The same
extension model now exists for node extensions such as BIRD/BGP API VIP.

The remaining unclear boundary is the container runtime. Today the runtime root
contains containerd, an OCI runtime, CNI plugin binaries, and systemd wiring
that kubeadm and kubelet depend on. That is expedient, but it couples a
Kubernetes-version-sensitive service stack to the base OS and makes it harder to
offer users an explicit runtime version choice.

The north star is not an extension-only host. KatlOS still owns boot, update,
state, health, networking, recovery, and the generic activation machinery needed
to run Kubernetes. The question is whether the concrete container runtime
payload should be part of that base or selected like other versioned payloads.

## Decision

The concrete container runtime is a versioned extension boundary.

Container runtime payloads should be delivered as a named bundle, separate from
the base KatlOS runtime root. The bundle owns:

```text
containerd and its service unit or unit overlay
the selected OCI runtime, such as crun or runc
required CNI plugin binaries
runtime-specific tmpfiles, sysctl, modules-load, or drop-in fragments
runtime capability metadata consumed by Kubernetes/kubelet planning
runtime compatibility metadata consumed by generation activation
```

KatlOS base runtime owns the generic contract around that bundle:

```text
extension discovery, fetch, verification, staging, and rollback
generation records and activation ordering
persistent state projection for /var/lib/containerd or successor runtime state
generic runtime capability checks
boot health and repair behavior when the selected runtime bundle is missing or invalid
kubelet dependency wiring through declared runtime capabilities, not hardcoded package presence
```

The base runtime should not permanently install containerd, OCI runtime binaries,
or CNI plugin payloads as part of its supported rootfs contract. It may keep
generic units, targets, and validators that activate and check the selected
runtime bundle.

For v0.1, the implementation work must either move the runtime into such a
bundle or record a deliberately scoped temporary exception. A temporary exception
is acceptable only if it is tied to concrete release risk and names the exact
base-owned runtime files, units, state paths, checks, and VM gates that still
depend on the current layout. Existing implementation convenience is not enough
justification.

## Consequences

Kubernetes payloads must depend on declared runtime capabilities rather than
assuming a containerd package in the base rootfs.

Generated confext and systemd wiring must be able to express kubelet dependency
on the selected runtime bundle. If the v0.1 implementation keeps containerd in
base temporarily, the code and docs must mark that as an exception to this ADR,
not as the durable architecture.

Persistent runtime state is not part of the immutable bundle. KatlOS keeps
state under the writable state partitions and projects it into the runtime path
needed by the selected runtime. Runtime bundle upgrades and KatlOS generation
rollback must preserve that state unless an explicit destructive operation says
otherwise.

VM tests that run Kubernetes need a way to provide both Kubernetes and container
runtime payloads without relying on install-image embedding. A local HTTPS or
other explicit test bundle source is acceptable for vmtest, but the production
contract must remain explicit and verifiable.

The supported image surface must distinguish between generic KatlOS runtime
activation APIs and concrete container runtime payload ownership. Operators
should not treat `containerd` as a supported user container platform simply
because KatlOS can run Kubernetes.

## Follow-Up

The next work after this ADR is:

```text
update durable docs to cite this decision and remove contradictory base-runtime wording
scope the v0.1 implementation plan for a container-runtime bundle or temporary exception
update mkosi profiles, runtime checks, generation state, kubelet readiness, and VM gates accordingly
```
