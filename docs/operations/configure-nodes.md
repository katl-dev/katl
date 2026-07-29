# Apply Cluster Configuration

Use `katlctl cluster apply` for supported configuration changes after
installation. The same `ClusterConfig` remains the source of truth for every
node and for kubeadm-owned Kubernetes configuration.

## Supported Input

The normal source is the same `ClusterConfig` used for installation. The current
renderer carries:

- SSH authorized keys;
- operator-owned kernel command-line additions;
- native host configuration file sets, including systemd-networkd files and
  drop-ins;
- desired data disks under `storage.disks`;
- operation-only system role and role-dependent Kubernetes bootstrap state.

Runtime-safe fields apply normally. Katl coordinates affected node generations
and kubeadm phases internally. System-disk installation selection and
Kubernetes version changes use the dedicated install and Kubernetes upgrade
workflows; data disks remain desired node storage.

If `spec.kubernetes.kubeadm` changes, cluster apply validates every node before
mutation and then reconciles every affected Kubernetes component online. A
Kubernetes configuration change never falls back to next-boot application or
requires a host reboot.

## Configure Kernel Arguments

Set `kernel.commandLine` under defaults or a concrete node:

```yaml
spec:
  defaults:
    kernel:
      commandLine:
        - intel_iommu=on
        - iommu=pt
```

Each entry is one argument without whitespace. Applying a changed list creates
a next-boot generation; `katlctl cluster apply` reports that a reboot is
required, and the new arguments become active after that reboot. Reapplying the
same list is a no-op. To remove all operator additions for one node, set:

```yaml
spec:
  nodes:
    - name: worker-1
      kernel:
        commandLine: []
```

Katl preserves release-required arguments and owns root selection, immutable
runtime mounting, generation and machine identity, and recovery targets.
Attempts to configure those arguments fail validation with the offending list
entry.

## Configure Native Linux Facilities

Use `hostConfiguration.fileSets` for file-based Linux and systemd configuration.
Katl validates ownership and carries the files in the node's generation; the
operator does not build or activate a confext.

```yaml
spec:
  defaults:
    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
      fileSets:
        forwarding:
          files:
            - path: /etc/sysctl.d/80-home-lab-forwarding.conf
              content: |
                net.ipv4.ip_forward = 1

        ups-device:
          files:
            - path: /etc/udev/rules.d/80-home-lab-ups.rules
              source: files/80-home-lab-ups.rules

        storage-modules:
          files:
            - path: /etc/modules-load.d/80-home-lab-storage.conf
              content: |
                br_netfilter
                vfio_pci

        containerd:
          files:
            - path: /etc/containerd/conf.d/80-home-lab.toml
              content: |
                version = 4

                [debug]
                  level = "warn"

        network-common:
          files:
            - path: /etc/systemd/network/20-bond0.network
              content: |
                [Match]
                Name=bond0

                [Network]
                DNS=172.53.53.53
                LinkLocalAddressing=no
                VLAN=bond0.20
                VLAN=bond0.40
```

`source` is relative to the ClusterConfig directory and is embedded when
`katlctl` builds the self-contained configuration bundle. Use `content` or
`source`, never both. Files default to mode `0644`; `0600` and `0640` are also
accepted.

Defaults and concrete nodes use the same named-set model. A node set replaces a
default set with the same name. To remove an inherited set on one node:

```yaml
spec:
  nodes:
    - name: worker-1
      hostConfiguration:
        fileSets:
          storage-modules:
            state: absent
```

Use a separate node set for host-specific networkd drop-ins so it composes with
the shared set:

```yaml
spec:
  nodes:
    - name: cp-1
      hostConfiguration:
        fileSets:
          network-address:
            files:
              - path: /etc/systemd/network/20-bond0.network.d/50-address.conf
                content: |
                  [Network]
                  Address=10.254.1.1/31

                  [Route]
                  Gateway=10.254.1.0
```

Katl accepts native `.network`, `.netdev`, and `.link` units plus one-level
`*.network.d/*.conf`, `*.netdev.d/*.conf`, and `*.link.d/*.conf` drop-ins below
`/etc/systemd/network`. Users select the unit and drop-in names but cannot move
network configuration outside that Katl-controlled directory. Any operator
`.network` unit replaces Katl's generated DHCP fallback; auxiliary `.link`,
`.netdev`, and drop-in files can compose with the fallback.

Sysctl files with a reversible concrete-key change can apply live. Udev rules
can reload live, but Katl does not retrigger existing devices. Module load,
modprobe, typed sysfs settings, containerd overlays, and networkd files are
next-boot-only.
Katl renders `hostConfiguration.sysfs` to an internal tmpfiles rule, applies
each value, and reads it back before boot health succeeds. Containerd imports
`/etc/containerd/conf.d/*.toml` when it starts. Other permitted files are
next-boot unless their set declares a bounded notification for an unprotected
existing unit:

```yaml
onChange:
  systemd:
    - unit: systemd-journald.service
      action: try-reload-or-restart
```

The accepted actions are `reload`, `try-reload-or-restart`, and `try-restart`.
Katl rejects protected paths, duplicate path ownership, executable or writable
modes, and attempts to notify release-critical units before rendering a
candidate generation. Each sysfs `path` must be a unique normalized `/sys/...`
path, and each `value` must be a non-empty single-line value without leading or
trailing whitespace. A node-level `sysfs` list replaces the defaults list; use
`sysfs: []` to clear inherited settings. Operator-authored files below
`/etc/tmpfiles.d` are rejected because Katl owns the generated sysfs rule.

## Apply The Cluster

Apply the source configuration directly:

```sh
katlctl cluster apply --config ./cluster.yaml
```

Katl compiles every selected node configuration, validates the whole cluster,
and starts no mutation if any node rejects the plan. It then applies node
configuration and all affected Kubernetes component phases in a safe serial
order, returning only after the cluster is healthy.

If the source has already been compiled, pass the bundle through the same flag:

```sh
katlctl cluster apply --config ./katl-lab.katlcfg
```

Katl derives and verifies the bundle's integrity metadata from the file.

`katlctl` derives per-node generations, component phases, rollout ordering, and
operation identities internally. A successful return means the complete
supported configuration is active; partial or unsupported plans fail with the
node, field, and recovery action.

## Check Status

Use `katlctl node status cp-1 --config ./cluster.yaml` for the current healthy
generation. Use `katlctl operations list --config ./cluster.yaml --node cp-1`
when diagnosing an accepted or recently completed configuration operation.

On-node evidence remains available under:

```text
/var/lib/katl/generations/<generation>/
/var/lib/katl/operations/<operation-id>/
/var/lib/katl/boot/selection.json
```

If status reports rollback failure or `failed-needs-repair`, stop and follow the
reported recovery action before submitting another cluster apply.
