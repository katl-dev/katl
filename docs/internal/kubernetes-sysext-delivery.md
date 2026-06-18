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
user-facing HTTPS source/ref syntax for `katlc` and `katlctl`
remote bundle/catalog fetch and node-local retention
artifact and catalog signing policy
release channel and deprecation policy
separate producer repository split
kubeadm-aware Kubernetes upgrade execution
published generic confext supplements, if any are needed
```
