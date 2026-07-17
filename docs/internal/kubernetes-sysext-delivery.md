# Kubernetes Sysext Delivery

Status: working design.

Katl needs a concrete path for Kubernetes payloads before the full day-2
upgrade controller exists. The north star is a set of Katl-produced, immutable
Kubernetes extension bundles for exact Kubernetes patch versions. Users and
automation reference those bundles by version and digest; Katl validates them
against the selected KatlOS runtime before creating or committing a generation.

## Decision

The durable user-facing artifact is a Kubernetes payload bundle. It is not a
Kubernetes distribution, not a user-specific node image, and not a naked raw
sysext blob.

The preferred bundle format is an OCI artifact with a Katl custom manifest. The
manifest should describe the Kubernetes payload version, architecture, Katl
runtime compatibility, package provenance, digest and size information,
signature hooks, and one or more payload layers. The systemd-sysext image remains
the activation payload that `katlc` stages locally, but the raw sysext file is a
layer inside the bundle rather than the primary object users are expected to
reference.

Each published bundle contains, either as OCI descriptors/layers or as the
equivalent static layout defined below:

```text
Katl custom bundle manifest
Kubernetes sysext payload layer
Kubernetes sysext metadata
catalog entry or catalog fragment
checksums and signature material when enabled
```

The sysext contains versioned Kubernetes host tools such as `kubeadm`,
`kubelet`, `kubectl`, and required helper packages. The sidecar metadata and
catalog entry bind artifact version, Kubernetes payload version, architecture,
package versions, source repository, digest, size, and supported Katl runtime
interfaces.

Generic confext content may be added to the bundle only when it is safe for
every node that selects that Kubernetes payload. Node-specific kubeadm input,
PKI, bootstrap tokens, kubeconfigs, network identity, secrets, and generated
Katl configuration remain node-local generated confext rendered by `katlc`.
Publishing prebuilt user-specific confext is outside the default path.

## Today's Install Story

`katlos-install` does not bundle or activate a Kubernetes sysext. The install
image creates generation 0, installs the KatlOS runtime, and records bootstrap
intent as an exact Kubernetes bundle source/ref.

A user who wants Kubernetes `v1.36.0` installs a compatible KatlOS image and
supplies `katlc` with an HTTPS source for the Kubernetes payload bundle, such as
a GHCR OCI reference endpoint or a GitHub Releases-hosted OCI layout/catalog,
together with an exact selection such as `v1.36.0`. The explicit bootstrap
operation later asks `katlc` to fetch the bundle, verify the Katl custom
manifest and payload digests, stage the sysext under Katl-owned storage, create
generation 1, select the staged sysext, render node-specific generated confext,
run kubeadm, and commit only after operation health checks pass.

A user who wants a fresh cluster on Kubernetes `v1.36.1` can use the same
KatlOS install image when runtime compatibility permits it, but supplies an
HTTPS source/ref that resolves to the `v1.36.1` bundle. `v1.36.0` and
`v1.36.1` remain separately addressable by exact payload version and digest.

Upgrading an already bootstrapped node from `v1.36.0` to `v1.36.1` is a
different workflow. The target sysext can be produced and cataloged now, but
node mutation remains unsupported until the kubeadm-aware upgrade operation and
kubelet activation gate are implemented and VM-tested.

## Node Acquisition And Activation

`katlc` acquires Kubernetes payload bundles from a user-supplied HTTPS source
and a separate selector. The public reference shape is:

```text
REGISTRY/REPOSITORY:vMAJOR.MINOR.PATCH-katl.BUILD[@sha256:<OCI-manifest-digest>]
```

The YAML-facing install or bootstrap intent uses:

```yaml
node:
  bootstrap:
    kubernetesBundle: ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@sha256:<OCI-manifest-digest>
```

`katlctl cluster bootstrap` uses the same value through `--kubernetes-bundle`.
Katl derives the registry API endpoint internally and records the submitted
reference plus every resolved digest in the node-local operation record.

Examples:

```text
ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1
ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@sha256:<OCI-manifest-digest>
```

The digest pin is optional so the first-use path remains approachable, but it is
strongly recommended. Before `katlc` writes a generation, a tag-only reference
is resolved once and normalized to the OCI manifest digest, exact Kubernetes
payload version, Katl bundle manifest digest, and sysext payload SHA-256.
Generation metadata never records a floating `default`, `latest`, or minor-only
Kubernetes selection.

Resolver behavior is host-shape specific but produces the same bundle manifest:

