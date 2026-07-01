# ADR-007: User config compiles to a Katl config bundle

Status: accepted.

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

The v0.1 bundle format is an OCI image-layout archive with a Katl custom
manifest. It is not a runnable container image. Katl owns the artifact type,
media types, schema versions, and canonical JSON used for digest identity.

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

## v0.1 Bundle Artifact Format

`katlctl config bundle` writes a single `.katlcfg` file. The file is a tar
archive containing an OCI Image Layout v1 directory. Implementations may also
use the unpacked directory form internally for tests and caches, but the
portable user artifact is the archive.

```text
homelab.katlcfg
  oci-layout
  index.json
  blobs/sha256/<bundle-manifest-digest>
  blobs/sha256/<member-digests>
```

The OCI artifact identity is:

```text
artifactType: application/vnd.katl.config.bundle.v1
config.mediaType: application/vnd.katl.config.bundle.v1+json
config.digest: sha256:<bundle-manifest-digest>
```

The custom bundle manifest is canonical JSON for digest purposes: UTF-8,
deterministic object key order from Katl tooling, lowercase `sha256:<hex>`
digests, integer byte sizes, RFC 3339 timestamps, and no mutable freshness,
`latest`, or URL-derived identity fields. The manifest does not contain its own
digest. The digest is computed over the manifest bytes and recorded in OCI
descriptors, catalogs, delivery metadata, install records, generation metadata,
and operator-visible status.

The custom manifest media type is:

```text
application/vnd.katl.config.bundle.v1+json
```

The manifest schema contains:

```text
apiVersion: config.katl.dev/v1alpha1
kind: KatlConfigBundle
artifactKind: katl.config.bundle.v1
artifactVersion
bundleSchemaVersion: 1
clusterName
createdAt

compatibility
  supportedArchitectures[]
  supportedKatlOSRuntimeInterfaces[]
  minKatlVersion, when needed
  maxKatlVersion, when needed
  requiredInstallerFeatures[]
  requiredKatlcFeatures[]
  configDomainSchemas[]
  installMaterialSchemaVersion
  clusterPlanSchemaVersion
  kubeadmAPIVersions[]

source
  normalizedConfig
  originalInputs[]
  sourceDigest
  sourceTreeDigest, when the source was a directory

cluster
  resolvedPlan
  bootstrapIntent
  kubernetesPayloads
  platformEndpointPlan, when used

nodes[]
  name
  systemRole
  nodeClass, when set
  architecture
  nodeMaterial
  installMaterial
  nativeConfig
  kubeadmInputs[]
  resolvedDigests

descriptors[]
  role
  node, when node-scoped
  mediaType
  digest
  sizeBytes
  fileName
  annotations

provenance
  katlctlVersion
  katlctlCommit
  compilerSchemaVersion
  sourceDigest
  renderedDigest
  createdBy
```

`artifactVersion` identifies one immutable compiler output. Re-running the
compiler with different source bytes, catalog selections, tool versions, or
rendered member bytes produces a new bundle manifest digest. Mutable aliases may
exist only for discovery outside the bundle; before install or apply, consumers
must normalize to `sha256:<bundle-manifest-digest>`.

## Required Members

Every v0.1 bundle contains descriptors for these cluster-scoped members:

```text
normalized source config
  role: source-normalized
  mediaType: application/vnd.katl.cluster-config.v1+yaml
  fileName: source/cluster.normalized.yaml

source provenance
  role: source-provenance
  mediaType: application/vnd.katl.source-provenance.v1+json
  fileName: source/provenance.json

compiled cluster plan
  role: cluster-plan
  mediaType: application/vnd.katl.cluster-plan.v1+json
  fileName: cluster/plan.json

Kubernetes payload resolution records
  role: kubernetes-payloads
  mediaType: application/vnd.katl.kubernetes.payload.resolution.v1+json
  fileName: cluster/kubernetes-payloads.json

bundle provenance
  role: bundle-provenance
  mediaType: application/vnd.katl.bundle-provenance.v1+json
  fileName: bundle/provenance.json
```

The Kubernetes payload resolution record stores, for each selected payload:

```text
requestedVersion
resolvedPayloadVersion
source
ref
bundleManifestDigest
sysextPayloadDigest
artifactVersion
architecture
supportedRuntimeInterfaces[]
catalogDigest, when selected through a catalog
resolverVersion
```

The bundle may reference externally published Kubernetes payload bundles by
source/ref/digest. It does not embed the Kubernetes sysext payload unless a
future offline-material ADR explicitly adds that mode.

## Node Material Layout

Every node listed in the resolved cluster plan has a node-scoped descriptor set
under `nodes/<node-name>/`. Node names are safe path segments after validation;
path separators, traversal, absolute paths, empty names, and names that differ
only by case are rejected.

Required node-scoped members are:

