# Katl North Star

Status: durable product direction.

Katl produces and maintains KatlOS: an installable, upgradeable,
systemd-native Kubernetes node OS. Users customize KatlOS by supplying Katl YAML
or configuration, which `katlc` validates and compiles into sysext/confext
generations. Those generations are activated with rollback-aware runtime state
while staying close to the native Linux, systemd, and kubeadm files and APIs
they configure.

The practical outcome is a reproducible path from generic KatlOS artifacts and
user-supplied cluster intent to booted KatlOS nodes, then to an explicitly
bootstrapped Kubernetes cluster:

```text
KatlOS source
  -> mkosi builds generic installer, runtime, sysext, and update artifacts
  -> user-managed boot or release infrastructure publishes those artifacts
  -> katlos-install installs KatlOS generation 0 with stored cluster intent
  -> katlctl cluster bootstrap asks katlc to validate that intent and create the
     first Kubernetes-capable candidate generation
  -> katlctl coordinates kubeadm bootstrap or join and commits the generation
     only after kubeadm and health checks succeed
  -> later explicit kubeadm-aware operations upgrade, repair, or recover nodes
  -> KatlOS activates, stages, reports, or rolls back host generations
  -> after bootstrap, the user installs and owns cluster add-ons, GitOps, and
     workloads
```

## Product Shape

Katl has three durable product surfaces:

```text
katlc
  Runs on KatlOS as the user-facing state and configuration command. It accepts
  user-supplied Katl YAML or configuration, validates supported domains,
  compiles them into generation-scoped sysext/confext payloads and metadata,
  and applies, stages, reports, or rolls back runtime state.

katlos-install
  Runs in the installer environment, applies a user-supplied install manifest,
  owns Katl disk layout, writes runtime generations, and records install state.

KatlOS runtime
  Boots installed nodes into a small, systemd-native Linux environment with the
  host plumbing needed for containerd, kubelet, kubeadm, updates, health
  checks, and rollback.
```

The KatlOS runtime is intentionally narrow. It carries the kernel, systemd,
networking, storage, SSH access for operators, container runtime support,
`katlc`, Katl-owned runtime services, generated configuration, and selected sysexts. Kubernetes add-ons,
workload policy, ingress, storage systems, GitOps controllers, and application
workloads live in the cluster layer unless a future design adds a bounded
node-level capability.

## Design Principles

Katl treats Kubernetes as the first-class host workload.

The base system should make kubeadm, kubelet, containerd, host networking,
kernel/network prerequisites needed by user-installed CNI, persistent Kubernetes
state, and cluster bootstrap predictable. The node is successful when generation
0 reaches installed-runtime health and an explicit bootstrap operation can reach
a clear kubeadm-ready point with enough status for an operator or test harness to
continue safely.

Katl uses systemd-native mechanisms directly.

The runtime model is built from systemd-boot, UKIs, systemd-repart,
systemd-sysext, systemd-confext, systemd-tmpfiles, native mount units,
systemd-networkd, boot health targets, and generation selection. Katl should
prefer native files and native ordering over hidden supervisors or broad custom
configuration engines.

Katl configuration is a thin, typed abstraction.

User input should describe Katl-owned domains and preserve native syntax where
that is the clearest interface. A network domain may accept `.network`,
`.netdev`, and `.link` content. Kubeadm input remains native kubeadm YAML.
Katl adds validation, ownership, render paths, apply mode, trust handling, and
rollback behavior around those native artifacts.

Katl configuration is applied to KatlOS nodes.