```text
GHCR or registry source
  Resolve the tag or supplied manifest digest through the OCI distribution API, fetch the
  Katl bundle manifest from the OCI config descriptor, verify that the bundle
  manifest digest matches the pinned digest when present, then fetch payload
  and metadata layers by digest.

GitHub Releases or static OCI layout source
  Resolve ref through an index or catalog document in the source directory,
  verify the selected bundle manifest digest, then fetch relative descriptor
  URLs by digest and size.

local or vmtest fixture source
  Use an HTTPS-served or file-backed static layout only when it contains the
  same catalog/index, bundle manifest, descriptors, and digests as the remote
  shape. Raw sysext files are not accepted as a source.
```

Unauthenticated public HTTPS is the v0.1 default. Authenticated registries,
private GitHub Releases, bearer tokens, client certificates, and mirror trust
roots are deferred until a separate credential materialization and redaction
design exists. Diagnostics must redact credentials embedded in URLs even though
credentialed URLs are not accepted for v0.1.

## Bundle Format

The user-facing `@sha256:` value pins the OCI distribution manifest. The Katl
custom bundle manifest remains the stable payload identity inside that OCI
manifest and is fetched through its config descriptor. Katl records and verifies
both digests; the sysext payload digest remains the activation identity.

The custom manifest media type is:

```text
application/vnd.katl.kubernetes.payload.bundle.v1+json
```

The manifest is canonical JSON for digest purposes: UTF-8, no insignificant
transport wrapper, object keys emitted in deterministic order by Katl tooling,
lowercase `sha256:<hex>` digests, integer byte sizes, RFC 3339 timestamps, and
no mutable fields such as download URL freshness, latest aliases, or mirror
preference. The digest is over those exact manifest bytes. The manifest does
not contain its own digest or OCI distribution digest; those values are computed
over the manifest bytes and recorded in refs, OCI descriptors, indexes,
catalogs, cache paths, and generation metadata.

The manifest schema is:

```text
apiVersion: payload.katl.dev/v1alpha1
kind: KubernetesPayloadBundle
name: katl-kubernetes
artifactKind: katl.kubernetes-payload.v1
artifactVersion
payloadVersion
kubernetesMinor
architecture
payloads[]
  role: systemd-sysext
  mediaType
  digest
  sizeBytes
  fileName
  annotations
metadata[]
  role: sysext-metadata | package-provenance | catalog-fragment | signature
  mediaType
  digest
  sizeBytes
  fileName
  annotations
sourceRepository
packageVersions
packageLockDigest or buildInputDigest
supportedRuntimeInterfaces[]
supportedKubeadmConfigAPIFamilies[]
supportedSourceKubernetesMinors[]
skewPolicy
createdAt
signatures[] or explicit unsigned-fixture marker
```

Required descriptor roles and media types for v0.1 are:

```text
systemd-sysext payload
  role: systemd-sysext
  mediaType: application/vnd.katl.sysext.raw.v1
  fileName: katl-kubernetes-<payloadVersion>-<architecture>.sysext.raw

sysext metadata
  role: sysext-metadata
  mediaType: application/vnd.katl.kubernetes.sysext.metadata.v1+json
  fileName: metadata.json

package provenance
  role: package-provenance
  mediaType: application/vnd.katl.package-provenance.v1+json
  fileName: package-provenance.json

catalog fragment
  role: catalog-fragment
  mediaType: application/vnd.katl.kubernetes.catalog.entry.v1+json
  fileName: catalog-entry.json
```

Optional descriptor roles are:

```text
checksum
  mediaType: text/plain

signature
  mediaType: application/vnd.dev.sigstore.bundle.v0.3+json or the selected
  signing-envelope media type

generic-confext
  mediaType: application/vnd.katl.confext.raw.v1
  allowed only when the producer-boundary rules prove the content is universal
```

In GHCR, the published object is an OCI image manifest:

```text
artifactType: application/vnd.katl.kubernetes.payload.bundle.v1
config.mediaType: application/vnd.katl.kubernetes.payload.bundle.v1+json
config.digest: sha256:<bundle-manifest-digest>
layers[]: payload and metadata descriptors from the custom manifest
annotations:
  dev.katl.payload.version: v1.36.0
  dev.katl.kubernetes.minor: v1.36
  dev.katl.architecture: x86_64
  dev.katl.bundle.manifest.digest: sha256:<bundle-manifest-digest>
  dev.katl.sysext.payload.digest: sha256:<sysext-payload-digest>
```

Tags are convenience aliases only:

```text
v1.36.0-katl.1
sha256-<bundle-manifest-digest>
```

Tags must never be recorded in generation metadata without resolving to the
bundle manifest digest and sysext payload digest. A release tag must not be
moved after publication. If a payload must be rebuilt for the same Kubernetes
patch, publish a new `artifactVersion` and tag such as
`v1.36.0-katl.2`; the `payloadVersion` remains `v1.36.0`.

The GitHub Releases or local fixture static layout uses the same custom
manifest bytes and digest:

```text
index.json
catalog/
  v1.36.json
bundles/
  v1.36.0/
    x86_64/
      bundle.json
      catalog-entry.json
      package-provenance.json
      metadata.json
blobs/
  sha256/
    <bundle-manifest-digest>
    <sysext-payload-digest>
    <metadata-digest>
    <package-provenance-digest>
checksums.txt
signatures/
  <bundle-manifest-digest>.sigstore.json
```

`bundles/<payloadVersion>/<architecture>/bundle.json` and
`blobs/sha256/<bundle-manifest-digest>` contain identical bytes. The duplicate
named path is for human inspection; the blob path is for digest-addressed
fetching and mirroring. Static layout fixtures used by tests must have the same
shape; a loose `katl-kubernetes.raw` file is not a valid source.

The source root `index.json` is a discovery document:

```text
apiVersion: payload.katl.dev/v1alpha1
kind: KubernetesPayloadIndex
entries[]
  payloadVersion
  artifactVersion
  kubernetesMinor
  architecture
  bundleManifestDigest
  bundleManifestPath
  sysextPayloadDigest
  supportedRuntimeInterfaces[]
  catalogEntryPath
  deprecated
```

`catalog/<kubernetesMinor>.json` is the minor-specific catalog view used for
listing and mirroring. Each entry repeats the bundle manifest digest and sysext
payload digest. Catalog documents may be signed, but catalog signatures prove
only the listing; `katlc` still verifies the selected bundle manifest and every
descriptor digest before staging.

Mirrors preserve bytes, not trust shortcuts. A mirror may rewrite source URLs
and catalog paths, but it must keep the custom bundle manifest bytes,
descriptor digests, sizes, payload blobs, payload versions, artifact versions,
and runtime compatibility metadata unchanged. If a mirror recompresses,
repackages, or regenerates any descriptor, it has created a new bundle and must
publish a new bundle manifest digest.

The public GHCR references are:

```text
ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1
ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@sha256:<OCI-manifest-digest>
```

The sysext payload digest, not the catalog digest, remains the activation digest
stored in generation metadata. The bundle manifest digest is the distribution
pin used to prove the same custom manifest was fetched again. The OCI manifest
digest, static index digest, and catalog digest may be recorded for diagnostics
and mirror audit, but they are not the generation activation digest. The catalog
is useful for listing and mirroring; it is not sufficient by itself to stage or
activate a payload.

Acquisition is separate from activation:

```text
1. resolve source/ref to one bundle manifest
2. verify the bundle manifest digest when pinned
3. verify manifest schema, artifact kind, payload version, architecture, and
   runtime compatibility metadata
4. fetch payload and sidecar descriptors into a temporary Katl artifact
   directory
5. verify every descriptor digest and size, including the sysext payload
6. write an immutable local cache entry under /var/lib/katl/artifacts/
7. create a candidate generation that selects the cached sysext and generated
   kubeadm-ready confext
8. activate only through the generation's `/run/extensions` link and
   `systemd-sysext`
```

The cache layout is content-addressed by bundle manifest digest and sysext
payload digest, for example:

```text
/var/lib/katl/artifacts/kubernetes/
  bundles/sha256-<bundle-manifest-digest>/
    bundle.json
    catalog-entry.json
    package-provenance.json
  sysext/sha256-<sysext-payload-digest>/
    katl-kubernetes-<payloadVersion>-<arch>.sysext.raw
    metadata.json
  index.json
  pins.json
  locks/
```

`katlc` writes cache entries through same-filesystem temporary directories and
renames only after every digest and required field verifies. Failed or
interrupted fetches leave either no cache entry or an entry marked incomplete
that cleanup may remove. The cache is an implementation detail; user config
does not point at cached paths.

`index.json` is a derived local index rebuilt from verified cache entries when
needed. It may speed listing, but it is not trusted without rechecking the
bundle manifest digest and sysext payload digest. `pins.json` records
operator-pinned exact payload versions and optional reasons. `locks/` contains
node-local advisory locks so concurrent operations cannot fetch, stage, or clean
the same digest at the same time. Locking is scoped by bundle manifest digest
and sysext payload digest, not by mutable tags.

Version listing has two views:

```text
remote list
  Reads the source catalog or index, verifies its schema and optional signature,
  and reports exact payload versions that match the requested architecture and
  runtime interface.

local list
  Reads `/var/lib/katl/artifacts/kubernetes`, verifies cached metadata and
  payload digests, and reports exact payload versions already available offline.
```

Offline bootstrap may use a cached bundle only when the cache entry includes the
same manifest fields, sysext payload digest, runtime compatibility metadata, and
unsigned-fixture or signature state that the online path would validate. A
cached raw sysext without its bundle manifest is not a valid acquisition source.

Retention keeps every Kubernetes sysext referenced by:

```text
the current selected generation
the previous known-good generation
any candidate, rollback, or in-progress OperationRecord
any operator-pinned exact payload version
```

