# Node Extension Bundle Format

Status: accepted format for reusable optional node extension bundles.

Katl node extensions are optional host-side application payloads selected by
Katl generations. Examples include the future BIRD routing helper and BGP API
VIP helper. This format defines how those extension payloads are packaged,
published, fetched, validated, cached, and selected. It builds on
`docs/internal/node-app-sysext-contract.md` and uses the same digest and
publication principles as `docs/internal/kubernetes-sysext-delivery.md` without
making the extension format Kubernetes-specific.

## Decision

The durable user-facing artifact is a node extension bundle. It is not a raw
sysext path, not an arbitrary systemd unit package, not a package-manager
request, not a confext patch, and not a way to smuggle cluster add-ons into
KatlOS. A bundle contains one app sysext payload plus the metadata needed for
`katlc` to decide whether that payload may be selected in a generation.

The bundle manifest digest is the stable source/ref pin. The sysext payload
digest remains the activation digest recorded in generation metadata after the
payload is staged locally.

## Source And Ref

Node extension acquisition uses the same normalized source/ref pattern as
Kubernetes payload bundles:

```text
source
  Absolute HTTPS URL for a Katl node extension bundle catalog, static OCI
  layout, or registry manifest endpoint.

ref
  Exact extension selector
  <appID>/<payloadVersion>@sha256:<bundle-manifest-digest>.
```

Examples:

```text
source=https://ghcr.io/v2/katl/node-extensions
ref=bird/bird-v2.17.1-katl.1@sha256:<bundle-manifest-digest>

source=https://github.com/katl/releases/download/node-extensions-v0.1/oci
ref=bgp-api-vip/bgp-api-vip-v0.1.0@sha256:<bundle-manifest-digest>
```

Tags and catalog aliases may help discovery, but before `katlc` writes a
generation, every selected extension is normalized to `appID`, exact
`payloadVersion`, `artifactVersion`, bundle manifest digest, sysext payload
digest, architecture, and supported runtime interface.

## Manifest

The custom manifest media type is:

```text
application/vnd.katl.node-extension.bundle.v1+json
```

The manifest is canonical JSON for digest purposes: UTF-8, deterministic object
key order from Katl tooling, lowercase `sha256:<hex>` digests, integer byte
sizes, RFC 3339 timestamps, and no mutable URL freshness or latest-alias fields.
The manifest does not contain its own digest or OCI distribution digest; those
values are computed over the manifest bytes and recorded in refs, OCI
descriptors, indexes, catalogs, cache paths, and generation metadata.

The manifest schema is:

```text
apiVersion: payload.katl.dev/v1alpha1
kind: NodeExtensionBundle
appID
artifactKind: katl.node-app-sysext.v1
artifactVersion
payloadVersion
architecture
displayName
description

capabilities[]
  name
  version
  configSchemaIDs[]
  operationKinds[]

compatibility
  supportedRuntimeInterfaces[]
  minKatlOSVersion, when needed
  maxKatlOSVersion, when needed
  requiredKernelModules[]
  requiredUnits[]
  requiredMounts[]
  requiredCapabilities[]
  activationPhases[]

systemd
  extensionID
  extensionVersion
  sysextLevel, when used
  providedUnits[]
  entrypointUnits[]
  readinessUnits[]
  orderingRequirements[]

configuration
  configHandoffPaths[]
  generatedDropInPaths[]
  supportedConfigSchemaIDs[]
  secretRefKinds[]

status
  liveStatusPath
  statusSchemaID
  durableSnapshotPath
  redactionVersion
  healthStates[]

rollback
  failClosedActions[]
  liveRollbackSupported
  requiresRebootForRollback
  externalStateWarning

payloads[]
  role: systemd-sysext
  mediaType
  digest
  sizeBytes
  fileName
  annotations

metadata[]
  role: package-provenance | catalog-fragment | signature | sbom | checksum
  mediaType
  digest
  sizeBytes
  fileName
  annotations

provenance
  sourceRepository
  sourceRevision
  buildInputDigest or packageLockDigest
  createdAt

signatures[] or explicit unsigned-fixture marker
```

