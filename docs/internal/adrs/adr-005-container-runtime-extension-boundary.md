# ADR-005: Container runtime stays in the base runtime

Status: accepted.

Date: 2026-06-20.

## Context

KatlOS is the OS that runs Kubernetes, but Kubernetes itself is delivered as a
versioned payload rather than being bundled into the install image. The same
extension model now exists for node extensions such as BIRD/BGP API VIP.

The remaining unclear boundary is the container runtime. The runtime root
already contains containerd, an OCI runtime, state directories, and systemd
wiring that kubeadm and kubelet depend on. Moving that stack into a separate
extension would let users select a runtime version, but it would also make every
Kubernetes boot, repair, rollback, and readiness path depend on an additional
payload before kubelet can even start safely.

That is not the right v0.1 split. KatlOS is not a generic extension-only host;
it is an OS for running Kubernetes. Users should need to select and install
cluster add-ons such as Cilium, Calico, storage, ingress, and GitOps after
bootstrap, but they should not need to assemble the CRI runtime needed for
Kubernetes to run at all.

## Decision

The concrete CRI runtime stack stays in the KatlOS base runtime.

KatlOS base runtime owns:

```text
containerd
the selected OCI runtime, such as crun or runc
containerd service configuration and Katl-owned ordering/drop-ins
persistent state projection for /var/lib/containerd
kubelet dependency wiring on the local CRI runtime
runtime health, repair, image-surface, and VM-test checks
```

KatlOS selects supported upstream containerd and crun releases independently of
the Fedora package update cadence. Katl builds RPMs from pinned sources against
the selected Fedora base and installs those exact packages into the immutable
runtime. Fedora supplies the host libraries. Containerd and its shim come from
the same source release; crun retains systemd cgroup and seccomp support.

Runtime updates must pass KatlOS image checks and the relevant Kubernetes and
lifecycle VM gates. Select containerd versions compatible with the supported
Kubernetes payloads. Validate rollback against persistent containerd state;
changing the immutable root does not restore an older copy of that state.

KatlOS does not own the production cluster CNI choice. Users install the CNI
implementation they choose, such as Cilium or Calico, after bootstrap. KatlOS may
own the generic CNI directory/configuration contract needed by the CRI runtime
and may carry minimal generic CNI tooling when required for Katl-owned tests or
bootstrap fixtures, but KatlOS must not present that as the managed cluster CNI
or require users to rebuild KatlOS to choose a CNI.

## Consequences

Kubernetes payloads can assume KatlOS provides a supported local CRI runtime, but
must still validate compatibility through runtime metadata and image-surface
checks.

Generated confext and systemd wiring may depend on `containerd.service` as a
KatlOS base service. This dependency is part of the OS contract, not an
extension payload contract.

Persistent runtime state remains writable state, not immutable rootfs content.
KatlOS keeps state under the writable state partitions and projects it into
`/var/lib/containerd`. KatlOS generation rollback must preserve that state unless
an explicit destructive operation says otherwise.

VM tests that run Kubernetes do not need a separate container-runtime bundle
source. They must still prove that the base runtime provides the expected CRI
runtime and that Kubernetes payloads are fetched separately from the install
image.

The supported image surface must distinguish between KatlOS-managed CRI runtime
support and a user-facing container platform. Operators should not treat
`containerd` as a supported user container platform simply because KatlOS uses it
to run Kubernetes.

## Follow-Up

The next work after this ADR is:

```text
update durable docs to cite this decision and remove contradictory extension-bundle wording
validate mkosi profiles, runtime checks, generation state, kubelet readiness, and VM gates against the base-runtime CRI contract
separate production CNI choice from any minimal CNI tooling carried for tests or bootstrap fixtures
```