Cleanup may remove unreferenced cache entries after operation records no longer
need their diagnostics. Cleanup must never remove a sysext selected by a valid
generation spec, and it must not rewrite generation metadata.
GC order is:

```text
1. take the cache cleanup lock
2. enumerate selected generations, boot-selection rollback targets, operation
   records, and pins
3. remove incomplete temporary entries older than the active operation timeout
4. remove unreferenced bundle metadata
5. remove unreferenced sysext payloads only after no remaining bundle references
   them
6. rebuild index.json from verified survivors
```

Before selection, `katlc` validates:

```text
payloadVersion exactly matches resolved intent
architecture matches the installed runtime
supportedRuntimeInterfaces includes the runtime root interface
Kubernetes minor and skew policy are allowed for the requested operation
kubeadm config kubernetesVersion, when present, matches the selected payload
supportedKubeadmConfigAPIFamilies covers the rendered kubeadm input
required host prerequisites are present or planned in the candidate generation
```

Skew validation is operation-specific:

```text
first bootstrap or first join
  Exact `v1.36.x` payload selected by bundle ref is allowed when rendered
  kubeadm input either omits kubernetesVersion or names the same exact version.

normal runtime config apply
  Kubernetes payload selection must be unchanged; any requested sysext change is
  rejected before render.

Kubernetes upgrade operation
  The selected target payload must be a supported next patch or reviewed minor
  transition from the live cluster version, must satisfy the bundle
  `supportedSourceKubernetesMinors[]` and `skewPolicy`, and must provide a
  kubeadm version that can own the upgrade phase before target kubelet
  activation.
```

For first bootstrap and join, this validation may select the exact staged
Kubernetes sysext for generation 1. For an already bootstrapped node, changing
the selected Kubernetes sysext is rejected unless the request is an explicit
kubeadm-aware Kubernetes upgrade operation. Normal runtime configuration apply
must not change the Kubernetes sysext.

Raw sysext activation is rejected as user input. Users may provide a local or
offline fixture source only when it presents the same bundle manifest and digest
shape as a remote source. `katlc` must not accept a path to
`katl-kubernetes-*.sysext.raw` as the stable API.

`systemd-sysupdate` is deferred for Kubernetes payload bundles. The accepted
sysupdate target is the KatlOS runtime root and UKI transfer path. Kubernetes
payload acquisition uses Katl bundle fetch and local staging for v0.1 because
selection is tied to kubeadm intent, generated confext, OperationRecords, and
Kubernetes skew policy. A later transport can use sysupdate primitives only if
generation spec remains authoritative and kubeadm-aware operation gates still
own activation.

## Producer Workflow

The v0.1 Kubernetes payload producer stays in this repository. The first
release workflow needs one reviewed place where KatlOS runtime-interface
metadata, mkosi inputs, package inventories, sysext content checks, bundle manifest
generation, VM fixtures, and milestone gates can move together. Splitting the
producer before the artifact contract is executable would create a second
release surface without reducing risk for the v0.1 proof.

The immediate release-train outcome is an end-to-end user path: install KatlOS,
select an exact Katl-published Kubernetes bundle, and let the supported
bootstrap operation provision a Kubernetes cluster. Users must not need to
clone Katl, run mkosi, or assemble a sysext bundle themselves. A KatlOS release
without at least one compatible project-published Kubernetes bundle is not a
complete Kubernetes provisioning story.

The in-repository workflow is narrow:

```text
Renovate updates mkosi.profiles/kubernetes-sysext/kubernetes.env
  -> GitHub Actions builds the runtime base needed for compatibility metadata
  -> GitHub Actions builds the Kubernetes sysext for the exact target version
  -> checks verify sysext contents, coherent package versions, and checksums
  -> katl-publish-kubernetes-sysext stages the OCI bundle manifest, layers, and catalog data
  -> the custom OCI artifact is published immutably to ghcr.io/katl-dev/kubernetes
```

The producer consumes Katl runtime compatibility as data, even while it lives in
this repository. That data is the same contract a future split producer must
consume:

```text
runtimeInterface
  Current KatlOS runtime extension contract, for example katl-runtime-1.

architecture
  Target architecture asserted by the runtime root and sysext metadata.

systemd extension identity
  SYSEXT_LEVEL or equivalent extension-release compatibility fields, when used.

required host prerequisites
  Runtime-provided kernel modules, mounts, units, sysctls, and userspace
  services that the Kubernetes payload expects before kubeadm or kubelet runs.

kubeadm config API support
  Kubeadm config API families and Kubernetes skew policy the payload may serve.

build input identity
  Runtime artifact digest, build-input digest, source revision,
  and tool versions needed to reproduce or audit compatibility.
```