```text
compiled install material
  role: node-install-material
  mediaType: application/vnd.katl.node-install-material.v1+json
  fileName: nodes/<node-name>/install/material.json

compiled cluster intent
  role: node-cluster-intent
  mediaType: application/vnd.katl.node-cluster-intent.v1+json
  fileName: nodes/<node-name>/cluster/intent.json

compiled native config plan
  role: node-native-config
  mediaType: application/vnd.katl.node-native-config.v1+json
  fileName: nodes/<node-name>/config/native.json

node digest index
  role: node-digests
  mediaType: application/vnd.katl.node-digests.v1+json
  fileName: nodes/<node-name>/digests.json
```

The compiled install material is the installer-facing node contract. It carries
the selected node name, role, target disk identity requirements, wipeTarget
guard, network/install inputs, resolved cluster intent reference, and provenance
needed for `katlos-install` to write generation 0. It must not require the
installer to evaluate source-level defaults, node classes, templates, local
file paths, catalogs, or version policies.

Node native config material contains only Katl-owned generated runtime
configuration domains. It is suitable for confext generation or later runtime
config apply, but each operation still decides which domains it may mutate.

## Kubeadm Input Storage

Kubeadm input is stored as rendered native kubeadm YAML, not as hidden strings
or unresolved source references. Each rendered kubeadm input has a node-scoped
descriptor:

```text
role: kubeadm-input
mediaType: application/vnd.katl.kubeadm.input.v1+yaml
fileName: nodes/<node-name>/kubernetes/kubeadm/<resolved-id>.yaml
annotations:
  dev.katl.kubeadm.resolved-id: <resolved-id>
  dev.katl.kubeadm.intent: init | control-plane | worker
  dev.katl.kubeadm.api-versions: <comma-separated versions>
```

The bundle manifest records the digest of every kubeadm input and the node
material references those digests by resolved ID. The installer and bootstrap
planner must reject node material that references a kubeadm input digest or
resolved ID not present in the same bundle.

## Reference Resolution Rules

The source config may use local authoring conveniences accepted by the source
schema, such as relative kubeadm config files. `katlctl config bundle` resolves
those references before writing the bundle. Bundle consumers never receive
authoring references as work to perform.

The compiler rejects a bundle when any emitted member still contains:

```text
relative or absolute host file paths from source config
environment variable substitutions
template expressions, ranges, or generators
latest, stable, or other mutable version aliases
unresolved Kubernetes payload catalog selectors
unresolved kubeadm config refs
unresolved node class or defaults references
raw sysext or rootfs paths where a Katl bundle source/ref is required
```

The only allowed external references in a v0.1 config bundle are explicit
source/ref/digest triples for separately published Katl artifacts, such as
Kubernetes payload bundles. Those references are resolved enough to include the
exact manifest digest and compatibility metadata in this bundle.

## Trust And Delivery

v0.1 config bundle trust is digest-only. Consumers verify:

```text
the archive unpacks as one OCI image layout
the OCI config descriptor media type is application/vnd.katl.config.bundle.v1+json
the custom manifest digest matches the expected bundle digest
every descriptor digest and size matches the fetched member bytes
compatibility metadata matches the installer/runtime interface and architecture
the selected node material exists and matches the selected node
```

Signing, trust roots, revocation, transparency logs, and encrypted secret
material are explicit follow-up work. The v0.1 format reserves optional
signature descriptors, but normal consumers must not treat missing signatures as
a downgrade because signatures are not part of the accepted v0.1 trust model.

Delivery paths identify and verify the same bundle this way:

```text
USB/offline media
  media carries homelab.katlcfg plus expected sha256 digest metadata; the
  installer receives or derives the selected node name and verifies the digest
  before selecting nodes/<node>/install/material.json.

PXE/matchbox
  boot input provides a bundle URL, expected bundle digest, and selected node.
  The installer fetches the archive, verifies the digest, and rejects mutable
  URL-only input.

local handoff
  the operator posts the bundle archive, selected node, and expected digest to
  the waiting installer. The installer verifies the posted bytes before
  mutation.

VM tests
  the harness builds one bundle, records its digest in the run manifest, and
  supplies each VM with the same archive plus selected node.

runtime apply
  katlctl submits bundle bytes or a bundle source plus expected digest to
  katlc. katlc stages the bundle, verifies descriptors, and then applies only
  the operation-allowed runtime domains.

future controllers
  controllers store desired bundle source/ref/digest and reconcile only after
  resolving to the same custom bundle manifest digest and compatibility data.
```

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
node's compiled material, validates target disk policy, installs the KatlOS
runtime payload carried by the installer media, records provenance, writes
generation 0, and reboots.

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

The following details remain follow-up work after the v0.1 bundle format
decision:

```text
exact source config kind and field names
how future in-cluster desired state is named, installed, authorized, and kept in
  sync with node-local katlc state
whether a post-day-one controller should own only Kubernetes upgrades or also
  KatlOS upgrades
production config bundle signing, trust roots, revocation, and encrypted secret
  material after digest-only v0.1
```
