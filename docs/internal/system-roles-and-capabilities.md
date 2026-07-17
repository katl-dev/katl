# Control-plane Participation And Deferred Capabilities

Status: current decision, revised for the contracted ClusterConfig API.

This document defines Katl's day-one Kubernetes node classification without
embedding compiler profiles or a general-purpose templating language in the
operator API.

## Public Decision

`nodes[].controlPlane` is the only ClusterConfig choice that determines a
node's Kubernetes bootstrap role:

```yaml
nodes:
  - name: cp-1
    controlPlane: true
  - name: worker-1
```

`true` means that the node joins the Kubernetes control plane. Omission or
`false` means worker. A cluster must contain at least one node with
`controlPlane: true`; Katl does not silently promote the first node.

Katl compiles that choice into its internal `control-plane` or `worker` system
role. Internal install manifests, inventories, node metadata, operation state,
and kubeadm planning may retain that enum because those documents are generated
Katl state rather than operator-authored ClusterConfig.

The choice affects kubeadm input and lifecycle ordering only. It does not imply
CNI, ingress, routing, storage, GPU, workload scheduling, or other application
behavior. Control-plane scheduling remains governed by Kubernetes taints and
operator policy.

## Authoring And Merge Model

Day-one configuration has two layers:

```text
1. shared spec.defaults
2. flat per-node settings
```

Concrete addresses, disk selectors, networkd units, SSH keys, labels, taints,
and supported storage choices remain direct configuration. Katl selects
role-dependent kubeadm state internally; operators do not name profiles or
provide role-default maps.

Conflicting output paths or settings are rejected according to the owning
configuration domain. No layer may render outside the supported node
configuration domains.

## Validation

ClusterConfig validation requires:

```text
at least one node
at least one node with controlPlane: true
the removed systemRole field is rejected rather than treated as an alias
all layer merges are deterministic
all rendered domains pass their domain-specific validation
selected internal kubeadm state matches the derived node role
target disk identity is explicit before destructive installation
```

Changing `controlPlane` changes operation-only Kubernetes bootstrap intent. It
must remain visible to runtime planning and cannot be silently applied as an
ordinary live configuration change.

## Deferred Capability Overlays

Composable traits such as GPU, storage, routing, or ingress behavior remain a
day-two design topic. Each needs a real operator workflow, bounded input schema,
systemd-native rendering or sysext contract, lifecycle behavior, status, and
tests before becoming user-facing.

Future capabilities must remain independent of `controlPlane`. A GPU
control-plane node and a GPU worker must both be expressible. Application
lifecycle remains in explicit Katl app contracts or user-managed GitOps.

## Rejected Options

Katl does not expose additional role strings for hypothetical future behavior.
It does not embed Jinja, Helm, Jsonnet, Starlark, Lua, shell, or arbitrary
expressions, and it does not implement Talos patch compatibility.

Node classes, role-default layers, an `overrides` wrapper, and capability
overlays are not part of the current ClusterConfig contract. User-side
templating may generate concrete ClusterConfig YAML, but Katl validates only the
resulting document.

## Testing Contract

Tests cover:

```text
controlPlane: true lowering to the internal control-plane role
omitted controlPlane lowering to worker
rejection when no control-plane node exists
rejection of the removed systemRole alias
role-dependent kubeadm material selection
normalized rendered paths and content for both node kinds
```

Fixtures must not depend on host-specific paths, current home directories, Nix
store paths, or mutable package versions.