The Kubernetes producer does not consume node inventory, node identity,
kubeadm config files, bootstrap tokens, certificates, kubeconfigs, BGP peer
addresses, platform API VIPs, or per-node network configuration. Those inputs
belong to `katlc` render and operation planning. Keeping that line prevents the
producer from becoming a node image or a cluster bootstrap engine.

The producer output is identical whether it remains in this repository or moves
later:

```text
custom KubernetesPayloadBundle manifest
  apiVersion, kind, artifactKind, artifactVersion, payloadVersion,
  kubernetesMinor, architecture, supportedRuntimeInterfaces[],
  supportedKubeadmConfigAPIFamilies[], supportedSourceKubernetesMinors[],
  skewPolicy, sourceRepository, packageVersions, packageLockDigest or
  buildInputDigest, createdAt, signatures[] or explicit unsigned-fixture marker

payload descriptors
  systemd-sysext payload layer with digest, size, media type, role, and file
  name; sidecar metadata and package-provenance descriptors with the same
  digest and size discipline

OCI or static layout
  GHCR OCI manifest/config/layers using the Katl media types defined above, or
  a GitHub Releases/static layout that contains an index, the same custom
  manifest bytes, descriptor files, payload blobs, checksums, and optional
  signatures

static layout paths
  index.json at the source root; bundles/<payloadVersion>/<architecture>/bundle.json
  for the custom manifest; blobs/sha256/<digest> for sysext payloads and
  sidecar metadata; catalog/<kubernetesMinor>.json for discovery; checksums.txt
  and signatures/ when enabled

catalog fragment
  exact payload version, artifact version, architecture, runtime interfaces,
  bundle manifest digest, sysext payload digest, source URL shape, and
  deprecation/retention metadata when applicable
```

Both the in-repository producer and any future split producer must emit these
same manifest bytes, media types, static layout paths, and digest
relationships.

Raw sysext files may remain build outputs or payload layers, but they are not
the producer's stable publication API. A consumer must be able to mirror or move
the bundle without changing the manifest bytes, descriptor digests, sysext
payload digest, or generation metadata recorded after staging.

Generic confext supplements are not part of the v0.1 Kubernetes producer unless
they are safe for every node that selects the payload and their paths and
semantics are declared in the same bundle manifest. Examples could include a
future read-only default kubelet drop-in that is independent of node identity
and cluster topology. The producer must reject or omit anything that depends on
node role, hostname, IP address, bootstrap token, certificate material, kubeadm
InitConfiguration or JoinConfiguration, CNI choice, platform API endpoint, BGP
peer, or operator secret. For v0.1, the practical default is no published
Kubernetes confext supplement.

Node-specific kubeadm and generated confext stay with `katlc` because they are
operation outputs, not redistributable payloads. `katlc` has the validated
install/bootstrap intent, selected bundle digest, node role, node identity,
operator-supplied kubeadm YAML, secret material, generation ID, and
OperationRecord context needed to render and verify those files immediately
before activation. Rendering them in the producer would either bake one node's
state into a shared artifact or force the producer to accept secret and
inventory inputs that belong on the node.

## v0.1 Payload Ownership Contract

The v0.1 product boundary is:

```text
KatlOS base runtime
  immutable OS substrate, boot health, generation activation, persistent-state
  projection, node-local operation agent, runtime prerequisites, and broad
  kernel/hardware support

payload bundles
  immutable application or Kubernetes userland plus metadata, provenance,
  capability declarations, systemd unit declarations, compatibility data, and
  digest-addressed payload descriptors

generated confext
  node-specific desired configuration rendered by katlc from supported typed
  input and selected in the same generation as the payloads it configures

operation records
  mutation intent, preflight evidence, external mutation boundaries, runtime
  diagnostics, and recovery state for actions that cannot be represented by
  immutable files alone
```

Generation metadata remains the authority that binds these pieces together.
The base runtime, selected sysext payloads, generated confexts, kernel command
line, and boot-selection state are switched as one generation. A payload bundle
may provide executable units and static defaults, but it does not become active
until `katlc` stages it under Katl-owned storage, verifies compatibility, writes
a generation spec, and `katl-generation-activate.service` exposes that
generation under `/run/extensions` and `/run/confexts`.

### Base Runtime Responsibilities

The KatlOS runtime rootfs owns the OS services and host capabilities that must
exist before any optional payload is selected. This is the installed node
runtime contract; the live installer image remains smaller and does not inherit
the runtime root's Kubernetes or container-host prerequisites.

```text
systemd boot-complete and deadman health units
katl-generation-activate and runtime handoff units
katlc and katlc-agent
/var and /etc/kubernetes persistent-state projection
systemd-networkd, systemd-resolved, dbus-broker, timesync, SSH recovery access
containerd, crun, and the v0.1 CNI plugin set
kernel modules and sysctls required by Kubernetes and normal installs
host networking prerequisites such as nftables, conntrack, iptables-nft, ipset,
  and ipvsadm when they are needed by kube-proxy, CNI, diagnostics, or
  pre-payload host validation
Katl-owned unit ordering and drop-ins that make payload units safe to run
```