`appID` is a stable lower-case safe path segment such as `bird` or
`bgp-api-vip`. It must not contain path separators, traversal, absolute paths,
uppercase-only branding, or names reserved by Katl core services.

`payloadVersion` names the app payload API or upstream daemon payload. For
BIRD, the expected shape is an upstream daemon version plus Katl extension
revision, such as `bird-v2.17.1-katl.1`. For a Katl-owned helper, the expected
shape is a Katl semantic payload version, such as `bgp-api-vip-v0.1.0`.
`artifactVersion` identifies the immutable bundle build that carries that
payload.

## Descriptors And Media Types

Required descriptor roles and media types are:

```text
systemd-sysext payload
  role: systemd-sysext
  mediaType: application/vnd.katl.sysext.raw.v1
  fileName: katl-node-extension-<appID>-<payloadVersion>-<architecture>.sysext.raw

package provenance
  role: package-provenance
  mediaType: application/vnd.katl.package-provenance.v1+json
  fileName: package-provenance.json

catalog fragment
  role: catalog-fragment
  mediaType: application/vnd.katl.node-extension.catalog.entry.v1+json
  fileName: catalog-entry.json
```

Optional descriptor roles are:

```text
sbom
  mediaType: application/spdx+json

checksum
  mediaType: text/plain

signature
  mediaType: application/vnd.dev.sigstore.bundle.v0.3+json or the selected
  signing-envelope media type
```

No descriptor may point outside the bundle by absolute path. Descriptor
digests and sizes are verified before any payload is cached or selected.

## OCI And Static Layout

GHCR publication uses an OCI image manifest:

```text
artifactType: application/vnd.katl.node-extension.bundle.v1
config.mediaType: application/vnd.katl.node-extension.bundle.v1+json
config.digest: sha256:<bundle-manifest-digest>
layers[]: sysext payload and metadata descriptors from the custom manifest
annotations:
  dev.katl.extension.app-id: <appID>
  dev.katl.extension.payload-version: <payloadVersion>
  dev.katl.extension.artifact-version: <artifactVersion>
  dev.katl.architecture: <architecture>
  dev.katl.bundle.manifest.digest: sha256:<bundle-manifest-digest>
  dev.katl.sysext.payload.digest: sha256:<sysext-payload-digest>
```

Tags are discovery aliases only:

```text
bird-v2.17.1-katl.1-x86_64
bird-v2.17.1-katl.1
bgp-api-vip-v0.1.0-x86_64
```

Tags must not be moved after publication. Rebuilding the same payload requires a
new `artifactVersion`; changing app behavior or supported config capability
requires a new `payloadVersion` or capability version.

The GitHub Releases or local fixture static layout uses identical custom
manifest bytes:

```text
index.json
catalog/
  node-extensions.json
  <appID>.json
bundles/
  <appID>/
    <payloadVersion>/
      <architecture>/
        bundle.json
        catalog-entry.json
        package-provenance.json
blobs/
  sha256/
    <bundle-manifest-digest>
    <sysext-payload-digest>
    <package-provenance-digest>
checksums.txt
signatures/
  <bundle-manifest-digest>.sigstore.json
```

`bundles/<appID>/<payloadVersion>/<architecture>/bundle.json` and
`blobs/sha256/<bundle-manifest-digest>` contain identical bytes. Local and VM
fixtures must use this same shape; loose raw sysext files are not valid bundle
sources.

The source root `index.json` records:

```text
apiVersion: payload.katl.dev/v1alpha1
kind: NodeExtensionBundleIndex
entries[]
  appID
  payloadVersion
  artifactVersion
  architecture
  bundleManifestDigest
  bundleManifestPath
  sysextPayloadDigest
  supportedRuntimeInterfaces[]
  capabilities[]
  catalogEntryPath
  deprecated
```

Catalog documents are discovery views. They may be signed, but catalog
signatures prove only the listing. `katlc` still verifies the selected bundle
manifest and every descriptor digest before staging.

Mirrors may rewrite source URLs and catalog paths, but must preserve manifest
bytes, descriptor digests, sizes, payload blobs, capability metadata, runtime
compatibility metadata, and artifact/payload versions. Repacking or
recompressing any descriptor creates a new bundle manifest digest.

## Config Ownership

