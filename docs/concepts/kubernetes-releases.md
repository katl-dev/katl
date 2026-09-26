# Kubernetes release delivery

Katl automatically discovers the newest stable Kubernetes patch in each of the
three newest minor branches every hour. Drafts and prereleases are excluded.
Discovery follows all pages of the upstream release listing and compares numeric
versions. Older published artifacts remain available; discovery does not delete
versions or upgrade any cluster. A missed intermediate patch is superseded by
the newest patch in its minor. An exact stable version can also be built through
workflow dispatch.

Publication does not upgrade any cluster. Operators select a version in
`ClusterConfig` and start the upgrade explicitly.

## Publication and recovery

Each version resolves exact kubeadm, kubelet, kubectl and cri-tools RPM versions
from its upstream minor repository. Missing packages fail that version's job;
the next scheduled run retries. Other versions continue independently.

Each upstream patch has one immutable publication, exposed as an OCI tag such
as `v1.37.0`. Katl releases, build recipe changes and RPM rebuilds do not create
additional Kubernetes releases. Pull requests still validate affected build
recipes; publication runs through scheduled upstream discovery or explicit
workflow dispatch, not pushes to Katl main.

The producer checks for a published version before resolving packages or
building images. Existing publications are reused, including bundles published
under the older numeric revision scheme. Missing short version tags are added
to those same verified digests. A partially uploaded candidate resumes
verification without replacing its bytes. Publication refuses to move an
existing version or compatibility tag to a different digest.

New bundles use an immutable build tag such as `v1.37.0-1`, alongside the short
version tag `v1.37.0`. The build suffix stays at `1` under the current policy.
Existing publications gain both aliases without changing their metadata or
digest. The reference format requires a compatible Katl CLI and node release.
A runtime compatibility change requires an explicit release policy decision;
ordinary recipe changes do not make that decision.

Publication checks the runtime and sysext, verifies the anonymous registry
contents, and verifies GitHub provenance before moving the compatibility tag.
VM bootstrap and upgrade validation in GitHub Actions is deferred until a
capable runner exists. Automated publication therefore establishes artifact
integrity and declared runtime compatibility, not an integrated Kubernetes
lifecycle guarantee for a newly released minor.

## Compatibility selection

The registry's `vVERSION` tag points directly to a verified Kubernetes bundle.
The resolver also supports existing compatibility aliases during
migration. The bundle contains its payload version, architecture, runtime
interfaces, and layer descriptors; no separate catalog
or generated source pull request is needed for delivery. Promotion is per
version, so another version's failure cannot withhold a successful release.

`katlctl` keeps its embedded, digest-pinned selections for offline use. For a
version absent from that snapshot, it reads the promoted bundle's metadata,
checks the requested version and runtime, and carries the resolved immutable
`repository@sha256:…` reference into the install bundle or upgrade operation.
The Kubernetes version is recorded separately and checked against fetched bundle
metadata; tags are never used to infer the payload version. Nodes still verify all
payload bytes while staging. Subsequent discovery does not change an operation
already holding a digest. Clients predating registry discovery need a one-time
Katl CLI update to use this path.

Version, compatibility and candidate tags remain immutable once published.
A weekly audit resolves current upstream versions
through the same compatibility path used by operators, checks anonymous
availability and verifies provenance; an upstream release that has not reached
promotion is reported as a failure rather than silently excluded from the audit.

## Developer validation

The producer has a stable pull-request check. Image builds run when the recipe
changes; unrelated pull requests keep the check without rebuilding images.
Normal development unit tests cover discovery pagination and release filtering,
metadata compatibility and immutable selection. Publication and promotion run
only from main. Manual dispatch defaults to validation without publication.

A release investigation can use read-only commands:

```sh
go run ./cmd/katl-kubernetes-release discover
go run ./cmd/katl-kubernetes-release resolve --payload-version v1.37.0
go run ./cmd/katl-kubernetes-release inspect \
  --payload-version v1.37.0
```

For local candidate preparation, build `Containerfile.mkosi` into the configured
`KATL_MKOSI_IMAGE`, then use `candidate --payload-version VERSION --repoquery
scripts/kubernetes-repoquery`. Candidate JSON supplies the exact build inputs.
The checked-in supported-version manifest remains a development fixture and
legacy release snapshot; adding a version there is not required for discovery
or publication.