The base runtime declares its capability surface through runtime metadata,
`/usr/lib/os-release` extension compatibility fields, artifact metadata, and
artifact verification gates such as `scripts/check-runtime-root`. The base does
not carry Kubernetes binaries, BIRD binaries, BGP API VIP helpers, Helm, Flux,
Cilium CLI, or arbitrary app daemons as supported runtime userland.

The v0.1 container-runtime boundary is intentionally conservative:
`containerd`, `crun`, required CNI plugins, and the corresponding systemd
wiring remain in the base runtime under ADR-005. They are OS prerequisites for
kubeadm and kubelet activation, not a user-managed container platform. KatlOS
keeps them current with the latest supported KatlOS base/runtime validation
gates. The bundled generic CNI plugin binaries are test/bootstrap prerequisites,
not a managed production CNI; users still install Cilium, Calico, or another
production CNI after bootstrap. Moving the CRI runtime stack into a payload
bundle requires a follow-up decision and proof that kubeadm operations, boot
health, rollback, repair, and image-surface checks still work without a
preinstalled runtime. Until that proof lands, Kubernetes payload bundles may
assume the base runtime provides the container runtime contract.

### Kubernetes Payload Responsibilities

A Kubernetes payload bundle owns immutable Kubernetes userland for one exact
payload version and architecture:

```text
kubeadm
kubelet
kubectl
crictl
version-coupled helper binaries that are required by kubeadm/kubelet packaging
Kubernetes sysext extension-release metadata
package provenance, payload metadata, catalog fragments, and bundle signatures
runtime compatibility and Kubernetes skew declarations
```

The bundle does not own node identity, kubeadm InitConfiguration or
JoinConfiguration, PKI, bootstrap tokens, kubeconfigs, etcd data, CNI policy,
API VIP selection, BGP peer configuration, or generated systemd enablement.
Those are node-local operation outputs rendered by `katlc` into generated
confext or persisted under state partitions.

Some helper tools can appear in both the base runtime and Kubernetes payload
because Fedora package dependencies and bootstrap diagnostics are not perfectly
separable. Ownership is still explicit:

```text
base runtime copy
  prerequisite used for host validation, kube-proxy/CNI support, break-glass
  diagnostics, and operations that must run before the Kubernetes sysext is
  selected

payload copy
  version-coupled or package-pulled helper used by kubeadm, kubelet, or
  Kubernetes-specific diagnostics after the Kubernetes sysext is active
```

`scripts/check-kubernetes-sysext` must not silently move host prerequisites
into the sysext. If a tool is required before payload activation, the check must
assert that the runtime root provides it. If a tool is required only by the
Kubernetes payload after activation, the sysext check may assert it inside the
payload.

### Node Extension Payload Responsibilities

Generic node extension bundles use the same source/ref, manifest digest,
descriptor digest, staged sysext, compatibility, and generation-selection model
as Kubernetes payload bundles. The format is defined in
`docs/internal/node-extension-bundle-format.md`; app behavior is defined by
app-specific contracts such as:

```text
docs/internal/generic-bird-extension-contract.md
docs/internal/bgp-api-vip-extension-contract.md
```

A node extension bundle owns immutable app userland, base app units, helper
binaries, status-helper binaries, declared capabilities, compatibility
requirements, and package provenance. It must not own Kubernetes binaries,
kubeadm configuration, package-manager state, arbitrary operator shell hooks, or
undeclared systemd units.

BIRD and BGP API VIP are separate payloads with a capability relationship.
Generic BIRD owns the BIRD daemon capability and its base service contract. The
BGP API VIP app owns the Kubernetes API VIP schema, VIP networkd config, BIRD
configuration rendered for that app, health-gated route advertisement, and
kubeadm `controlPlaneEndpoint` integration. BGP API VIP may require the BIRD
capability, but it must not make BIRD part of the KatlOS base runtime.

### Generated Confext And Unit Ownership

Generated confext is Katl's node-specific configuration vehicle. It may write
only paths owned by the active typed domain and declared payload contract. For
v0.1 those paths include:

```text
/etc/katl/node.json
/etc/katl/kubeadm/<ref>/config.yaml and declared kubeadm patches
/etc/systemd/network/*.network or *.netdev for supported network domains
/etc/katl/apps/<appID>/config.yaml
/etc/systemd/system/katl-app-<appID>*.d/*.conf
/etc/systemd/system/*.wants or *.requires symlinks only for declared units
```

Payload sysexts may ship base units and static defaults. Generated confext may
add bounded drop-ins, config files, and enablement for declared units. Neither
payloads nor confext may override Katl core boot-health, generation-activation,
runtime-handoff, package-manager, or arbitrary non-Katl units unless a future
decision names that ownership and adds validation.

