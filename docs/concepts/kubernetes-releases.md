# Kubernetes release delivery

The KatlOS image does not contain Kubernetes. Katl ships each Kubernetes
release separately as a *Kubernetes bundle*, delivered as a system extension,
and a node adds the bundle to a new [generation](generations.md) during
bootstrap, join, or a Kubernetes upgrade. This page explains which bundles
exist and how Katl selects one;
[Upgrade Kubernetes](../operations/upgrade-kubernetes.md) describes the rollout
procedure.

## Published versions

Katl publishes immutable Kubernetes bundles for the newest stable patches of the
three newest upstream minor branches, and older published versions remain
available. The build schedule can skip an intermediate patch if upstream has
already published a newer patch from the same branch.

A Kubernetes bundle has one exact version, such as `v1.36.1`, and declares which
KatlOS runtimes can use it. Publication checks artifact integrity and that
declared runtime compatibility. Check the release guidance before using a
Kubernetes minor that has not been qualified through the KatlOS bootstrap and
upgrade journeys.

## Version selection

The value of `spec.kubernetes.version` in `cluster.yaml` selects the exact
Kubernetes version. Publishing a newer version leaves that selection in place
until you edit the file and start an upgrade.

The `katlctl` from your KatlOS release resolves the selected version and checks
that its Kubernetes bundle fits the KatlOS runtime; the nodes then fetch and
verify the bundle. The CLI can resolve the versions in its built-in catalog
without contacting the registry. To resolve a newer published version, the
workstation needs access to `ghcr.io` while it plans the operation. The nodes
fetch the Kubernetes bundle from `ghcr.io` themselves, so they need registry and CA
access to it; air-gapped or registry-mirrored delivery is not supported.
