# Apply Cluster Configuration

Use `katlctl cluster apply` for supported configuration changes after
installation. The same `ClusterConfig` remains the source of truth for every
node and for kubeadm-owned Kubernetes configuration.

## Supported Input

The normal source is the same `ClusterConfig` used for installation. The current
renderer carries:

- SSH authorized keys;
- operator-owned kernel command-line additions;
- systemd-networkd files;
- native host configuration file sets; and
- operation-only system role and role-dependent Kubernetes bootstrap state.

Runtime-safe fields apply normally. Katl coordinates affected node generations
and kubeadm phases internally. Disk/install selection and Kubernetes version
changes use the dedicated install and Kubernetes upgrade workflows.

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

Use `hostConfiguration.sets` for file-based Linux and systemd configuration.
Katl validates ownership and carries the files in the node's generation; the
operator does not build or activate a confext.

```yaml
spec:
  defaults:
    hostConfiguration:
      sets:
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

        kernel-tunables:
          files:
            - path: /etc/tmpfiles.d/80-home-lab-kernel-tunables.conf
              content: |
                w /sys/module/printk/parameters/time - - - - N

        containerd:
          files:
            - path: /etc/containerd/conf.d/80-home-lab.toml
              content: |
                version = 4

                [debug]
                  level = "warn"
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
        sets:
          storage-modules:
            state: absent
```

Sysctl files with a reversible concrete-key change can apply live. Udev rules
can reload live, but Katl does not retrigger existing devices. Module load,
modprobe, tmpfiles sysfs writes, and containerd overlays are next-boot-only.
Katl runs each operator tmpfiles file and verifies the requested sysfs value
before boot health succeeds. Containerd imports `/etc/containerd/conf.d/*.toml`
when it starts. Other permitted files are next-boot unless their set declares a
bounded notification for an unprotected existing unit:

```yaml
notify:
  systemd:
    - unit: systemd-journald.service
      action: try-reload-or-restart
```

The accepted actions are `reload`, `try-reload-or-restart`, and `try-restart`.
Katl rejects protected paths, duplicate path ownership, executable or writable
modes, and attempts to notify release-critical units before rendering a
candidate generation. Operator tmpfiles files are intentionally narrower than
the full `tmpfiles.d` language: each non-comment line must be an exact `w` rule
for a normalized `/sys/...` path, with `-` for mode, user, group, and age, and a
concrete value without globs, specifiers, or escapes.

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
