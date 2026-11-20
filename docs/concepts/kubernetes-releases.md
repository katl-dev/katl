# Kubernetes release delivery

Use this page to choose a Kubernetes version for an install or upgrade.
The value of `spec.kubernetes.version` in `cluster.yaml` selects the exact
Kubernetes version. Publishing a newer version leaves that selection in place
until you edit the file and start an upgrade.

## Choose a version

Katl publishes immutable bundles for the newest stable patches of the three
newest upstream minor branches. Older published versions remain available.
The build schedule can skip an intermediate patch if upstream has already
published a newer patch from the same branch.

A bundle has one exact version, such as `v1.36.1`, and declares which KatlOS
runtimes can use it. Use the `katlctl` that matches your KatlOS release. The
CLI resolves the version and checks that its bundle fits your KatlOS runtime.
The nodes fetch and verify the selected bundle.

## Compatibility selection

The CLI can resolve the versions in its built-in catalog without contacting
the registry. To resolve a newer published version, the workstation needs
access to `ghcr.io` while it plans the operation. Nodes need access to fetch
the bundle unless you supply it through a supported local mechanism.

Publication checks artifact integrity and declared runtime compatibility.
Check the release guidance before using a Kubernetes minor that has not been
qualified through the KatlOS bootstrap and upgrade journeys.

When you are ready to change the version of an existing cluster, follow
[Upgrade Kubernetes](../operations/upgrade-kubernetes.md) and inspect the
plan before you run the rollout.
