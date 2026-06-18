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

Each published bundle contains, either as OCI descriptors/layers or as an
equivalent static layout while the format is being finalized:

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

A user who wants Kubernetes `v1.36.2` installs a compatible KatlOS image and
supplies `katlc` with an HTTPS source for the Kubernetes payload bundle, such as
a GHCR OCI reference endpoint or a GitHub Releases-hosted OCI layout/catalog,
together with an exact selection such as `v1.36.2`. The explicit bootstrap
operation later asks `katlc` to fetch the bundle, verify the Katl custom
manifest and payload digests, stage the sysext under Katl-owned storage, create
generation 1, select the staged sysext, render node-specific generated confext,
run kubeadm, and commit only after operation health checks pass.

A user who wants a fresh cluster on Kubernetes `v1.36.3` can use the same
KatlOS install image when runtime compatibility permits it, but supplies an
HTTPS source/ref that resolves to the `v1.36.3` bundle. `v1.36.2` and
`v1.36.3` remain separately addressable by exact payload version and digest.

Upgrading an already bootstrapped node from `v1.36.2` to `v1.36.3` is a
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
    kubernetesBundleRef: v1.36.2@sha256:<bundle-manifest-digest>
```

`katlctl cluster bootstrap` and node-local `katlc` operations use the same
fields, exposed as `--kubernetes-source` and `--kubernetes-ref` for the operator
CLI and recorded in the OperationRecord as `kubernetes.source` and
`kubernetes.ref`. Legacy catalog-only field names should be treated as
pre-decision scaffolding and replaced by these fields during implementation.

Examples:

```text
source=https://ghcr.io/v2/katl/kubernetes-payloads
ref=v1.36.2@sha256:<bundle-manifest-digest>

source=https://github.com/katl/releases/download/kubernetes-v1.36.2/oci
ref=v1.36.2@sha256:<bundle-manifest-digest>
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
  Resolve the exact tag from ref through the OCI distribution API, verify that
  the resolved manifest digest matches the pinned digest when present, then
  fetch descriptors and layers by digest.

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

The custom bundle manifest must expose enough fields for node-side selection
before OCI media types are finalized:

```text
apiVersion
kind: KubernetesPayloadBundle
name
artifactKind: katl.kubernetes-payload.v1
artifactVersion
payloadVersion
kubernetesMinor
architecture
bundleManifestDigest
payloads[]
  role: systemd-sysext
  mediaType
  digest
  sizeBytes
  fileName
metadata[]
  role: sysext-metadata | package-provenance | catalog-fragment | signature
  mediaType
  digest
  sizeBytes
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

The sysext payload digest, not the catalog digest, remains the activation digest
stored in generation metadata. The bundle manifest digest is the distribution
pin used to prove the same manifest was fetched again. The catalog is useful for
listing and mirroring; it is not sufficient by itself to stage or activate a
payload.

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

The first producer can live in this repository because the sysext currently
depends on Katl runtime compatibility metadata and the local mkosi build layout.
The workflow should be narrow:

```text
Renovate updates mkosi.profiles/kubernetes-sysext/kubernetes.env
  -> GitHub Actions builds the runtime base needed for compatibility metadata
  -> GitHub Actions builds the Kubernetes sysext for the exact target version
  -> checks verify sysext contents, metadata, package locks, and checksums
  -> katl-publish-kubernetes-sysext stages the OCI bundle manifest, layers, and catalog data
  -> GHCR or GitHub Releases-hosted OCI artifacts are published immutably
```

Moving this producer to a separate repository is desirable once the artifact
contract is stable enough that the producer can consume Katl runtime interface
metadata without importing the whole KatlOS build tree. The split should happen
when it reduces release coupling, not before local VM and artifact validation are
reliable. A separate repo still needs to publish the same catalog schema and
must not weaken Katl runtime compatibility checks.

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
catalog entry. A successful `v1.36.3` publication does not replace `v1.36.2`;
both remain addressable by exact payload version and digest until retention
policy removes or deprecates them.

Minor updates, such as `v1.36` to `v1.37`, require the same artifact production
mechanics plus Kubernetes version-skew policy review. Katl should continue to
reject unsupported minor transitions on already bootstrapped nodes until the
kubeadm upgrade gate allows them.

## v0.1 Release Version Policy

v0.1 targets Kubernetes minor `v1.36`. The release is cut against an exact base
`v1.36.x` payload bundle for install and an exact next patch bundle for the
Kubernetes upgrade proof, not against a floating minor or whatever the node can
resolve at bootstrap time. Development fixtures follow the package lock and
currently resolve the base bundle to `v1.36.2`; the paired upgrade bundle is the
next available `v1.36` patch, expected to be `v1.36.3` when it is published by
the upstream package repository. If a newer patch is selected for the final
release candidate, both base and next payload versions must move through a
reviewed package-lock update, bundle rebuild, and VM gate. After that cut,
user-facing install examples, fixture metadata, catalog entries, kubeadm YAML,
and generation records must name the exact `vMAJOR.MINOR.PATCH` payload version
and sysext activation digest.

The release policy intentionally separates three versions:

```text
kubernetes payloadVersion
  Exact Kubernetes patch carried by the sysext, for example v1.36.2.

bundle artifactVersion
  Immutable Katl build/revision identity for the bundle that carries that
  payload, for example v1.36.2-katl.1 or a release-candidate build ID.

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
custom OCI manifest media types and bundle schema
production artifact and catalog signing key distribution
release channel and deprecation policy
separate producer repository split
kubeadm-aware Kubernetes upgrade execution
published generic confext supplements, if any are needed
```
