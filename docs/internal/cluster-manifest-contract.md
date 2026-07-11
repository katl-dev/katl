# Cluster Manifest Contract

Status: v0.1 reference contract for user-authored cluster config.

This document defines the user-facing cluster manifest shape that
`katlctl config bundle` compiles into a resolved Katl config bundle. It applies
ADR-006 node classes and keeps the source manifest explicit. The compiled
install manifest remains `install.katl.dev/v1alpha1`; users should not author
one compiled install manifest per node for normal workflows.

## Envelope

The v0.1 source manifest kind is:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: homelab
spec: {}
```

`spec` is the only policy-bearing top-level field. Unsupported top-level fields
or unsupported fields under `spec` are rejected before bundle output is written.

Before creating installation media or a bundle, run the non-writing compiler
preflight:

```console
katlctl config validate ./cluster.yaml
```

The command resolves local references, applies every layer, compiles all node
plans, and reports the cluster name, content digests, and resolved node names and
roles. It does not create an output file. Errors identify the source field path,
including node indexes, for example
`spec.nodes[0].overrides.install.targetDisk.byPath`.

Editors and validation tools can consume the exact structural schema shipped by
the installed CLI:

```console
katlctl config schema > cluster-config-v1alpha1.schema.json
```

The JSON Schema is derived from the same Go types as the strict YAML decoder.
The compiler remains authoritative for semantic rules such as destructive disk
guards, Kubernetes selection, local-reference safety, and layer conflicts.

## Layer Model

Every node is rendered from four explicit layers, in this order:

```text
1. spec.defaults
2. spec.nodeClasses[<node.nodeClass>]
3. spec.systemRoleDefaults[<node.systemRole>]
4. nodes[].overrides
```

Later layers may override earlier scalar values only when that configuration
domain explicitly defines override semantics. Lists and maps merge only where
the domain defines identity and conflict handling. Otherwise the compiler
rejects the manifest instead of guessing.

`spec.defaults` contains cluster-wide defaults shared by all nodes.

`spec.nodeClasses` is a map of named hardware/model classes. A node may
reference at most one class with `nodeClass`. Classes are user-declared only;
Katl does not infer classes from DMI, PCI IDs, disk model strings, MAC OUIs, or
other hardware facts in v0.1.

`spec.systemRoleDefaults` is keyed by `control-plane` and `worker`. It carries
role-specific bootstrap and runtime defaults. System role remains Kubernetes
lifecycle intent; it is not a hardware capability system.

`nodes[].overrides` contains concrete per-node identity, address, target disk
identity, and other node-specific corrections.

## Supported Field Shape

The source manifest uses this reference shape:

```yaml
spec:
  defaults:
    install:
      wipeTarget: true
      targetDiskDefaults:
        minSizeMiB: 32768
    identity:
      ssh:
        authorizedKeys: []
    networkd:
      files: []
    kubernetes:
      version: v1.36.1
      bundle: ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:<OCI-manifest-digest>
      kubeadm:
        configRef: worker

  nodeClasses:
    example-class:
      install:
        targetDiskDefaults:
          minSizeMiB: 65536
      networkd:
        files: []
      kubernetes:
        labels: {}
        taints: []

  systemRoleDefaults:
    control-plane:
      kubernetes:
        kubeadm:
          configRef: control-plane
    worker:
      kubernetes:
        kubeadm:
          configRef: worker

  nodes:
    - name: cp-1
      systemRole: control-plane
      nodeClass: example-class
      overrides:
        identity:
          hostname: cp-1
        bootstrap:
          address: 192.0.2.11
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-KATL_CP_1_ROOT
```

The exact domain schemas remain the domain contracts. This reference shape names
the source-manifest layer locations and merge order that those domain schemas
compile from.

## Destructive Install Guard

Every rendered node install material must contain:

```yaml
install:
  wipeTarget: true
  targetDisk:
    byID: /dev/disk/by-id/ata-KATL_NODE_ROOT