Systemd ownership is validated before selection:

```text
bundle manifest lists every provided unit and entrypoint unit
generated confext writes only declared drop-in or enablement paths
systemd-analyze verify is used where practical for generated units/drop-ins
required units and mounts from the bundle are present in the base runtime or in
  the same selected payload set
activation phase is compatible with the operation, for example pre-kubeadm for
  BGP API VIP and Kubernetes bootstrap
```

### Capability Validation

Capabilities are declared on both sides of the boundary:

```text
base runtime declares provided OS capabilities
  runtimeInterface, architecture, extension compatibility level, kernel module
  support, required mounts, required services, and host prerequisite commands

payload bundles declare required and provided capabilities
  supportedRuntimeInterfaces, requiredKernelModules, requiredUnits,
  requiredMounts, requiredCapabilities, provided app capabilities, supported
  config schema IDs, operation kinds, and activation phases
```

Selection succeeds only when `katlc` can prove the payload requirements are met
by the installed runtime and by the other payloads selected in the same
generation. Validation must fail closed before writing a generation when a
bundle requires a missing kernel module, missing unit, unsupported runtime
interface, unsupported config schema, or unsupported operation phase.

The same validation model applies to Kubernetes and node extensions. Kubernetes
is a first-class payload type because kubeadm operations and skew policy need
Kubernetes-specific checks, but its acquisition, digest verification,
compatibility validation, staging, generation selection, and activation are the
same pattern used by generic node extension bundles.

### Deliberate v0.1 Non-Generalization

v0.1 deliberately does not generalize these surfaces:

```text
arbitrary raw sysext or confext activation as user input
arbitrary systemd unit injection
package-manager requests in install, runtime, or payload input
user-provided raw BIRD configuration
generic app marketplace behavior beyond the accepted node extension bundle
  contract and the BIRD/BGP API VIP implementations
container-runtime-as-extension beyond the ADR-005 v0.1 base-runtime exception
credentialed private bundle sources, client certificates, bearer tokens, or
  mutable latest selectors
inline secrets in payload manifests or generated confext
Kubernetes add-on lifecycle such as Helm, Flux, Cilium, Rook, CoreDNS, or
  Envoy Gateway management
normal runtime config apply that changes Kubernetes sysext selection on an
  already bootstrapped node
```

Those behaviors require follow-up contracts, operation records, validation
gates, redaction rules, and VM proofs before they can become supported product
surface.

Moving the Kubernetes producer to a separate repository is allowed only after
all of the following are true:

```text
the KubernetesPayloadBundle manifest and layout are implemented and
  compatibility-tested in this repository
katlc fetch, cache, digest verification, runtime compatibility validation, and
  generation selection consume only the published bundle contract
the producer can obtain runtimeInterface and build-input metadata from a
  versioned KatlOS runtime metadata artifact instead of the local build tree
package inventory recording, sysext content verification, catalog generation,
  signature or unsigned-fixture policy, and VM fixture publication have
  equivalent gates in the split repository
cross-repository release ordering is explicit for the v1.36.0 -> v1.36.1 proof
  pair and does not require floating tags or mutable bundle contents
the split demonstrably reduces release coupling, build time, or ownership
  complexity enough to justify the extra publication surface
```

Until those criteria are met, separate-repository production is a future
topology choice, not a v0.1 requirement. A split producer must publish the same
catalog schema and bundle layout and must not weaken runtime compatibility,
package provenance, digest verification, or node-local generated-confext
boundaries.

The north-star split is one shared project-owned producer repository under the
`katl-dev` organization for Kubernetes and other payload bundles minted by the
project, rather than one source repository per payload. `katl-dev/packages` and
`katl-dev/bundles` are candidate repository names; the source repository name
is deliberately not part of the artifact contract. That producer consumes
versioned KatlOS compatibility metadata and publishes independent immutable
bundle namespaces. KatlOS product logic, bundle consumption, generation state,
and node-local configuration remain in `katl-dev/katl`.

## Publication Target

OCI in GHCR is the canonical publication shape. Each project-minted bundle kind
uses a clearly named package; Kubernetes is `ghcr.io/katl-dev/kubernetes`. A
Kubernetes bundle has a human-facing `vMAJOR.MINOR.PATCH-katl.BUILD` tag and a
mandatory `sha256-<bundle-manifest-digest>` resolver tag. End users provide the
Kubernetes version; the matching Katl release resolves it to a readable OCI tag
and immutable manifest digest. Expert operation APIs may still carry an
explicit reference. `katlc` requires the Katl bundle manifest to be the resolved
config. GitHub Releases or a static HTTPS layout may mirror the same bytes
later, but they are not the primary store.

