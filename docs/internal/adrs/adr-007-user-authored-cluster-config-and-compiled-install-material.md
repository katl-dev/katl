# ADR-007: User config compiles to a Katl config bundle

Status: proposed.

Date: 2026-06-20.

## Context

Katl has three different configuration shapes that should not be confused:

```text
source cluster config
  one operator-owned document, or a small local source tree, that describes the
  cluster, shared defaults, node classes, system role defaults, nodes, install
  policy, bootstrap intent, and selected Kubernetes intent

Katl config bundle
  one resolved, immutable, self-contained artifact produced by katlctl from the
  source config; suitable for USB, PXE, local handoff, VM tests, runtime config
  apply, and future controller reconciliation

compiled per-node install material
  node-local material inside the bundle, consumed by katlos-install after the
  installer has selected one concrete node
```

The current `install.katl.dev/v1alpha1` manifest mixes user-authored input with
compiled node-local install material. It asks users to provide fields such as
the KatlOS image payload, resolved bootstrap profile IDs, Kubernetes bundle refs,
and other provenance-like values that are better derived by Katl.

Katl should let users write pleasant source config, but installers and updaters
should consume one resolved artifact. Managing sidecar files and relative
references directly through USB, PXE, local handoff, and day-two update paths is
not a good user experience.

The desired workflow is:

```text
katlctl config bundle ./cluster.yaml --output homelab.katlcfg

USB
  copy homelab.katlcfg to installer media

PXE/matchbox
  serve homelab.katlcfg over HTTP and select a node at boot

local handoff
  POST homelab.katlcfg, plus the selected node, to the waiting installer

runtime apply / future controller
  submit or reference homelab.katlcfg as the desired resolved config input
```

KatlOS installer media should also be product-shaped. A KatlOS v15 installer
should install the bundled KatlOS v15 runtime payload by default. A normal user
should not need to select an arbitrary rootfs image during install.

Day-one Kubernetes bootstrap still needs explicit user intent: Kubernetes
version, bootstrap profile, kubeadm/component configuration, API endpoint, node
roles, and node access. Post-day-one Kubernetes upgrades need a separate durable
operation or desired-state surface that can be reconciled by `katlctl`,
node-local `katlc`, or a later in-cluster/on-node controller. Editing the
original install input is not the live upgrade mechanism.

## Decision

The primary deliverable from user-authored cluster config is a Katl config
bundle.

`katlctl config bundle` resolves local references, validates all supported
domains, renders node material, resolves catalogs, and writes a single immutable
artifact:

```text
katlctl config bundle ./cluster.yaml --output homelab.katlcfg
```

The bundle is the artifact delivered to installers, runtime apply operations,
and future controllers. The source config may be one file or a small source tree,
but the bundle must be self-contained. Installers and node agents must reject
unresolved external file references.

The bundle format should use a standard content-addressable container artifact
shape, preferably OCI image layout or an OCI artifact media type, without
presenting it as a runnable container image. Katl owns the media types and
schema versions.

A v0.1 bundle should carry at least:

```text
manifest metadata and schema version
normalized source cluster config
compiled cluster plan
compiled per-node install material
compiled native /etc files
rendered kubeadm input files
resolved Kubernetes payload bundle refs and digests
source and rendered digests
katlctl version/provenance
compatibility requirements
```

The bundle does not normally carry the KatlOS runtime rootfs payload. The normal
install path uses the runtime payload bundled with the installer media. Any
future external runtime payload override must be an advanced feature with a
separate ADR, compatibility checks, provenance, and a concrete user story.

## Source Cluster Config

The source cluster config owns user intent:

```text
cluster name
install policy, including wipeTarget
shared defaults
node classes
systemRole defaults
node list and per-node overrides
bootstrap endpoint/access intent
Kubernetes bootstrap intent
kubeadm/component configuration
```

The source config can be optimized for authoring. It may support inline native
objects and, if accepted for the source format, local file references. File
references are resolved only by `katlctl config bundle`; they are not delivered
as references to the installer.

For the common path, native kubeadm configuration should be expressible inline
so a user can maintain one portable source document:

```yaml
spec:
  kubernetes:
    version: v1.36.0
    bootstrapProfiles:
      control-plane:
        intent: control-plane
        kubeadm:
          config: |
            apiVersion: kubeadm.k8s.io/v1beta4
            kind: InitConfiguration
            nodeRegistration:
              criSocket: unix:///run/containerd/containerd.sock
            ---
            apiVersion: kubeadm.k8s.io/v1beta4
            kind: ClusterConfiguration
            kubernetesVersion: v1.36.0
            controlPlaneEndpoint: api.katl.home:6443
      worker:
        intent: worker
        kubeadm:
          config: |
            apiVersion: kubeadm.k8s.io/v1beta4
            kind: JoinConfiguration
            nodeRegistration:
              criSocket: unix:///run/containerd/containerd.sock
```

If local file references are supported, they are a source-authoring convenience:

```yaml
kubeadm:
  configFile: ./kubeadm/control-plane.yaml
```

The bundle records the resolved content and digest, not the unresolved path.

## Installer Consumption