```

`install.wipeTarget: true` is the installer guard that authorizes destructive
mutation of the selected target disk. Missing, false, or null values are refused
before disk mutation.

`install.targetDiskDefaults` is allowed only for safe non-identifying
constraints, for example:

```yaml
install:
  targetDiskDefaults:
    minSizeMiB: 32768
```

`targetDiskDefaults` must not select a destructive target by itself. Per-node
target disk identity remains required for destructive install. Stable identity
belongs under `nodes[].overrides.install.targetDisk` and should use `byID`,
`wwn`, or `serial`. Short kernel names such as `/dev/sda` are not valid
destructive selectors.

The compiler rejects class-level or defaults-level target disk identity that
would apply to multiple nodes.

## All Same Hardware Example

This example uses `spec.defaults` for the common hardware policy and keeps only
node identity, address, role, and disk identity under each node.

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: all-nuc
spec:
  defaults:
    install:
      wipeTarget: true
      targetDiskDefaults:
        minSizeMiB: 32768
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
    networkd:
      files:
        - name: 10-lan.network
          content: |
            [Match]
            Name=enp1s0

            [Network]
            DHCP=yes
    kubernetes:
      version: v1.36.1
      bundle: ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

  systemRoleDefaults:
    control-plane:
      kubernetes:
        kubeadm:
          configRef: control-plane
    worker:
      kubernetes:
        kubeadm:
          configRef: worker

  nodes:
    - name: cp-1
      systemRole: control-plane
      overrides:
        identity:
          hostname: cp-1
        bootstrap:
          address: 192.0.2.11
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-NUC_CP_1_ROOT

    - name: worker-1
      systemRole: worker
      overrides:
        identity:
          hostname: worker-1
        bootstrap:
          address: 192.0.2.21
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-NUC_WORKER_1_ROOT
```

## Mixed MS-01/MSA2 Example

This example uses node classes for hardware-specific defaults and still keeps
destructive disk identity per node.

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: mixed-homelab
spec:
  defaults:
    install:
      wipeTarget: true
      targetDiskDefaults:
        minSizeMiB: 32768
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
    kubernetes:
      version: v1.36.1
      bundle: ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

  nodeClasses:
    ms01:
      install:
        targetDiskDefaults:
          minSizeMiB: 65536
      networkd:
        files:
          - name: 10-ms01-lan.network
            content: |
              [Match]
              Name=enp2s0

              [Network]
              DHCP=yes
      kubernetes:
        labels:
          katl.dev/hardware-class: ms01

    msa2:
      install:
        targetDiskDefaults:
          minSizeMiB: 32768
      networkd:
        files:
          - name: 10-msa2-lan.network
            content: |
              [Match]
              Name=end0

              [Network]
              DHCP=yes
      kubernetes:
        labels:
          katl.dev/hardware-class: msa2

  systemRoleDefaults:
    control-plane:
      kubernetes:
        kubeadm:
          configRef: control-plane
    worker:
      kubernetes:
        kubeadm:
          configRef: worker

  nodes:
    - name: ms01-cp-1
      systemRole: control-plane
      nodeClass: ms01
      overrides:
        identity:
          hostname: ms01-cp-1
        bootstrap:
          address: 192.0.2.11
        install:
          targetDisk:
            serial: MS01_CP_1_NVME_ROOT

    - name: msa2-worker-1
      systemRole: worker
      nodeClass: msa2
      overrides:
        identity:
          hostname: msa2-worker-1
        bootstrap:
          address: 192.0.2.21
        install:
          targetDisk:
            serial: MSA2_WORKER_1_NVME_ROOT
```

## Rejected Behavior

Katl v0.1 rejects:

```text
unknown nodeClass references
more than one nodeClass on one node
class-level targetDisk identity that could wipe multiple nodes
template expressions, loops, ranges, or generated node lists
Jinja, Helm, Jsonnet, Starlark, Lua, shell, or arbitrary expression languages
hardware auto-detection for selecting nodeClass
implicit target disk selection from model, size, or class alone
unresolved local file references in compiled bundle output
```

Users may generate a Katl manifest with their own tooling before passing it to
Katl. Katl itself validates only the explicit manifest it receives.