The app sysext owns executable files, base systemd units, extension-release
metadata, and read-only app defaults. Node-specific app configuration is
generated confext rendered by `katlc` from supported typed config domains.

Bundle manifests may declare only Katl-owned configuration handoff paths:

```text
/etc/katl/apps/<appID>/config.yaml
/etc/systemd/system/katl-app-<appID>*.d/
```

An app-specific decision may add additional paths only under Katl-owned
namespaces or under a native domain that Katl already owns. The bundle must not
declare arbitrary `/etc`, kubeadm, CNI, Kubernetes object, package-manager, or
operator-selected paths. Secret values are never inline bundle metadata; a
bundle may declare `secretRefKinds[]` only after a separate secret
materialization design defines storage, redaction, and rotation.

## Status And Health Ownership

The default live status path is:

```text
/run/katl/apps/<appID>/status.json
```

The durable operation snapshot path is:

```text
/var/lib/katl/operations/<operation-id>/apps/<appID>/status.json
```

The sysext payload may provide the daemon and status writer. `katlc` owns
validation of the declared status schema, redaction policy, health states,
readiness units, and durable snapshots. Status content is not a trust root for
selecting a bundle; selection is decided by manifest compatibility, digests,
operation intent, and generation metadata.

## Cache And Retention

`katlc` caches verified node extension bundles under Katl-owned artifact
storage:

```text
/var/lib/katl/artifacts/node-extensions/
  bundles/sha256-<bundle-manifest-digest>/
    bundle.json
    catalog-entry.json
    package-provenance.json
  sysext/sha256-<sysext-payload-digest>/
    katl-node-extension-<appID>-<payloadVersion>-<architecture>.sysext.raw
  index.json
  pins.json
  locks/
```

Retention keeps every extension bundle or payload referenced by:

```text
the current selected generation
the previous known-good generation
any candidate, rollback, live-apply, or in-progress OperationRecord
any operator-pinned appID/payloadVersion
```

Cleanup removes incomplete temporary entries first, then unreferenced bundle
metadata, then unreferenced sysext payloads only after no retained bundle
references them. Cleanup must never rewrite generation metadata or remove a
payload selected by a valid generation.

## User-Provided Extensions

User-provided extensions are allowed only through the same bundle contract.
They are not a raw sysext escape hatch. A user-provided bundle must:

```text
use the NodeExtensionBundle manifest schema
declare a stable appID and artifactKind `katl.node-app-sysext.v1`
use Katl-scoped units, status paths, and config handoff paths
declare capabilities and supported config schema IDs
declare runtime compatibility and host prerequisites
provide payload and metadata descriptors with digest and size
include signing material or an explicit unsigned-fixture marker
pass the same validation as Katl-provided bundles
```

`katlc` rejects raw sysext paths, arbitrary unit files, arbitrary package names,
global systemd extension directories, inline secrets, unknown capabilities,
unsupported config schemas, unscoped paths, mutable tags without bundle digest
pins, and app manifests that claim ownership of Katl core services.

## Relationship To Kubernetes Bundles

Node extension bundles share these Kubernetes payload bundle principles:

```text
custom manifest digest is the source/ref pin
raw sysext payload is a layer, not the stable user-facing API
sysext payload SHA-256 is the generation activation digest
OCI and static layouts carry the same manifest bytes and descriptor digests
runtime compatibility metadata gates selection
catalogs are discovery documents, not sufficient activation authority
local fixtures use the same manifest and digest shape as published bundles
```

They differ from Kubernetes payload bundles in these ways:

```text
appID and capability metadata are required
app-specific config handoff, status, health, rollback, and fail-closed metadata
  are required
Kubernetes skew policy and kubeadm config API support are not generic fields
node-specific app config is rendered by app-specific Katl config domains, not
  by the bundle producer
```

The BIRD and BGP API VIP Beads must define their own app-specific capability,
input, status, operation, and VM-test contracts on top of this reusable bundle
format.

## Non-Goals

This format does not make any concrete extension user-facing. It does not
define the BIRD app, the BGP API VIP app, CNI lifecycle, Kubernetes add-on
management, arbitrary routing daemon passthrough, arbitrary systemd units, or a
plugin marketplace. Those require app-specific contracts and tests.
