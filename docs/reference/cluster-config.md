# Cluster configuration

Use one `config.katl.dev/v1alpha1` `ClusterConfig` to name the nodes, select their
roles and disks, and choose a Kubernetes version. Keep the file after installation
so you can bootstrap Kubernetes and apply later changes from the same source.

## Start with a generated configuration

Run `katlctl config validate ./cluster.yaml` while authoring. One run reports
all independent source-policy and effective per-node errors in stable public
field-path order, names the affected node, and includes YAML line numbers when
the source location is available.

This example shows the structure that `katlctl config init` generates. Replace
the example key, addresses, and disk IDs with values for your machines.

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: katl-lab
spec:
  # Stable Kubernetes API endpoint for multi-control-plane clusters.
  # controlPlaneEndpoint:
  #   host: api.home.arpa
  #   # port: 6443
  # Set controlPlane: true on nodes that join the Kubernetes control plane.
  # Omission means worker.
  # Nodes use DHCP by default; native systemd-networkd files can be set under defaults or a node.
  kubernetes:
    version: v1.36.1
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
    # Optional operator-owned additions to the installed kernel command line.
    # kernel:
    #   commandLine:
    #     - intel_iommu=on
    #     - iommu=pt
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-KATL_CP_1_ROOT
      management:
        address: 192.0.2.11
      # Optional on multihomed nodes; this is Kubernetes identity, not the
      # operator-reachable management address.
      kubernetes:
        address: 10.254.1.1
    - name: worker-1
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-KATL_WORKER_1_ROOT
      management:
        address: 192.0.2.21
      kubernetes:
        address: 10.254.1.3
```

When no native kubeadm file is supplied, Katl's generated kubeadm profile keeps
kube-proxy enabled, allows workloads on control-plane nodes, and assigns Node
Pod CIDRs from `10.244.0.0/16` and Service IPs from `10.96.0.0/12`. Supply
`spec.kubernetes.kubeadm.configFile` to own the native `ClusterConfiguration`,
including different or intentionally absent Pod and Service subnets.

The release ISO supplies the KatlOS image, and Katl resolves the selected
Kubernetes payload from the exact required `spec.kubernetes.version`. Keeping
that version in source makes compilation reproducible across `katlctl`
versions; `katlctl config init` always writes a concrete compatible version.
ClusterConfig does not expose KatlOS or Kubernetes
image URLs, credentials, named kubeadm profiles, node classes, or other
compiler mechanisms. An advanced `systemExtensions` entry may select an
operator-owned native software bundle by OCI reference as described below.

## Node addresses

On a multihomed node, set the optional exact `nodes[].kubernetes.address`.
Katl uses it for kubelet `--node-ip` and the kubeadm local API advertise address
during both init and control-plane join. It is deliberately one literal IP,
not a subnet-selection policy. Omit it for normal single-uplink nodes. The
separate `management.address` remains the operator-reachable address used by `katlctl`
and may be different.

## Host configuration files

Use `hostConfiguration.fileSets` for supported native Linux files, including
systemd-networkd configuration. See
[the supported file paths and apply modes](../operations/configure-nodes.md#configure-native-linux-facilities)
to choose the path for a change. For management authentication and secret
references, see [management access](../operations/access.md#security-boundary).

## Kernel arguments

`kernel.commandLine` adds operator-owned arguments to the installed kernel
command line. It may be set under `spec.defaults` or on a concrete node; a
node-level `kernel` block replaces the default list, and `commandLine: []`
clears inherited additions. Each list item is one whitespace-free kernel
argument, such as `intel_iommu=on` or `iommu=pt`. Katl rejects arguments that
would override its root, immutable-runtime, generation identity, or recovery
policy. Image-required and Katl-owned arguments remain internal and are always
carried alongside the configured additions.

## Data volumes

Persistent data disks are configured under a node's `storage.volumes` during
installation or through normal node configuration. The supported and
journey-verified filesystems are `ext4`, `xfs`, and `btrfs`. Each entry selects
exactly one whole disk or one existing partition, and Katl derives both the GPT
label `u-<name>` and mount path `/var/mnt/<name>`; operators cannot choose
another location.

Defaults may share non-identifying storage policy such as `minSizeMiB`, the
filesystem, and `wipe: false`. Stable disk or partition identity and
`wipe: true` must be set on the concrete node volume; Katl rejects them under
`spec.defaults` so one inherited value cannot select or erase storage across
the cluster.

Removing a volume is non-destructive. Set an inherited entry to
`state: absent`, omit a node-specific entry from the desired collection, or
use `storage.volumes: []` to clear all inherited volumes for that node. Online
apply first unmounts `/var/mnt/<name>`, then removes Katl's generated mount
unit and stops managing the target. It does not format, repartition, run
`wipefs`, or erase the partition, filesystem, mount-point directory, or data.
Re-adding a matching selector mounts the preserved filesystem again.

### Select a disk or partition

A disk-backed entry with `wipe: true` authorizes reinitialization of the
selected disk, including any existing contents. Katl uses `systemd-repart`
to create and format its convention-labeled partition:

```yaml
install:
  systemDisk:
    byID: /dev/disk/by-id/ata-KATL_WORKER_1_ROOT
