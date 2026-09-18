# Kubernetes release delivery

Katl automatically discovers the newest stable Kubernetes patch in each of the
three newest minor branches every hour. Drafts and prereleases are excluded.
Discovery follows all pages of the upstream release listing and compares numeric
versions. Older published artifacts remain available; discovery does not delete
versions or upgrade any cluster. A missed intermediate patch is superseded by
the newest patch in its minor. An exact stable version can also be built through
workflow dispatch.

The policy follows the separation used by
[kubernetes.nix](https://github.com/Zariel/kubernetes.nix): the available versions
update automatically, while operators explicitly select cluster upgrades.

## Candidate identity and recovery

Each version resolves exact kubeadm, kubelet, kubectl and cri-tools RPM versions
from its upstream minor repository. Missing packages fail that version's job;
the next scheduled run retries. Other versions continue independently.

The package selection and Katl build recipe determine a numeric artifact
revision. This revision is a content identity, not an ordering or upgrade
counter. A different package revision or recipe gets a different immutable OCI
tag. An existing candidate is inspected and reused instead of rebuilt, so a
failure after upload can resume verification and promotion without replacing
published bytes. Completed candidates need no further attestation or promotion
on an unchanged scheduled run.

Publication checks the runtime and sysext, verifies the anonymous registry
contents, and verifies GitHub provenance before moving the compatibility tag.
VM bootstrap and upgrade validation in GitHub Actions is deferred until a
capable runner exists. Automated publication therefore establishes artifact
integrity and declared runtime compatibility, not an integrated Kubernetes
lifecycle guarantee for a newly released minor.

## Compatibility selection

The registry's `compatible-VERSION-ARCHITECTURE-RUNTIME` tag points directly to a
verified Kubernetes bundle. The bundle already contains its payload version,
architecture, runtime interfaces and layer descriptors; no separate catalogue
or generated source pull request is needed for delivery. Promotion is per
version, so another version's failure cannot withhold a successful release.

`katlctl` keeps its embedded, digest-pinned selections for offline use. For a
version absent from that snapshot, it reads the promoted bundle's metadata,
checks the requested version and runtime, and carries the resolved immutable
digest into the install bundle or upgrade operation. Nodes still verify all
payload bytes while staging. Subsequent discovery does not change an operation
already holding a digest. Clients predating registry discovery need a one-time
Katl CLI update to use this path.

The compatibility tag is the only mutable reference. Candidate tags and their
content remain immutable. A weekly audit resolves current upstream versions
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
  --payload-version v1.37.0 --artifact-version v1.37.0-katl.REVISION
```

For local candidate preparation, build `Containerfile.mkosi` into the configured
`KATL_MKOSI_IMAGE`, then use `candidate --payload-version VERSION --repoquery
scripts/kubernetes-repoquery`. Candidate JSON supplies the exact build inputs.
The checked-in supported-version manifest remains a development fixture and
legacy release snapshot; adding a version there is not required for discovery
or publication.
