# ADR-006: Cluster manifests use explicit layers and node classes

Status: accepted.

Date: 2026-06-20.

## Context

Katl needs a user-facing cluster manifest that can describe a small homelab
without forcing users to repeat the same hardware and node configuration on
every host. A common example is a cluster with several hosts of one hardware
model and several hosts of another, such as three MS-01 nodes and three MSA2
nodes. Another common case is simpler: every host is the same and only node
names, addresses, and disk identities differ.

The current cluster plan already has three layers:

```text
cluster defaults
systemRole defaults
per-node overrides
```

That is enough for shared SSH keys, shared network configuration, and
control-plane or worker bootstrap defaults. It is not enough for reusable
hardware-specific configuration without duplicating those fields in every node.

Katl should solve that directly in the Katl manifest model. It should not add a
general template language, range expansion, hardware auto-detection, or a hidden
provisioning system. The user-facing manifest that reaches `katlc` should remain
explicit, deterministic, and easy to validate before destructive work.

## Decision

Cluster manifests support named node classes.

The concrete v0.1 source manifest field contract is documented in
`docs/internal/cluster-manifest-contract.md`.

The user-facing cluster plan layer order is:

```text
1. cluster defaults
2. node class
3. systemRole defaults
4. per-node overrides
```

`spec.nodeClasses` is a map from a user-chosen class name to a bounded node
layer. A node may reference at most one class in v0.1:

```yaml
spec:
  nodeClasses:
    ms01:
      networkd:
        files:
          - name: 10-lan.network
            content: |
              [Match]
              Name=enp1s0

              [Network]
              DHCP=yes
      install:
        targetDiskDefaults:
          minSizeMiB: 32768
      kubernetes:
        nodeLabels:
          katl.dev/hardware-class: ms01

  nodes:
    - name: cp-1
      systemRole: control-plane
      nodeClass: ms01
      overrides:
        install:
          targetDisk:
            serial: MS01_ROOT_001
        bootstrap:
          address: 192.0.2.11
```

Node classes are user-declared. Katl does not infer a class from DMI, PCI IDs,
disk model strings, MAC OUIs, or any other hardware fact in v0.1. Hardware facts
may be used to validate a selected target, but not to pick a class implicitly.

Node classes may set only supported node configuration domains. For v0.1 that
means the same domains already accepted by cluster defaults and node overrides,
plus one explicit installer convenience: `install.targetDiskDefaults`.

`install.targetDiskDefaults` carries safe non-identifying constraints for the
target disk selector, such as `minSizeMiB`. It does not select a destructive
target by itself. Per-machine disk identity, such as `byID`, `wwn`, or `serial`,
belongs in the node override unless a later design defines a safer dedicated
inventory source. A rendered install manifest must still contain one concrete,
unambiguous target disk selector before `katlos-install` may mutate disks.

The v0.1 implementation must reject:

```text
unknown nodeClass references
multiple node classes on one node
class-level destructive target disk identity that applies to multiple nodes
conflicting files, labels, taints, extra disks, or scalar fields across layers
unsupported fields or host-local template paths
implicit hardware class detection
range expansion, globbing, shell, Jinja, Helm, Jsonnet, Starlark, Lua, or other
  embedded template/expression languages
```

All layer merges must be deterministic. Lists and maps merge only where the
domain defines identity and conflict behavior. Otherwise the compiler rejects
conflicts before writing install material.

For clusters where every host is the same, users should use `spec.defaults`
first and add node classes only when there is a meaningful model, chassis,
network, or storage distinction.

## Consequences

Small homelab manifests can stay concise without becoming generated code. A
mixed-hardware cluster can put common MS-01 and MSA2 facts in named classes while
keeping node identity, address, role, and disk identity explicit per node.

The manifest remains stable input to `katlc`. Users may still generate Katl
manifests with their own tooling, but Katl itself owns validation of explicit
layers rather than evaluating arbitrary templates.

Destructive install safety remains tied to concrete target selection. A node
class may reduce repetition, but it cannot make Katl choose a disk by model or
size alone and then wipe it.

System role stays about Kubernetes lifecycle intent, not hardware. Hardware
classes do not become additional system roles such as `storage`, `gpu`, or
`router`. Those remain future capabilities, node app extensions, or user GitOps
depending on the feature.

The compiler and docs need examples for both common cases:

```text
one hardware class for all nodes, mostly using spec.defaults
mixed classes such as ms01 and msa2 with per-node disk identities
```

## Follow-Up

The next work after this ADR is:

```text
update the cluster plan schema and docs to expose nodeClasses and nodeClass
implement node-class layer merging and targetDiskDefaults rendering
add golden tests for all-same-hardware and mixed MS-01/MSA2 style manifests
add negative tests for unknown classes, unsafe disk defaults, and layer conflicts
ensure generated per-node install manifests remain explicit and deterministic
```