The readable tag deliberately retains the exact Kubernetes patch version.
Renovate's Docker datasource preserves tag precision and treats the hyphenated
suffix as compatibility, so `v1.36.0-katl.1` can advance to
`v1.36.1-katl.1`. The release catalog pairs that readable identity with its
digest pin. Katl separately verifies and records the custom bundle manifest
digest.

The OCI manifest digest is the distribution digest. The sysext payload digest is
still recorded as the activation digest in bundle metadata, catalog data, and
generation records after `katlc` stages the payload locally.

The release-owned compatibility catalog maps exact Kubernetes version to OCI
bundle identity, manifest digest, architectures, and supported KatlOS runtime
interfaces. It is embedded in `katlctl`, is not a ClusterConfig input, and is
updated by a ready auto-merged PR after the producer publishes and verifies a
new immutable bundle. Missing versions and incompatible runtimes fail before an
install or upgrade operation is accepted.

The catalog is authoritative for discovery, not for trust by itself. Consumers
still verify the referenced OCI and sysext digests and, once signing is enabled,
verify the catalog or artifact signatures before staging or activation.

## Version Bumps

Kubernetes patch updates should be ordinary reviewed dependency updates.
Renovate should update the declared target payload and package expectations in
`mkosi.profiles/kubernetes-sysext/kubernetes.env` or its successor. That change
triggers the producer workflow, which builds a new immutable sysext artifact and
catalog entry. A successful `v1.36.1` publication does not replace `v1.36.0`;
both remain addressable by exact payload version and digest until retention
policy removes or deprecates them.

The checked-in release lock carries an artifact revision in addition to the
payload and exact RPM NEVRAs. A Renovate patch update resets that revision to
`1`. A reviewed rebuild of unchanged Kubernetes package inputs increments the
revision manually. The main-branch producer trigger consumes only this lock;
unrelated Katl source changes do not mint a new Kubernetes bundle.

Minor updates, such as `v1.36` to `v1.37`, require the same artifact production
mechanics plus Kubernetes version-skew policy review. Katl should continue to
reject unsupported minor transitions on already bootstrapped nodes until the
kubeadm upgrade gate allows them.

## v0.1 Release Version Policy

v0.1 targets Kubernetes minor `v1.36`. The default release proof pair is exact
payload bundle `v1.36.0` for install/bootstrap and exact next patch bundle
`v1.36.1` for the Kubernetes upgrade proof. The milestone does not target a
floating minor, a scheduled-but-unreleased patch, or whatever the node can
resolve at bootstrap time. If the pair changes, both base and next payload
versions must be already-released `v1.36` patches and must move through a
reviewed Kubernetes input update, bundle rebuild, and VM gate. After that cut,
user-facing install examples, fixture metadata, catalog entries, kubeadm YAML,
and generation records must name the exact `vMAJOR.MINOR.PATCH` payload version
and sysext activation digest.

The release policy intentionally separates three versions:

```text
kubernetes payloadVersion
  Exact Kubernetes patch carried by the sysext, for example v1.36.0.

bundle artifactVersion
  Immutable Katl build/revision identity for the bundle that carries that
  payload, for example v1.36.0-katl.1 or a release-candidate build ID.

katlos runtimeInterface
  Compatibility contract consumed before selection, currently katl-runtime-1.
```

`payloadVersion` is the cluster intent and kubeadm `kubernetesVersion` match.
`artifactVersion` distinguishes rebuilt or release-candidate bundles for the
same payload when provenance, build inputs, or manifest format changes.
`runtimeInterface` decides whether the staged payload may be selected with the
installed KatlOS runtime. Matching KatlOS product versions is not sufficient.

Generic node extension bundles use the same split. The v0.1 BIRD extension
payload is named by the upstream daemon version plus a Katl extension revision,
for example `bird-v2.17.1-katl.1`. The BGP API VIP extension is Katl-owned and
uses a Katl semantic payload version, for example `bgp-api-vip-v0.1.0`. Each
extension bundle still has its own immutable `artifactVersion`, declared
capabilities, supported runtime interfaces, architecture, config handoff paths,
status paths, and health semantics. The detailed reusable extension manifest is
defined with the node extension bundle decision, but raw arbitrary sysext
activation is not a supported user-facing version policy.

Before artifact signing lands, local and CI fixtures may be checksum-only if
they use the same bundle manifest, descriptors, payload digests, package or
build input digest, and runtime compatibility fields as the signed path will
use. The absence of signatures must be explicit fixture metadata, not an
implicit trust downgrade. Published v0.1 release artifacts need the signing
policy decision before they become a stable distribution channel.

## Deferred

The following remain separate backlog items:

```text
production artifact and catalog signing key distribution
release channel and deprecation policy
separate producer repository split
kubeadm-aware Kubernetes upgrade execution
published generic confext supplements, if any are needed
```
