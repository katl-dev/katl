# System Roles and Node Capabilities

Status: current decision.

This document defines how Katl represents shared cluster configuration,
kubeadm-oriented node roles, capability overlays, and per-node overrides without
embedding a general-purpose templating language in Katl.

It builds on `docs/internal/supported-node-config-domains.md`: every rendered
output must still land in a supported systemd-native domain or bounded
Katl-owned file. Day-2 application behavior remains in sysexts or user-managed
GitOps.

## Decision

Each node has exactly one `systemRole`.

`systemRole` answers the kubeadm/bootstrap question only:

```text
control-plane
  node is intended to receive kubeadm control-plane input
  may need control-plane storage defaults such as /var/lib/etcd policy
  does not imply CNI, ingress, routing daemon, storage add-on, or GPU behavior

worker
  node is intended to receive kubeadm join/worker input
  does not imply workload labels, taints, storage, GPU, or ingress behavior
```

Nodes may also have zero or more named `capabilities`.

Capabilities are composable configuration overlays. They describe node traits or
workload affordances, not kubeadm bootstrap identity:

```text
nvidia-gpu
ceph-osd
local-storage
bird-router
ingress
```

Capabilities do not replace `systemRole`. A control-plane node can have
`ingress`; a worker can have `local-storage`; either can have no capabilities.
Concrete values such as IP addresses, disk selectors, route metrics, SSH keys,
or kubeadm config references remain direct supported domain configuration.

## First Implementation

The first implementation should support:

```text
built-in system roles
  control-plane
  worker

user-defined capabilities
  names validated as stable DNS-label-like identifiers
  content limited to supported config domains
  no arbitrary file templates or shell hooks

built-in capabilities
  deferred until a concrete tested need exists
```

This keeps Katl from pretending to support complex domain behavior like GPUs or
routing daemons before the corresponding sysext, package, runtime, and smoke
tests exist. A future built-in capability must still compile to supported
domains or select a day-2 sysext contract; it must not smuggle application
lifecycle into config rendering.

## Merge Order

Per-node materials are rendered from these layers, in order:

```text
1. cluster defaults
2. systemRole defaults
3. capability overlays, in declared node order
4. node overrides
```

Later layers override earlier scalar settings only when the domain explicitly
allows override semantics. List and map behavior is domain-specific and must be
documented in the domain implementation.

Recommended defaults:

```text
cluster defaults
  shared DNS, time, SSH/operator keys, common sysctl/modules/tmpfiles policy,
  artifact selections, and common Kubernetes sysext selection

systemRole defaults
  kubeadm configRef defaults, control-plane storage policy, bootstrap labels or
  taints that are needed by kubeadm input

capability overlays
  additional supported-domain configuration such as extra mounts, sysctl,
  modules-load, tmpfiles, or networkd snippets

node overrides
  concrete hostname, node name, addresses, disk selectors, and any explicit
  per-node corrections
```

No layer may render outside the supported node configuration domains.

## Conflict Handling

Katl must detect conflicts before rendering per-node materials.

Conflict rules:

```text
same scalar set to different values by two capabilities
  reject unless the domain defines deterministic priority or merge semantics

same output path produced by two layers
  reject unless it is the same normalized content and the domain allows
  idempotent duplicates

same list item repeated
  normalize and de-duplicate only when the domain defines item identity

capability requires unsupported domain
  reject

capability requires day-2 behavior
  reject in install/runtime config; implement as sysext or user GitOps instead

node override conflicts with systemRole bootstrap invariants
  reject
```

Examples:

```text
two capabilities both set net.ipv4.ip_forward to different values
  reject

two capabilities both request the same modules-load entry
  allowed only if module identity normalizes to the same name

bird-router capability tries to install or manage a BIRD service
  reject until represented by a tested sysext or explicit supported domain

node override changes a control-plane node to a worker kubeadm configRef
  reject unless systemRole is also changed to worker
```

## Validation

Cluster plan validation must require:

```text
every node has exactly one systemRole
systemRole is control-plane or worker for the first implementation
capability names are unique per node after normalization
capability definitions exist before use
capability definitions render only supported domains
all layer merges are deterministic
all rendered domains pass their domain-specific validation
node identity is present or can be derived deterministically
selected KubeadmConfig matches systemRole intent
```

`control-plane` nodes should select kubeadm input containing control-plane
configuration. `worker` nodes should select join/worker input. Katl may validate
this by parsing the referenced native kubeadm YAML, not by relying on the config
name alone.

Capabilities may express metadata that later turns into Kubernetes labels or
taints, but applying those labels and taints to a live cluster is not hidden in
confext activation. It remains an explicit kubeadm/kubectl-aware action or
user-managed GitOps.

## Rejected Options

Katl does not embed a general-purpose templating language. There is no Jinja,
Helm, Jsonnet, Starlark, Lua, shell, or arbitrary expression engine in the Katl
cluster-plan format.

Katl does not implement Talos patch compatibility.

Katl does not model GPU, storage, router, ingress, or update-controller behavior
as `systemRole` values. Those are capabilities, day-2 sysexts, or user GitOps.

User-side templating remains allowed outside Katl. Users may generate Katl input
with their own tooling, but the Katl API that reaches `katlc` remains explicit
roles, capabilities, node overrides, and supported domains.

## Testing Contract

The compiler and planner need deterministic tests:

```text
golden tests for rendered per-node install materials
golden tests for cluster defaults plus systemRole defaults
golden tests for multiple capabilities applied in declared order
negative tests for conflicting capability values
negative tests for unknown systemRole and unknown capability
negative tests for missing role or duplicate capability use
negative tests for capability output outside supported domains
negative tests for systemRole and selected KubeadmConfig mismatch
```

Golden fixtures should include at least:

```text
single control-plane node
single worker node
control-plane plus ingress capability
worker plus local-storage capability
worker plus multiple non-conflicting capabilities
conflicting capabilities rejected before render
```

Tests must compare normalized rendered paths and content. They must not depend
on host-specific absolute paths, current user home directories, Nix store paths,
or mutable package versions.
