# ClusterConfig Contract

Status: v1alpha1 reference contract for operator-authored cluster intent.

`ClusterConfig` describes meaningful cluster and node choices. It does not ask
operators to model Katl's compiler, artifact selection, generated profiles,
generations, credentials, or operation bookkeeping. `katlctl` compiles one
source into the per-node material needed by install, configuration, and
bootstrap workflows.

## Envelope

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: homelab
spec: {}
```

Unknown fields are rejected. The API is still alpha, so removed aliases and
unshipped shapes are not retained for compatibility.

Use the compiler and its schema directly:

```console
katlctl config validate ./cluster.yaml
katlctl config schema > cluster-config-v1alpha1.schema.json
```

## Authoring Model

There are two authoring levels:

1. `spec.defaults` for desired behavior shared by every node.
2. Flat entries in `spec.nodes` for concrete node choices.

There are no node classes, system-role default layers, or `overrides` wrapper.
Katl selects its control-plane and worker kubeadm profiles internally from
`controlPlane`. Operators who need a native kubeadm setting that Katl does not
model may supply one bounded cluster-wide kubeadm file and optional patch
directory. They never name or select the generated profiles.

## Supported Shape

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: homelab
spec:
  # Optional; defaults to the first control-plane node address on port 6443.
  controlPlaneEndpoint:
    host: api.home.arpa
    # port: 6443
    # advertisement:
    #   vip: 10.40.0.10
    #   bgp:
    #     localASN: 64512
    #     peers:
    #       - address: 10.0.0.1
    #         asn: 64500

  kubernetes:
    version: v1.36.1
    # Advanced native input, resolved relative to this ClusterConfig.
    # kubeadm:
    #   configFile: ./kubeadm.yaml
    #   patchesDir: ./kubeadm-patches

  defaults:
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAA... operator@home
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
      extraDisks: []
    kubernetes:
      labels: {}
      taints: []

  nodes:
    - name: cp-1
      # Set to true for nodes that join the Kubernetes control plane.
      # Omission means worker.
      controlPlane: true
      bootstrap:
        address: 192.0.2.11
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-KATL_CP_1_ROOT
      kubernetes:
        labels:
          topology.kubernetes.io/zone: rack-a
        taints: []
```

`name` is also the node hostname. A separate hostname alias is deliberately not
part of the contract.

`bootstrap.address` is the operator-reachable address used for installation,
initial Kubernetes bootstrap, and an optional initial workstation context.
It need not be a permanent node identity, but it must remain reachable through
those steps. For DHCP nodes, use a reservation or update the workstation
saved context when the address changes.

`controlPlane: true` is the only public role choice. Omission or `false` means
worker, and at least one node must set it to true. Katl derives its internal
system role, kubeadm material, and lifecycle ordering from this value.

Nodes use a generated DHCP systemd-networkd profile when neither defaults nor
the node supplies native networkd files. Default and node files merge by file
name and conflicting content is rejected.

Kubernetes labels merge by key and taints by their Kubernetes identity.
Conflicting values are rejected instead of silently selecting a layer.

## Install Selection

Each node chooses its own install target. Prefer durable `byID`, `wwn`, or
`serial` selectors; short kernel names such as `/dev/sda` are not valid
destructive selectors.

`targetDiskDefaults` may contain only non-identifying constraints such as
minimum size. It cannot select a disk for several nodes. Extra disks are desired
node storage and therefore remain valid cluster intent.

The decision to execute a destructive install belongs to the install operation,
not ClusterConfig. There is no `wipeTarget` authorization field in this API.

## Deliberately Internal Inputs

The following are not ClusterConfig fields:

- KatlOS image URLs, checksums, local paths, or release descriptors;
- Kubernetes OCI bundle references, catalogs, resolver inputs, or digests;
- named kubeadm profiles, maps, render paths, or config references;
- bootstrap access methods, tokens, or credential references;
- node classes, platform API endpoint helpers, or role-default layers;
- generation IDs, operation IDs, source digests, or validation bookkeeping.

Release media, provisioning commands, workstation contexts, and Katl's
compiler provide these inputs at the operation boundary. For example, PXE
bundle compilation takes KatlOS artifact metadata as command flags while the
same ClusterConfig remains usable for ISO installation.

The bounded exception is `spec.kubernetes.kubeadm`. `configFile` and
`patchesDir` are repository-relative operator inputs that `katlctl` validates
and embeds into the compiled bundle. Katl supplies missing role documents, the
selected Kubernetes version, the containerd CRI socket, safe rendered paths,
and dynamic bootstrap credentials. It rejects unsupported API kinds, unsafe
host paths, symlinks, traversal, and a kubeadm version that conflicts with
`spec.kubernetes.version`.

## Runtime Planning

When a ClusterConfig is rendered for an installed node, Katl includes every
supported desired field in the node change request. Runtime-live fields such as
SSH keys and networkd files can be applied directly. Operation-only fields such
as control-plane participation and role-dependent Kubernetes bootstrap state
remain visible to the planner and produce an explicit lifecycle action or
refusal; the renderer must not silently omit them.

Changing bounded native kubeadm input updates desired role-dependent bootstrap
state. Normal config apply may stage and report that state, but making a running
cluster match it requires the dedicated kubeadm-aware operation. Native input
acceptance does not imply that every kubeadm change has a supported live
transition.

Disk installation fields are consumed by installation and are not runtime
configuration. Kubernetes version changes are handled by the Kubernetes
upgrade workflow rather than ordinary node configuration apply.

## Rejected Flexibility

Katl v1alpha1 rejects aliases and speculative mechanisms including:

```text
nodes[].overrides
nodes[].systemRole
spec.systemRoleDefaults
spec.nodeClasses and nodes[].nodeClass
spec.platformAPIEndpoint
spec.katlosImage
spec.kubernetes.bundle and spec.kubernetes.catalogRef
spec.kubeadmConfigs, named kubeadm maps, and kubeadm config references
bootstrap access or credential fields
identity.hostname
install.wipeTarget
nodeLabels and nodeTaints aliases
templates, loops, ranges, generated node lists, and expression languages
```

Operators may generate valid ClusterConfig YAML with external tooling. Katl
validates only the concrete document it receives.