storage:
  volumes:
    - name: data
      selector:
        disk:
          byID: /dev/disk/by-id/ata-KATL_WORKER_1_DATA
      filesystem: btrfs
      wipe: true
```

To preserve an existing Talos UserVolumeConfig partition, select
`partition.byVolumeName: true`. Katl derives `u-<name>` and requires it to
identify exactly one unmounted partition. A stable partition by-id path,
PARTUUID, or filesystem UUID may be supplied inside `partition` instead:

```yaml
storage:
  volumes:
    - name: local-hostpath
      selector:
        partition:
          byVolumeName: true
      filesystem: xfs
```

With `wipe: false`, the selected target must already contain the requested
filesystem and Katl preserves it. With `wipe: true`, Katl formats the selected
target; a partition selector never repartitions its parent disk. Live apply
refuses mounted or otherwise active destructive targets rather than disrupting
workloads.

Selectors are used only while provisioning or explicitly replacing a volume.
Once Katl resolves a selector, the resulting PARTUUID (or filesystem UUID when
no PARTUUID exists) is stored in that node generation and the systemd mount
uses that exact identity. A second disk carrying the same `u-<name>` label
therefore cannot redirect an existing mount. If no prior binding exists, an
ambiguous label blocks planning and must be replaced with a unique `byID`,
`partUUID`, or `filesystemUUID` selector.

`wipe: true` explicitly authorizes erasing the selected disk or partition,
including existing filesystems and partition metadata, when provisioning the
volume. No additional acknowledgement is required for installation or
configuration apply. With `wipe: false` (the default), Katl reuses a compatible
existing filesystem and refuses changes that require formatting.

Choose the target with a unique stable selector. Wipe intent does not bypass
ambiguous selection, active-device checks, or conflicts with the system disk
and other volumes. Once provisioned, an unchanged generation-bound volume is
reused on repeated applies and reboot; retaining `wipe: true` does not erase
its contents again.

### Replace a bound volume

Replacing a generation-bound volume is a separate decision from overwriting
its contents. Change the selector to the replacement's exact stable identity,
plan the change, inspect the reported target, then authorize that transition:

```console
katlctl cluster apply --config ./cluster.yaml --node worker-1 \
  --rebind-volume worker-1/data
```

`--rebind-volume` is one-shot operation authority and is not persisted in
ClusterConfig. Set `wipe: true` to authorize formatting the replacement,
including existing contents. Removing a volume does not rebind
or erase it: Katl retains an unmounted generation binding for that logical
name, so re-adding the same exact identity preserves its data and selecting a
different identity still requires `--rebind-volume`.

### Formatting behavior

Katl uses `systemd-repart --discard=no` to partition and format the system disk
and whole-disk data volumes. This creates fresh filesystems without waiting for
a full-device discard. The state filesystem retains fs-verity support; immutable
root images are written to their slots separately.

For an explicitly selected existing partition, Katl formats only that partition
in place, also with discard disabled. It preserves the parent partition table
and sibling partitions. A filesystem format is not a secure data sanitization
operation; unused space can be trimmed separately after installation.

## Kubernetes API endpoint

For a Katl-managed endpoint, add the VIP. Katl owns that address only on
healthy control-plane nodes; external endpoints and workers do not run the VIP
controller:

```yaml
spec:
  controlPlaneEndpoint:
    host: api.home.arpa
    advertisement:
      vip: 10.40.0.10
