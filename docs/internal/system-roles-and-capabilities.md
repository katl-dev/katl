# System Roles And Deferred Capabilities

Status: current decision, revised to defer capability overlays.

This document defines the day-one Katl node classification model without
embedding a general-purpose templating language in Katl.

Day one uses:

```text
cluster defaults
systemRole defaults
per-node overrides
explicit supported node configuration domains
```

Capability overlays remain a day-2 design topic. They need a clearer input
model, merge model, and test contract before they become user-facing.
The opt-in platform API endpoint routing capability is one concrete deferred
capability and is documented in
`docs/internal/platform-api-endpoint-routing-capability.md`.

This document builds on `docs/internal/supported-node-config-domains.md`: every
rendered output must still land in a supported systemd-native domain or bounded
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

Concrete values such as IP addresses, disk selectors, route metrics, SSH keys,
networkd units, sysctl keys, mount requests, or kubeadm config references remain
direct supported domain configuration. They are not hidden behind roles.

## First Implementation

The first implementation supports built-in system roles only:

```text
control-plane
worker
```

The first implementation does not support capability overlays.

This keeps Katl from pretending to support complex domain behavior like GPUs,
storage placement, routing daemons, ingress nodes, or alternate runtimes before
the corresponding input schema, systemd-native rendering, sysext contract,
runtime integration, and smoke tests exist.

## Merge Order

Per-node materials are rendered from these layers, in order:

```text
1. cluster defaults
2. systemRole defaults
3. node overrides
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

node overrides
  concrete hostname, node name, addresses, disk selectors, and any explicit
  per-node corrections
```

No layer may render outside the supported node configuration domains.

## Conflict Handling

Katl must detect conflicts before rendering per-node materials.

Conflict rules:

```text
same output path produced by two layers
  reject unless it is the same normalized content and the domain allows
  idempotent duplicates

same scalar set to different values by systemRole defaults and node override
  reject unless the domain explicitly allows override semantics

same list item repeated
  normalize and de-duplicate only when the domain defines item identity

node override conflicts with systemRole bootstrap invariants
  reject
```

Examples:

```text
node override changes a control-plane node to a worker kubeadm configRef
  reject unless systemRole is also changed to worker

node override attempts to render into an unsupported domain
  reject
```

## Validation

Cluster plan validation must require:

```text
every node has exactly one systemRole
systemRole is control-plane or worker for the first implementation
all layer merges are deterministic
all rendered domains pass their domain-specific validation
node identity is present or can be derived deterministically
selected KubeadmConfig matches systemRole intent
```

`control-plane` nodes should select kubeadm input containing control-plane
configuration. `worker` nodes should select join/worker input. Katl may validate
this by parsing the referenced native kubeadm YAML, not by relying on the config
name alone.

Applying Kubernetes labels and taints to a live cluster is not hidden in confext
activation. It remains an explicit kubeadm/kubectl-aware action or user-managed
GitOps.

## Deferred Capability Overlays

Capability overlays are deferred to day two.

Future capabilities may model composable node traits or workload affordances,
for example:

```text
nvidia-gpu
ceph-osd
local-storage
bird-router
ingress
```

They must not replace `systemRole`. A future GPU control-plane node and a GPU
worker node must both be expressible.

Before capabilities become user-facing, Katl needs a separate design for:

```text
whether capabilities are built-in, user-defined, or both
input schema and naming rules
merge order relative to cluster defaults, systemRole defaults, and node
  overrides
conflict handling when multiple capabilities touch the same domain
interaction with sysexts and user-managed GitOps
validation and golden-test expectations
examples that do not require arbitrary templates or shell hooks
```

Future capability overlays must compile to supported domains, select an explicit
day-2 sysext/app contract, or remain user GitOps. They must not smuggle
application lifecycle into config rendering.

The platform API endpoint routing capability follows that rule: it is opt-in,
dynamic-routing-oriented, and expected to use an explicit app sysext or bounded
native inputs rather than becoming a hidden `systemRole`.

## Rejected Options

Katl does not embed a general-purpose templating language. There is no Jinja,
Helm, Jsonnet, Starlark, Lua, shell, or arbitrary expression engine in the Katl
cluster-plan format.

Katl does not implement Talos patch compatibility.

Katl does not model GPU, storage, router, ingress, or update-controller behavior
as `systemRole` values. Those are future capabilities, day-2 sysexts, or user
GitOps.

User-side templating remains allowed outside Katl. Users may generate Katl input
with their own tooling, but the Katl API that reaches `katlc` remains explicit
system roles, node overrides, and supported domains for the first
implementation.

## Testing Contract

The compiler and planner need deterministic tests:

```text
golden tests for rendered per-node install materials
golden tests for cluster defaults plus systemRole defaults
negative tests for unknown systemRole
negative tests for missing systemRole
negative tests for systemRole and selected KubeadmConfig mismatch
negative tests for output outside supported domains
```

Golden fixtures should include at least:

```text
single control-plane node
single worker node
cluster defaults plus per-node network overrides
control-plane node with explicit per-node storage settings
worker node with explicit per-node extra disk mount request
```

Tests must compare normalized rendered paths and content. They must not depend
on host-specific absolute paths, current user home directories, Nix store paths,
or mutable package versions.
