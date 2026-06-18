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

A user who wants Kubernetes `v1.36.1` installs a compatible KatlOS image and
supplies `katlc` with an HTTPS source for the Kubernetes payload bundle, such as
a GHCR OCI reference endpoint or a GitHub Releases-hosted OCI layout/catalog,
together with an exact selection such as `v1.36.1`. The explicit bootstrap
operation later asks `katlc` to fetch the bundle, verify the Katl custom
manifest and payload digests, stage the sysext under Katl-owned storage, create
generation 1, select the staged sysext, render node-specific generated confext,
run kubeadm, and commit only after operation health checks pass.

A user who wants a fresh cluster on Kubernetes `v1.36.2` can use the same
KatlOS install image when runtime compatibility permits it, but supplies an
HTTPS source/ref that resolves to the `v1.36.2` bundle. `v1.36.1` and
`v1.36.2` remain separately addressable by exact payload version and digest.

Upgrading an already bootstrapped node from `v1.36.1` to `v1.36.2` is a
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
catalog entry. A successful `v1.36.2` publication does not replace `v1.36.1`;
both remain addressable by exact payload version and digest until retention
policy removes or deprecates them.

Minor updates, such as `v1.36` to `v1.37`, require the same artifact production
mechanics plus Kubernetes version-skew policy review. Katl should continue to
reject unsupported minor transitions on already bootstrapped nodes until the
kubeadm upgrade gate allows them.

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
