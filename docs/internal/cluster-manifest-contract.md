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
katlctl config resolve ./cluster.yaml --node cp-1
katlctl config diff ./cluster-before.yaml ./cluster-after.yaml --node cp-1
```

`config resolve` is the supported effective-value view. It preserves public
ClusterConfig names while showing field provenance, derived storage paths and
labels, owned files, warnings, and apply/lifecycle boundaries. `config diff`
compares that model rather than compiled install-manifest internals.

## Authoring Model

There are two authoring levels:

1. `spec.defaults` for desired behavior shared by every node.
2. Flat entries in `spec.nodes` for concrete node choices.

There are no node classes, system-role default layers, or `overrides` wrapper.
Katl selects its control-plane and worker kubeadm profiles internally from
`controlPlane`. Operators who need a native kubeadm setting that Katl does not
model may supply one bounded cluster-wide kubeadm file and optional patch
directory. They never name or select the generated profiles.

Layering is defined by collection shape:

- Scalars and structured selectors inherit omitted fields. Supplied values
  replace the corresponding field; an empty identity string or `false` clears
  that inherited field.
- Anonymous lists, including kernel command-line entries, SSH keys, sysfs
  settings, and Kubernetes taints, replace wholesale. An explicit `[]` clears
  the inherited list.
- Maps such as Kubernetes labels merge by key. An explicit `{}` clears the
  inherited map.
- Named file sets and system extensions compose by name. A node entry replaces
  the complete default entry with that name, `state: absent` removes it, and an
  explicit empty collection clears all inherited entries.
- Storage volumes deliberately differ from other named collections. They
  compose by name, but a same-name node entry inherits individual omitted
  fields, including selector constraints, filesystem, and `wipe`. This lets a
  default minimum-size or filesystem policy combine with a node-specific
  identity. Switching between `disk` and `partition` replaces the selector
  kind. `state: absent` removes a named volume and `volumes: []` clears the
  collection without erasing its storage.

Storage defaults are policy, never target identity or destructive authority.
They may carry fields such as `minSizeMiB`, `filesystem`, and explicit
`wipe: false`. Katl rejects default `byID`, `wwn`, `serial`, every partition
selector (including `byVolumeName: true`), `partUUID`,
`filesystemUUID`, and `wipe: true`. Those choices must appear on the concrete
node volume that they affect.

Node-specific `wipe: true` expresses the desired formatting result but is not
destructive authority. Installer and online-apply planning discover the target;
blank targets may proceed automatically, while non-blank targets require a
separate operation acknowledgement naming the concrete node and volume. That
acknowledgement is never persisted in ClusterConfig or inherited by later
operations.

Storage removal is a management transition, not a data transition. An empty
node `storage.volumes` collection clears inherited volumes, and an entry with
`state: absent` removes the inherited entry of the same name. Live apply stops
the Katl-managed `/var/mnt/<name>` mount before removing its generated unit.
The underlying partition table, partition, filesystem, and data are preserved;
removal never invokes target discovery, repartitioning, formatting, or wiping.
The mount-point directory may remain empty. Re-adding a selector for the same
filesystem resumes management and mounts the preserved data.

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
    access:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAA... operator@home
    kernel:
      commandLine:
        - intel_iommu=on
        - iommu=pt
    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
      fileSets:
        network:
          files:
            - path: /etc/systemd/network/10-lan.network
              content: |
                [Match]
                Name=enp1s0

                [Network]
                DHCP=yes
        storage-modules:
          files:
            - path: /etc/modules-load.d/80-home-lab-storage.conf
              content: |
                br_netfilter
    install:
      systemDisk:
        minSizeMiB: 32768
    storage:
      volumes:
        - name: data
          selector:
            disk:
              minSizeMiB: 1048576
          filesystem: btrfs
          wipe: false
    kubernetes:
      labels: {}
      taints: []

  nodes:
    - name: cp-1
      # Set to true for nodes that join the Kubernetes control plane.
      # Omission means worker.
      controlPlane: true
      management:
        address: 192.0.2.11
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-KATL_CP_1_ROOT
      storage:
        volumes:
          - name: data
            selector:
              disk:
                byID: /dev/disk/by-id/ata-KATL_CP_1_DATA
      kubernetes:
        # Optional exact address used for this node's Kubernetes identity.
        address: 10.254.1.1
        labels:
          topology.kubernetes.io/zone: rack-a
        taints: []
```

`name` is also the node hostname. A separate hostname alias is deliberately not
part of the contract.