`katlos-install` consumes one selected node's compiled material from the bundle.
It does not need the whole source config as executable policy.

Installer delivery methods consume the same bundle:

```text
PXE/matchbox
  boot infrastructure serves the bundle and identifies the node, for example by
  explicit node name in boot input

USB/offline media
  media carries the bundle; the installer selects one node by explicit local
  input, firmware/asset identity, or another accepted selector

local handoff
  the operator posts the bundle plus selected node to the waiting installer

VM tests
  the harness builds one bundle and feeds selected node material to each VM
```

The installer verifies the bundle digest and compatibility metadata, selects one
node's compiled material, validates target disk policy, installs the bundled
KatlOS runtime payload, records provenance, writes generation 0, and reboots.

The compiled per-node install material may include resolved and provenance
fields that users should not write by hand:

```text
resolved KatlOS runtime payload metadata from the installer media
resolved Kubernetes payload bundle source/ref/digest
resolved bootstrap profile ID
resolved kubeadm config path and digest
compiled native /etc files
request digest and source metadata
node-local bootstrap intent
```

`install.katl.dev/v1alpha1` may remain as this compiled node-local contract and
debugging surface, but it is not the primary user-authored product API.

## Kubernetes Bootstrap Intent

The source cluster config should include the Kubernetes version or version
policy needed for first bootstrap. Kubernetes is part of the cluster lifecycle,
not the host installer payload.

The ownership split is:

```text
source cluster config
  selects the desired Kubernetes bootstrap version and profile

bundle compiler/catalog resolver
  resolves the version into a supported Kubernetes payload bundle and validates
  compatibility with the KatlOS runtime interface

config bundle
  stores the resolved bundle ref, profile ID, kubeadm input digest, and bootstrap
  intent for each node

katlctl cluster bootstrap
  performs the explicit cluster mutation using the stored intent and live node
  readiness; installation alone does not run kubeadm
```

Users need to specify enough kubeadm/component configuration to bootstrap a real
cluster, but Katl should not make them duplicate resolved implementation fields.
Where Katl exposes kubeadm, it should do so as supported native kubeadm input in
the source config, not as hidden strings in node-local install material.

## Runtime Apply And Post-Day-One Kubernetes Management

The config bundle can also be the delivery artifact for post-install
configuration changes, but the operation being performed still controls what
mutations are allowed.

After bootstrap, the initial install bundle is provenance and initial intent. It
is not automatically the live desired state for the cluster forever.

Post-day-one Kubernetes upgrades should use a lifecycle operation surface:

```text
katlctl-driven operation
  explicit operator command submits a Kubernetes upgrade request to katlc agents
  and records operation status

future in-cluster/on-node controller
  reconciles a cluster-scoped desired Kubernetes version and coordinates
  kubeadm-aware upgrades across nodes
```

The future controller can learn from projects such as HomeOperations `tuppr`:
represent Kubernetes upgrade target version separately from OS upgrade target
version, allow only one Kubernetes upgrade at a time, record status/history,
gate progress on health checks, and coordinate OS and Kubernetes upgrades so
they do not destabilize the cluster.

Katl should keep the same separation:

```text
source config / install bundle
  initial desired Kubernetes version for creating the cluster

runtime operation state
  authoritative source for in-progress or completed bootstrap/upgrade actions

future controller desired state
  desired live Kubernetes version after day one
```

Editing and rebundling source config may be how a user proposes new desired
state, but applying that bundle must go through an explicit operation such as
config apply, Kubernetes upgrade, or KatlOS upgrade. The bundle by itself does
not imply every possible mutation.

## Consequences

The user-facing install experience becomes one source config and one resolved
bundle that can feed all delivery methods. Users do not need to author one
low-level install manifest per node or manage sidecar files for normal installs.

The same bundle format can support USB, PXE, handoff, VM tests, runtime config
apply, and future controller reconciliation. Delivery becomes uniform even when
the source config uses inline native YAML or local file references.

`katlos-install` stays node-local and simple. It validates and executes one
selected node's material from the bundle, writes generation 0, records
provenance, and reboots. It does not understand or reconcile whole-cluster
desired state.

Kubernetes version belongs in cluster lifecycle intent. It should be present in
the source config for bootstrap, but it should later move into a dedicated
runtime operation or controller-managed desired state for upgrades.

The bundle compiler must preserve enough provenance for repeatability and
debugging: bundle digest, source config digest, resolved Kubernetes bundle
digest, kubeadm input digests, generated node material digests, and Katl tooling
version.

## Open Questions

The following details still need refinement before this ADR can be accepted:

```text
exact source config kind and field names
exact config bundle media type and on-disk layout
whether v0.1 supports source config file refs or requires all native config
  inline
how USB media selects a node when one bundle contains multiple nodes
how PXE and handoff represent the selected node and bundle digest
whether compiled node material is a public stable schema or an internal bundle
  member schema
how future in-cluster desired state is named, installed, authorized, and kept in
  sync with node-local katlc state
whether a post-day-one controller should own only Kubernetes upgrades or also
  KatlOS upgrades
how config bundle signing and trust roots are introduced after digest-only v0.1
```