```

The API port defaults to `6443`. Katl starts with the VIP released and acquires
it only after that node's kube-apiserver is ready. If the VIP must be routed,
configure routing separately with native system extensions. For example:

```yaml
spec:
  controlPlaneEndpoint:
    host: api.home.arpa
    advertisement:
      vip: 10.40.0.10
  defaults:
    systemExtensions:
      - bundle: registry.example/katl/extensions/bird:v3.3.1-katl.4
        configuration:
          files:
            - path: /etc/bird.conf
              source: ./bird/bird.conf
        units:
          - name: bird.service
            enable: true
            requiredForBootHealth: true
            dropIns:
              - name: 10-site.conf
                content: |
                  [Service]
                  ExecStartPre=/usr/bin/bird -p -c /etc/bird.conf
                  AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
                  CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
```

Katl creates the stable `katl-api` dummy interface and adds the VIP only while
the local kube-apiserver is healthy. It does not interpret BIRD configuration
or infer that an extension is related to the VIP. The typed
`systemExtensions` list is generic: Katl resolves the OCI bundle to immutable
bytes while compiling, carries it in the self-contained config bundle, installs
its sysext and optional confext in the selected generation, and applies native
files and unit drop-ins from that same entry. Configuration source paths are
relative to `cluster.yaml`.

## System extensions

The full OCI repository identifies each list item; there is no separate `name`.
Defaults merge into nodes by repository, excluding the tag or digest. A node
item replaces the complete default item for that repository. To remove an
inherited item, repeat its selector with `state: absent`; Katl does not download
the artifact for removal.

For a release-owned extension, use `release` instead of `bundle` and provide
the full repository without a tag or digest. The target KatlOS release selects
the release-advertised digest. Katl does not infer a registry or organization,
or select the newest published artifact. For example:

```yaml
systemExtensions:
  - release: ghcr.io/katl-dev/katl/extensions/drbd9
```

To select the beta NVIDIA driver on an x86-64 node, use its release repository:

```yaml
systemExtensions:
  - release: ghcr.io/katl-dev/katl/extensions/nvidia
```

The NVIDIA extension contains open kernel modules, matching host compute
libraries, and GPU System Processor firmware. It is intended for Turing and
newer GPUs; the initial hardware validation target is an NVIDIA T4. It does not
include the NVIDIA Container Toolkit or a Kubernetes device plugin. The node
must permit unsigned kernel modules; Katl does not disable signature
enforcement. This beta artifact has not yet been validated with a physical GPU,
a Kubernetes GPU workload, or a host upgrade and rollback. Verify `nvidia-smi`
on the node after reboot before placing GPU workloads on it.

Payload changes and removal select a next-boot generation. If the
payload is unchanged, configuration, unit drop-ins, and enablement changes
apply live. Katl reloads or restarts running consumers;
unit definition changes restart them so the new definition takes effect.
Native `[Install]` metadata controls enablement. `requiredForBootHealth` makes
the unit an explicit boot prerequisite; ordinary enabled units may start later.
Use `katlctl system-extension inspect` or `validate` for
an OCI bundle and `katlctl system-extension status --node NODE [NAME]` after
apply. Registry credentials, when needed during workstation compilation, come
from the normal Docker credential store and are never embedded in config or
node state.

A tag-only `bundle` reference keeps the routine home-lab path convenient, but
the tag may select different bytes in a later compilation. Katl resolves it
once, verifies the content, and prints a warning for every affected node with
the exact digest-pinned replacement:

```text
registry.example/katl/extensions/bird:v3.3.1-katl.4@sha256:<OCI-manifest-digest>
```

Commit that value in `cluster.yaml` when repeatable selection matters. A
digest-pinned reference compiles without the mutable-reference warning.

## Native kubeadm configuration

Reference native kubeadm files when you need to choose cluster-wide settings
such as Pod and Service address ranges or kube-proxy behavior.

```yaml
spec:
  kubernetes:
    version: v1.36.1
    kubeadm:
      configFile: ./kubeadm.yaml
      patchesDir: ./kubeadm-patches