`management.address` is the operator-reachable address used for installation,
initial Kubernetes bootstrap, and an optional initial workstation context.
It need not be a permanent node identity, but it must remain reachable through
those steps. For DHCP nodes, use a reservation or update the workstation
saved context when the address changes.

`nodes[].kubernetes.address` is an optional exact IP address for Kubernetes on
a multihomed node. Katl supplies it to kubelet as `--node-ip`, to kubeadm init
as `InitConfiguration.localAPIEndpoint.advertiseAddress`, and to control-plane
join as `JoinConfiguration.controlPlane.localAPIEndpoint.advertiseAddress`.
It must be a literal unicast IPv4 or IPv6 address; hostnames and CIDR selectors
are rejected. Omit it on ordinary single-uplink nodes to retain kubeadm and
kubelet automatic address selection. The value is node-specific and cannot be
set under `spec.defaults`.

`controlPlane: true` is the only public role choice. Omission or `false` means
worker, and at least one node must set it to true. Katl derives its internal
system role, kubeadm material, and lifecycle ordering from this value.

Nodes use a generated DHCP systemd-networkd profile when neither defaults nor
the node supplies a `.network` unit below `/etc/systemd/network`. Auxiliary
`.link`, `.netdev`, and drop-in files compose with that fallback. Network files
use the same named-file-set layering as other host configuration: a node set replaces
a default set with the same name, while differently named sets compose. This
allows a shared unit and a node-specific drop-in to be expressed independently.

Kubernetes labels compose by key, with node values replacing default values.
An explicitly empty label map clears inherited labels. Taints have no stable
source identity and therefore a supplied node list replaces the default list
wholesale; `taints: []` clears it.

## Install Selection

Each node chooses its own install target. Prefer durable `byID`, `wwn`, or
`serial` selectors; short kernel names such as `/dev/sda` are not valid
destructive selectors.

`defaults.install.systemDisk` may contain only non-identifying constraints such
as minimum size. It cannot select a disk for several nodes. A node's
`install.systemDisk` supplies the durable identity and may override common
constraints. `storage.volumes` describes persistent desired data-disk state
rather than a one-time installation input.

The decision to execute a destructive install belongs to the install operation,
not ClusterConfig. There is no `wipeTarget` authorization field in this API.

## Deliberately Internal Inputs

The following are not ClusterConfig fields:

- KatlOS image URLs, checksums, local paths, or release descriptors;
- Kubernetes OCI bundle references, catalogs, resolver inputs, or digests;
- named kubeadm profiles, maps, render paths, or config references;
- management access methods, tokens, or credential references;
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
`spec.kubernetes.version`. Kubelet `node-ip` and the init/join local API
advertise address are also Katl-owned and must be expressed through
`nodes[].kubernetes.address`.

## Runtime Planning

When a ClusterConfig is rendered for an installed node, Katl includes every
supported desired field in the node change request. Runtime-live fields such as
SSH keys and host configuration files use their declared apply behavior.
Networkd paths are staged for next boot. Operation-only fields such
as control-plane participation and role-dependent Kubernetes bootstrap state
remain visible to the planner and produce an explicit lifecycle action or
refusal; the renderer must not silently omit them.

Changing bounded native kubeadm input updates desired role-dependent bootstrap
state. Normal config apply may stage and report that state, but making a running
cluster match it requires the dedicated kubeadm-aware operation. Native input
acceptance does not imply that every kubeadm change has a supported live
transition.

Changing `nodes[].kubernetes.address` on an already installed node is a
node-identity migration. Normal config apply keeps the requested field visible
but refuses the change with a kubeadm-aware operation requirement; set the
address in the ClusterConfig used for installation.

`install.systemDisk` is consumed by installation. `storage.volumes` remains
persistent node intent and is reconciled by supported configuration workflows.
Kubernetes version changes are handled by the Kubernetes upgrade workflow
rather than ordinary node configuration apply.

The node list is not destructive reconciliation authority. Omitting a node
stops targeting it and preserves its host, Kubernetes membership, etcd
membership, partitions, filesystems, and data. Removal, enrolled rename,
replacement, and role change use the explicit node wipe/reinstall workflow so
the affected node is named and Kubernetes or etcd membership is handled before
any node-local reset. Cluster apply refuses an enrolled name or role mismatch;
one fresh same-name replacement may join at a time after the explicit wipe and
reinstall workflow. An unenrolled hostname change remains an ordinary
next-boot configuration change.

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
management access or credential fields
access.hostname
install.wipeTarget
nodeLabels and nodeTaints aliases
templates, loops, ranges, generated node lists, and expression languages
```

Operators may generate valid ClusterConfig YAML with external tooling. Katl
validates only the concrete document it receives.