Install and runtime apply paths both start from Katl YAML or configuration, not
pre-rendered extension trees supplied by users. On first install,
`katlos-install` bootstraps generation 0 and stores user-supplied cluster intent
without activating Kubernetes. The first Kubernetes-capable generation is
created later when `katlctl cluster bootstrap` asks `katlc` to validate stored
intent, select the requested bundled Kubernetes sysext, and render the generated
confext needed for kubeadm. Later host configuration changes can use normal
`katlc` generation apply or stage flows. Sysext payloads remain prebuilt
artifacts; node-local compilation decides how trusted config selects and
activates them. `katlc` and the KatlOS runtime services are the enforcement point
on installed nodes: unknown domains, unsupported fields, unsupported apply
modes, unsupported sysext selection requests, and raw extension activation
inputs are rejected before anything is rendered or activated.

Katl artifacts are generic and reusable.

Installer images, runtime roots, Kubernetes sysexts, and update bundles should
be usable through PXE, matchbox, USB, local handoff, or an existing installed
node upgrade path. Node identity, install authorization, disk selection, network
configuration, cluster intent, and secrets arrive through runtime input channels
rather than being baked into a generic artifact.

Katl lifecycle uses generations and operations.

A generation selects the runtime root, UKI, kernel command line, sysext set,
generated confext set, and health state as one unit. Runtime root updates,
Kubernetes sysext updates, and configuration-only changes may move
independently when compatibility metadata allows it, while rollback always
returns to a complete previously selected generation.

Operations are explicit stateful workflows that move reality toward a desired
generation or cluster state. `katlc` owns node-local planning and status,
systemd executes and supervises node-local operation units, `katlctl` may
coordinate operator-driven multi-node workflows, and kubeadm remains
authoritative for Kubernetes cluster mutation.

Multi-node operator workflows may use bounded `katlctl` coordinator run records
that aggregate explicit node operation attempts. Those records provide audit,
retry, and diagnostics; they do not make Katl a continuous cluster lifecycle
controller.

Update machinery should use native systemd functions where they fit:
systemd-boot selection and boot counting, systemd-sysext and systemd-confext
activation, native mount ordering, tmpfiles, and health targets. Katl agents
coordinate validation, generation spec/status, operation records, and rollback around those
native mechanisms.

Katl is GitOps-oriented at the node boundary.

Katl should fit naturally into a repository-driven workflow: review config,
compile it, build or select artifacts, publish them, install or update nodes,
then let kubeadm and the user's chosen cluster tooling, including GitOps when
they use it, reconcile the cluster layer. Katl should make the handoff explicit
through generated metadata, status, logs, and stable commands rather than
absorbing cluster add-on lifecycle.

Katl grows by proving one operating loop at a time.

Implementation should move from local build and VM proof, to install, to
kubeadm readiness, to multi-node bootstrap, to update and rollback. Each loop
needs unit tests for planning, golden tests for generated assets, systemd
verification where practical, and VM coverage for boot, install, update, and
Kubernetes handoff behavior.

## Ownership Boundaries

Katl owns:

```text
KatlOS configuration compilation and validation through katlc
generic KatlOS installer and update artifact contracts
target root disk layout selected by a Katl install manifest
runtime generation spec/status
systemd boot, mount, extension, and health wiring
generated confext content for supported domains
Kubernetes sysext selection and compatibility checks
kubeadm-ready host prerequisites
operator-facing node status and diagnostics
```

Users own:

```text
DHCP, TFTP, iPXE, matchbox, USB writing, firmware, and asset publication
cluster bootstrap policy after Katl prepares the nodes
kubeadm invocation timing outside Katl-provided bootstrap helpers
CNI, CoreDNS, Flux, GitOps controllers, ingress, storage, and workloads
site routing policy and fabric integration
secrets distribution and cluster application configuration
```

Future Katl capabilities can make some user-owned workflows easier by defining
small, typed integration points. A capability should compile to supported native
domains, select an explicit sysext or helper contract, or produce clear handoff
material for user-managed GitOps.

## User Story

A user keeps cluster node intent in Git. The repository describes node roles,
hostnames, networkd units, SSH keys, kubeadm config references, target disk
selectors for install, requested Kubernetes versions, and any supported extra
data disk mounts.

