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
intent such as the requested Kubernetes version or catalog reference.

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
and a separate selector. The normalized source/ref shape is:

```text
source
  Absolute HTTPS URL for a Katl Kubernetes bundle catalog, static OCI layout, or
  registry manifest endpoint.

ref
  Exact payload selector `vMAJOR.MINOR.PATCH`, optionally pinned as
  `vMAJOR.MINOR.PATCH@sha256:<bundle-manifest-digest>`.
```

The YAML-facing install or bootstrap intent uses:

```yaml
node:
  bootstrap:
    kubernetesBundleSource: https://ghcr.io/v2/katl/kubernetes-payloads
    kubernetesBundleRef: v1.36.0@sha256:<bundle-manifest-digest>
```

`katlctl cluster bootstrap` and node-local `katlc` operations use the same
fields, exposed as `--kubernetes-source` and `--kubernetes-ref` for the operator
CLI and recorded in the OperationRecord as `kubernetes.source` and
`kubernetes.ref`. Legacy catalog-only field names should be treated as
pre-decision scaffolding and replaced by these fields during implementation.

Examples:

```text
source=https://ghcr.io/v2/katl/kubernetes-payloads
ref=v1.36.0@sha256:<bundle-manifest-digest>

source=https://github.com/katl/releases/download/kubernetes-v1.36.0/oci
ref=v1.36.0@sha256:<bundle-manifest-digest>
```

`source` is a location, not a trust root. `ref` resolves to one exact Katl
custom bundle manifest. A catalog or minor selector may be used for discovery,
but before `katlc` writes a generation, the selected payload is normalized to an
exact `vMAJOR.MINOR.PATCH` payload version, bundle manifest digest, and sysext
payload SHA-256. Generation metadata never records a floating `default`, latest,
or minor-only Kubernetes selection.

Resolver behavior is host-shape specific but produces the same bundle manifest:

```text
GHCR or registry source
  Resolve the exact tag from ref through the OCI distribution API, fetch the
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

The Katl custom bundle manifest is the stable payload identity. Its digest is
the `@sha256:` pin used in `kubernetesBundleRef`, regardless of whether the
bundle is hosted through GHCR or a static HTTPS layout. OCI distribution
manifests, tags, catalogs, and indexes help locate that custom manifest, but
they are not the identity recorded as the bundle manifest digest.

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
v1.36.0
v1.36.0-x86_64
v1.36.0-katl.1-x86_64
```

Tags must never be recorded in generation metadata without resolving to the
bundle manifest digest and sysext payload digest. A release tag must not be
moved after publication. If a payload must be rebuilt for the same Kubernetes
patch, publish a new `artifactVersion` and tag such as
`v1.36.0-katl.2-x86_64`; the `payloadVersion` remains `v1.36.0`.

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

User-facing refs are therefore stable across hosting shapes:

```text
source=https://ghcr.io/v2/katl/kubernetes-payloads
ref=v1.36.0@sha256:<bundle-manifest-digest>

source=https://github.com/katl/releases/download/kubernetes-v1.36.0/oci
ref=v1.36.0@sha256:<bundle-manifest-digest>

source=https://mirror.example.invalid/katl/kubernetes
ref=v1.36.0@sha256:<same-bundle-manifest-digest>
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
metadata, mkosi inputs, package locks, sysext content checks, bundle manifest
generation, VM fixtures, and milestone gates can move together. Splitting the
producer before the artifact contract is executable would create a second
release surface without reducing risk for the v0.1 proof.

The in-repository workflow is narrow:

```text
Renovate updates mkosi.profiles/kubernetes-sysext/kubernetes.env
  -> GitHub Actions builds the runtime base needed for compatibility metadata
  -> GitHub Actions builds the Kubernetes sysext for the exact target version
  -> checks verify sysext contents, metadata, package locks, and checksums
  -> katl-publish-kubernetes-sysext stages the OCI bundle manifest, layers, and catalog data
  -> GHCR or GitHub Releases-hosted OCI artifacts are published immutably
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
  Runtime artifact digest, package-lock or build-input digest, source revision,
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

Moving the Kubernetes producer to a separate repository is allowed only after
all of the following are true:

```text
the KubernetesPayloadBundle manifest and layout are implemented and
  compatibility-tested in this repository
katlc fetch, cache, digest verification, runtime compatibility validation, and
  generation selection consume only the published bundle contract
the producer can obtain runtimeInterface and build-input metadata from a
  versioned KatlOS runtime metadata artifact instead of the local build tree
package-lock refresh, sysext content verification, catalog generation,
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

## Publication Target

OCI is the preferred publication shape for forward compatibility. GHCR is the
natural registry target. GitHub Releases may still host the same bundle as a
static OCI layout, index, or mirrored artifact set for simple HTTPS retrieval
and inspection. In both cases, the object users reference is the bundle/catalog
endpoint, not a raw sysext blob.

The OCI manifest digest is the distribution digest. The sysext payload digest is
still recorded as the activation digest in bundle metadata, catalog data, and
generation records after `katlc` stages the payload locally.

The catalog is authoritative for discovery, not for trust by itself. Consumers
must still verify the referenced sysext digest and, once signing is enabled,
verify the catalog or artifact signatures before staging or activation.

## Version Bumps

Kubernetes patch updates should be ordinary reviewed dependency updates.
Renovate should update the declared target payload and package expectations in
`mkosi.profiles/kubernetes-sysext/kubernetes.env` or its successor. That change
triggers the producer workflow, which builds a new immutable sysext artifact and
catalog entry. A successful `v1.36.1` publication does not replace `v1.36.0`;
both remain addressable by exact payload version and digest until retention
policy removes or deprecates them.

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
reviewed package-lock update, bundle rebuild, and VM gate. After that cut,
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
same payload when provenance, package locks, or manifest format changes.
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
they use the same bundle manifest, descriptors, payload digests, package lock or
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