```

Both paths are relative to `cluster.yaml`. Katl validates the native document
kinds and API versions, rejects unsafe host paths and filesystem escapes,
supplies its required role defaults, and embeds the resulting files into the
compiled `.katlcfg`. It selects control-plane and worker documents internally
and injects bootstrap tokens and certificate material only when the explicit
bootstrap operation runs. Omit the entire `kubeadm` block for Katl's complete
defaults.

### Memory reserved for the host

Katl reserves `1Gi` of node memory for the operating system by default through
`KubeletConfiguration.systemReserved.memory`. Kubelet subtracts this reservation
from node allocatable memory, so the scheduler leaves room for node services.
To change the shared cluster kubelet configuration, add a native
`KubeletConfiguration` document to `spec.kubernetes.kubeadm.configFile`:

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
systemReserved:
  memory: 3Gi
```

Katl retains the `1Gi` default when that native document omits
`systemReserved.memory`. To keep the policy in Katl's node-local input, set a
reference in `spec.defaults.kubernetes.kubelet.configFile` or on one node as
shown below. A resolved node-local value overrides the cluster-wide value. This
reservation changes scheduling capacity; it does not impose a hard memory limit
on pods or system services.

### Cluster networking and kube-proxy

This bounded native file is the stable interface for cluster-wide Kubernetes
networking choices. Set Pod and Service CIDRs in
`ClusterConfiguration.networking`, and set `ClusterConfiguration.proxy.disabled` when
the chosen CNI replaces kube-proxy:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
proxy:
  disabled: true
networking:
  podSubnet: 172.20.0.0/16
  serviceSubnet: 172.21.0.0/16
```

Include a native `KubeProxyConfiguration` document when kube-proxy remains in
use but needs non-default policy. Katl does not duplicate these upstream
cluster-wide fields under a second typed ClusterConfig networking surface.
Katl honors the cluster's disabled setting during bootstrap and control-plane
joins, and leaves service routing to your chosen CNI. Disabling kube-proxy
does not install or configure a replacement CNI.

### Node-local kubelet settings

For a kubelet policy that applies to every node, reference one native
`kubelet.config.k8s.io/v1beta1` `KubeletConfiguration` from the node defaults:

```yaml
spec:
  defaults:
    kubernetes:
      kubelet:
        configFile: ./kubelet.yaml
  nodes:
    - name: worker-1
      kubernetes:
        kubelet:
          configFile: ./worker-1-kubelet.yaml
```

Nodes inherit the default reference. A node-level reference, such as the
`worker-1` value above, replaces it for that node. Use `katlctl config resolve`
to see the effective reference and whether it came from defaults or the node.

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
systemReserved:
  cpu: 500m
  memory: 3Gi
topologyManagerPolicy: restricted
```

The file is a bounded native overlay, not a Katl-owned host path. Katl applies
each node's resolved reference as a node-local kubeadm kubelet patch during
bootstrap and online changes, then verifies the local kubelet configuration and
node health. It never uploads the overlay to the cluster-wide `kubelet-config`
ConfigMap. Changing a default changes the desired input for every node that
inherits it. Katl rejects multiple documents, other API kinds or versions, and
fields that replace Katl-owned runtime paths.

Kubeadm applies the selected ordinary native patches first, then the explicit
fields in this overlay. Omitted overlay fields retain their value from the
patched shared configuration.

## Use the configuration

Use `cluster.yaml` directly for [ISO installation](../installing.md#iso-or-local-handoff),
[bootstrap](../operations/bootstrap-kubernetes.md), and
[configuration changes](../operations/configure-nodes.md). For network boot
or offline provisioning, use the
[compiled bundle procedure](../install-pxe-matchbox.md#compile-one-cluster-configuration-bundle).