The user publishes generic Katl artifacts through their own infrastructure.
PXE, matchbox, USB, virtual media, or local handoff can all provide the
installer with the same generic image plus node-specific install input.

`katlos-install` applies the manifest to the selected node. It verifies
artifacts, partitions and formats the Katl-owned root disk, writes generation 0,
persists identity, state layout, and cluster intent, installs boot metadata, and
reboots.

The installed runtime reaches a local health target for generation 0 with no
Kubernetes binaries activated and no cluster state created. The user runs
`katlctl cluster bootstrap`, which asks `katlc` to validate the stored intent,
create and activate the first Kubernetes-capable candidate generation, select
the requested Kubernetes sysext, render kubeadm input, and expose the writable
kubeadm output paths. `katlctl` then coordinates kubeadm init or join and the
generation is committed only after kubeadm succeeds and health checks pass. Once
cluster bootstrap completes, the user installs CNI, DNS, GitOps, policies,
storage, and applications with their chosen cluster tooling.

Updates follow the same model. A new desired state compiles into a new
generation. Online-applicable configuration can apply immediately through a
tested domain path. Staged configuration, runtime root changes, and sysext
changes become a new boot generation with boot health and rollback semantics.

## Decision Filter

Use these questions when adding a feature or design:

```text
Does it help build, install, update, recover, or operate Kubernetes nodes?
Does it compile to native Linux, systemd, kubeadm, or Katl-owned artifacts?
Does it preserve clear ownership of persistent state and generated state?
Does it fit the generation model for health, update, and rollback?
Does it keep generic artifacts separate from node-specific input?
Does it improve the GitOps workflow without owning the entire cluster layer?
Can it be validated with unit, golden, systemd, VM, or integration tests?
```

If the answer is unclear, capture the design as an open question and keep the
implementation surface small until a testable operating loop exists.

## Document Map

Start with `docs/internal/initial-design.md` for the current architecture
snapshot. This document provides the durable product direction that the current
design is expected to serve.

Installer, runtime, and artifact contracts:

```text
docs/internal/installer-runtime-design.md
docs/internal/single-katlos-image-artifact.md
docs/internal/installer-boot-artifact-variants.md
docs/internal/node-install-to-bootstrap-state-machine.md
docs/internal/generations-and-operations.md
```

Configuration model and trusted input:

```text
docs/internal/adrs/adr-001-generated-confext-configuration.md
docs/internal/adrs/adr-002-live-and-next-boot-config-apply-modes.md
docs/internal/adrs/adr-003-runtime-config-input-and-trust.md
docs/internal/supported-node-config-domains.md
docs/internal/system-roles-and-capabilities.md
docs/internal/kubeadm-config-input-design.md
```

Schemas and examples:

```text
docs/internal/schemas/install-manifest-v1alpha1.schema.json
docs/internal/examples/minimal-install-manifest.json
docs/internal/examples/config-domain-install-manifest.json
```

Generation, health, update, and rollback:

```text
docs/internal/generation-metadata-model.md
docs/internal/boot-health-semantics.md
docs/internal/rollback-selection-rules.md
```

Persistent state:

```text
docs/internal/persistent-state-inventory.md
docs/internal/writable-state-layout.md
docs/internal/etc-kubernetes-projection.md
docs/internal/stacked-etcd-bootstrap-data-policy.md
```

Cluster bootstrap and Kubernetes readiness:

```text
docs/internal/cluster-bootstrap-cli.md
docs/internal/kubeadm-api-smoke-design.md
docs/internal/kubernetes-upgrade-operations.md
```

Optional platform endpoint work:

```text
docs/internal/platform-api-endpoint-user-story.md
docs/internal/platform-api-endpoint-routing-capability.md
docs/internal/platform-api-endpoint-helper-input-schema.md
```

Development and test harnesses:

```text
docs/developing.md
docs/internal/go-vm-test-harness-design.md
```
