# Cluster configuration

A `config.katl.dev/v1alpha1` `ClusterConfig`, usually kept as `cluster.yaml`, is
the desired state of a KatlOS cluster: it names the nodes, selects their roles
and disks, chooses the Kubernetes version, and carries supported host and
Kubernetes configuration. Keep the file after installation; installation,
bootstrap, and later changes all read it, and a saved workstation context never
replaces it.

## Field index

Paths under `spec.defaults` can also be set on a concrete node in `spec.nodes`,
where they replace the inherited value as described in each section. The
**Applies** column summarizes how `katlctl cluster apply` activates a change,
and "—" means the field has no default.

| Path | Purpose | Default | Applies |
| --- | --- | --- | --- |
| `spec.managementAuthentication` | `mtls` or `trusted-network` access to the management API; see [management authentication](../operations/management-auth.md#choose-an-access-mode) | Required; `mtls` when `spec.managementIdentity` is set without a mode, and `config init` writes `mtls` | Set the initial mode at installation. Use `katlctl management enable-mtls --config <path>` to [migrate installed nodes from trusted-network to mTLS](../operations/management-auth.md#migrate-to-mtls-management). Moving from mTLS to trusted-network is unsupported in place. |
| `spec.managementIdentity` | mTLS management secrets file, relative to `cluster.yaml` | — | Installation; replace the management authority with `katlctl management identity rotate` |
| `spec.controlPlaneEndpoint` | [Stable Kubernetes API endpoint](#kubernetes-api-endpoint): `host`, `port`, optional `advertisement.vip` | `port: 6443` | Next boot before bootstrap; host, port, and VIP changes are refused after bootstrap |
| `spec.kubernetes.version` | Exact Kubernetes version | Written by `config init` | [Kubernetes upgrade](../operations/upgrade-kubernetes.md) workflow |
| `spec.kubernetes.kubeadm` | [Native kubeadm](#native-kubeadm-configuration) `configFile` and `patchesDir` | Katl's generated kubeadm profile | Live |
| `spec.kubernetes.external` | API `endpoint` of an existing cluster for [external workers](../operations/external-workers.md) | — (Katl-bootstrapped control plane) | Refused; reprovision the node |
| `spec.defaults.access.ssh.authorizedKeys` | SSH authorized keys; each node needs at least one | — | Next boot |
| `spec.defaults.kernel.commandLine` | [Operator kernel arguments](#kernel-arguments) | — | Next boot |
| `spec.defaults.hostConfiguration.fileSets` | [Native host files](#file-sets) | — | Depends on the [file type](#apply-behavior-by-file-type) |
| `spec.defaults.hostConfiguration.enabledUnits` | [Units to enable and start](#enabled-units) | — | Live |
| `spec.defaults.hostConfiguration.maskedUnits` | [Units to mask](#masked-units) | — | Live |
| `spec.defaults.hostConfiguration.preservedNetdevs` | [Netdevs left to plain networkd behavior](#virtual-network-devices) | — | Live |
| `spec.defaults.hostConfiguration.sysfs` | [Typed sysfs values](#sysfs-settings) | — | Next boot |
| `spec.defaults.systemExtensions` | [System extensions](#system-extensions) by OCI repository | — | Extension change or removal: next boot; configuration and units: live |
| `spec.defaults.storage.volumes` | [Data volumes](#data-volumes) | `wipe: false` | Live |
| `spec.defaults.kubernetes.labels` | [Kubernetes node labels](#node-labels-and-taints) | — | Applied when the node joins; later changes print kubectl commands |
| `spec.defaults.kubernetes.taints` | [Kubernetes node taints](#node-labels-and-taints) | — | Applied when the node joins; later changes print kubectl commands |
| `spec.defaults.kubernetes.kubelet.configFile` | [Node-local kubelet overlay](#node-local-kubelet-settings) | — | Live |
| `spec.defaults.generationRetention` | [Automatic generation cleanup](#generation-retention) | `keepLast: 5`, `maxAge: 30d` | Live |
| `spec.nodes[].name` | Stable node name | — | See the [node lifecycle matrix](../operations/configure-nodes.md#node-lifecycle-matrix) |
| `spec.nodes[].controlPlane` | Join the Kubernetes control plane | `false` (worker) | Wipe and reinstall |
| `spec.nodes[].install.systemDisk` | [System disk](#system-disk) for installation; required | — | Installation only |
| `spec.nodes[].management.address` | [Operator-reachable address](#node-addresses) used by `katlctl` | — | Workstation targeting; a control plane's API proxy listener restarts live |
| `spec.nodes[].kubernetes.address` | [Kubernetes node address](#node-addresses) on a multihomed node | — | Refused as an operation-only change |

## Start with a generated configuration

Run `katlctl config validate ./cluster.yaml` while authoring. It checks both the
file as written and each node's effective configuration, and one run reports
every independent error in a stable order by field path, naming the affected
node and, when available, the YAML line number.

This example shows the structure that `katlctl config init` generates. Replace
the example key, addresses, and disk IDs with values for your machines.

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: katl-lab
spec:
  # Stable Kubernetes API VIP or DNS name; without it, the first control plane's address is used and cannot be changed later.
  # controlPlaneEndpoint:
  #   host: api.home.arpa
  #   # port: 6443
  # Set controlPlane: true on nodes that join the Kubernetes control plane.
  # Omission means worker.
  # Nodes use DHCP by default; native systemd-networkd files can be set under defaults or a node.
  managementAuthentication: mtls
  managementIdentity: management-secrets.yaml
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
Kubernetes bundle from the exact required `spec.kubernetes.version`. Keeping
that version in source makes compilation reproducible across `katlctl`
versions; `katlctl config init` always writes a concrete compatible version.
ClusterConfig does not expose KatlOS or Kubernetes
image URLs, credentials, named kubeadm profiles, node classes, or other
compiler mechanisms. An advanced `systemExtensions` entry may select an
operator-owned native software bundle by OCI reference as described below.

## Node addresses

`management.address` is an IP address or DNS name, optionally with the
`katlc` agent port as `host:port`; without a port `katlctl` uses 9443. The
workstation reaches the node's kube-apiserver (6443) and API proxy (7445) at
the same host, and `katlctl context save` records the agent endpoint with its
port.

On a multihomed node, set the optional exact `nodes[].kubernetes.address`.
Katl uses it for kubelet `--node-ip` and the kubeadm local API advertise address
during both init and control-plane join. It is deliberately one literal IP,
not a subnet-selection policy. Omit it for normal single-uplink nodes. The
separate `management.address` remains the operator-reachable address used by `katlctl`
and may be different, including a NAT address that no node interface holds.
When the two differ, nodes reach each other only at `kubernetes.address`: API
proxy backends, joins and etcd membership use it, and nodes never dial the
management address. `katlctl` reaches a control plane's API proxy through the
management address with TLS verified against the control-plane endpoint name,
so when `management.address` is a literal IP the proxy listens there as well
as on `kubernetes.address`. That serves a second interface on a multihomed node
directly and is harmless behind NAT, where translated connections arrive at
`kubernetes.address`. A DNS name in `management.address` gets no listener of its
own, so on a control plane it must reach the node at `kubernetes.address`,
directly or through NAT. For what
each address change means after installation,
see [Change a node's address](../operations/change-cluster-nodes.md#change-a-nodes-address).

## Node labels and taints

Set Kubernetes node labels and taints under `kubernetes` in `spec.defaults` or
on a node:

```yaml
spec:
  defaults:
    kubernetes:
      labels:
        topology.kubernetes.io/region: home
  nodes:
    - name: worker-1
      kubernetes:
        labels:
          topology.kubernetes.io/zone: rack-a
        taints:
          - key: dedicated
            value: storage
            effect: NoSchedule
```

Labels compose by key, with node values replacing default values, and an
explicitly empty `labels: {}` clears inherited labels. A node `taints` list
replaces the default list wholesale, and `taints: []` clears it. Keys and
values must be valid Kubernetes label keys and values, and a taint `effect`
must be `NoSchedule`, `PreferNoSchedule`, or `NoExecute`.

Each node registers with Kubernetes using its labels and taints when it is
initialized or joins, through `katlctl cluster bootstrap`, `katlctl node join`,
or [external-worker](../operations/external-workers.md) enrollment. Bootstrap
and join use the configuration the node already holds, so apply changes made
before then with `katlctl cluster apply`.

Kubernetes applies registration metadata only once, so Katl does not change an
existing Node. After a node has joined, `katlctl cluster apply` records a label
or taint change and prints the `kubectl label` and `kubectl taint` commands that
update the Node.

Labels in the `kubernetes.io` and `k8s.io` namespaces must be ones kubelet may
set on its own Node, such as `topology.kubernetes.io/zone` or keys under
`node.kubernetes.io/`. Set others, such as `node-role.kubernetes.io/worker`,
with `kubectl label node` after the node joins.

## Host configuration

`hostConfiguration` carries native Linux and systemd configuration. Katl
validates ownership and carries the files in the node's generation; the
operator does not build or activate a confext. For task-oriented examples, see
[Apply cluster configuration](../operations/configure-nodes.md#configure-native-linux-facilities).
For management authentication and secret references, see
[management authentication](../operations/management-auth.md).

### File sets

`hostConfiguration.fileSets` is a map of named sets. Each set lists explicit
`files`, or includes a `directory` with a `destination`; use one form per set.

```yaml
spec:
  defaults:
    hostConfiguration:
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
```

`source` is relative to the ClusterConfig directory and is embedded when
`katlctl` builds the self-contained configuration bundle. Use `content` or
`source`, never both. Files default to mode `0644`; `0600` and `0640` are also
accepted.

Defaults and concrete nodes use the same named-set model: a node set with the
same name replaces the entire default set. Duplicate destination paths are
errors, including collisions between inherited and node-specific sets. To
remove an inherited set on one node, set `state: absent` without a directory
declaration:

```yaml
spec:
  nodes:
    - name: worker-1
      hostConfiguration:
        fileSets:
          storage-modules:
            state: absent
```

Katl rejects protected paths, duplicate path ownership, executable or writable
modes, and attempts to notify release-critical units before rendering a
candidate generation. Operator-authored files below `/etc/tmpfiles.d` are
rejected because Katl owns the generated [sysfs](#sysfs-settings) rule.

### Directory sets

A set with `directory` and `destination` includes a collection of native files
without listing every source and destination:

```yaml
spec:
  defaults:
    hostConfiguration:
      fileSets:
        network:
          directory: files/network
          destination: /etc/systemd/network
  nodes:
    - name: cp-1
      hostConfiguration:
        fileSets:
          addresses:
            directory: files/nodes/cp-1/network
            destination: /etc/systemd/network
```

Paths inside each directory are preserved below `destination`, including nested
drop-ins. For example, `files/network/20-bond0.network.d/50-route.conf` becomes
`/etc/systemd/network/20-bond0.network.d/50-route.conf`. Directory paths are
relative to the ClusterConfig directory. Katl recursively includes every regular
file; keep only configuration inputs in the included directory. Symbolic links
and special files are rejected, and existing destination restrictions still
apply. Included files use mode `0644` regardless of workstation permissions; use
explicit `files` entries when a file needs `0600` or `0640`.

Katl expands directories when validating and building the bundle; nodes receive
the same self-contained files as explicit entries, and `katlctl config resolve`
shows the expanded configuration. Directories do not introduce overlay
precedence. Removing a source file removes it from the set's desired
configuration on the next apply. An empty directory is an error: use
`state: absent` to remove the complete set. Directory sets do not change how
network files apply.

### Networkd files

Katl accepts native `.network`, `.netdev`, and `.link` units plus one-level
`*.network.d/*.conf`, `*.netdev.d/*.conf`, and `*.link.d/*.conf` drop-ins below
`/etc/systemd/network`. Users select the unit and drop-in names but cannot move
network configuration outside that Katl-controlled directory.

Without operator network units, Katl generates a DHCP fallback. Any operator
`.network` unit replaces that fallback; auxiliary `.link`, `.netdev`, and
drop-in files can compose with it. The fallback offers DHCP only to Ethernet
links that have no virtual netdev kind and marks those links required and
routable for boot health. Interfaces created later by a CNI, including veth
pairs, Cilium host devices, overlays, and CNI bridges, therefore remain
unmanaged by networkd and cannot make a broken management path appear healthy.
To make a host bridge, bond, VLAN, or tunnel part of the node's own network,
declare its native networkd units; the operator units then replace the fallback
and own that topology explicitly.

Boot health requires every managed link whose effective networkd configuration
enables `RequiredForOnline` to be online and at least one of those links to be
routable. Set `RequiredForOnline=no` in a link's `[Link]` section when a managed
secondary link is intentionally optional at boot.

### Virtual network devices

networkd creates a bond, bridge, VLAN, dummy, or other virtual device from its
`.netdev` unit, but it never deletes a device whose unit was removed and never
applies changed `.netdev` settings to a device that already exists. Katl closes
that gap for the devices its own configuration defines: a device is
Katl-managed when the running generation's `.netdev` units under
`/etc/systemd/network` name it with `[NetDev] Name=`. Devices created by a CNI,
kube-proxy, or any other software, such as `cilium_host`, `lxc*`, `cni0`,
`flannel.1`, or `kube-ipvs0`, are never Katl-managed.

`katlctl cluster apply` handles a Katl-managed device as follows:

| Change | Live apply | Next-boot apply |
| --- | --- | --- |
| `.netdev` added | networkd creates the device when it reloads | The device is created at boot |
| `.netdev` removed | Katl deletes the device with `networkctl delete` after networkd reloads | The device is not created at boot |
| Any setting in the `.netdev` or its drop-ins changed, such as a VLAN `Id`, bond `Mode`, or `Kind` | Staged for the next boot; `--mode live` refuses the change | The device is created with the new settings at boot |

Comments, blank lines, `[NetDev] Description=`, and renaming the unit file do
not count as changes. Katl treats every other difference as one that needs a
new device. Plans name each device that apply deletes and each device that is
recreated with new settings after the reboot.

Katl refuses a live deletion when the device carries one of these
addresses:

- the address `katlctl` used for the apply, or the node's recorded management
  address;
- the node's Kubernetes address or, without `kubernetes.address`, a link that
  carries a default route, which kubelet uses;
- the control-plane endpoint or VIP. Katl resolves an endpoint name without a
  VIP when it checks, and refuses when the name does not resolve.

It also refuses when such a device depends on the target. For example, the
target might be the bond below a management VLAN, a port of a management
bridge, or the link that carries a management tunnel. Katl also refuses when it
cannot tell which link carries a tunnel that holds one of these addresses, such
as a WireGuard interface. Katl repeats the check immediately before each
deletion. Stage such a change with `--mode next-boot` when the node
stays reachable at the same address, for example when a bridge and its port
share a MAC address and DHCP reservation. When the address moves, follow
[Re-address a worker](../operations/change-cluster-nodes.md#re-address-a-worker).
Katl also refuses to delete a device that a retained netdev, such as a VLAN that
stays configured, is stacked on. Deleting a bond or bridge only releases its
ports.

After a live apply, Katl reads kernel link state and fails the apply when it
does not match the configuration:

- a device that it deleted is still present;
- a device that it added is missing or has the wrong kind, VLAN id,
  parent link, or ports;
- an unchanged device is missing or lost its parent or ports.

Katl reports, but does not verify, a `.netdev` whose `[Match]` conditions may
not apply to the host and a stacked device such as a VLAN that no `.network`
unit references, because networkd does not create those. It leaves a removed
device in place when networkd still loads another unit for it, such as one under
`/run/systemd/network`.

A failed apply rolls back to the previous generation. Katl then checks that the
previous devices came back as they were before the apply. It does not
reconfigure links to force them back, because that can drop the addresses of a
management link. When a device did not come back, the rollback is reported as
failed and names the device; reboot the node to rebuild it from the selected
generation. If the agent stops while deleting a device, the generation's apply
status names that device.

Devices follow the booted or live-activated generation. After a rollback or a
reboot into another generation, the next live apply compares against the
generation that is actually running.

List a device in `hostConfiguration.preservedNetdevs` to keep plain networkd
behavior for it: Katl never deletes it, and a changed definition
takes effect when the device is next created, normally at boot. A node list
replaces the defaults. Native `.netdev.d` drop-ins compose as usual.

```yaml
spec:
  defaults:
    hostConfiguration:
      preservedNetdevs:
        - br-vms
```

Removing the `.network` unit of a physical link makes networkd stop managing
that link. On the systemd version that KatlOS ships, networkd then removes the
addresses and routes that it configured on the link, leaving only the kernel's
IPv6 link-local address, so no stale configuration remains. Katl never deletes
physical links.

### Apply behavior by file type

| Files | Activation |
| --- | --- |
| Network files | Live: Katl reloads networkd for `.network` and `.netdev` changes and reloads udev for `.link` changes. A changed `.netdev` waits for the next boot; see [virtual network devices](#virtual-network-devices) and the [network caution](../operations/configure-nodes.md#configure-native-linux-facilities) |
| Sysctl files directly under `/etc/sysctl.d` | Live when every changed key is an existing kernel parameter that Katl can restore and that no later file overrides; see [sysctl files](#sysctl-files). Otherwise next boot |
| Udev rules | Live reload; Katl does not retrigger existing devices |
| Unit definitions and drop-ins under `/etc/systemd/system` | Live; see [enabled units](#enabled-units) |
| Supported systemd component drop-ins, such as `journald.conf.d` and `timesyncd.conf.d` | Live; Katl reloads or restarts an active consumer as appropriate |
| Module load, modprobe, and containerd overlays | Next boot |
| Other permitted files | Next boot, unless Katl can derive their consumer or their set declares an `onChange` notification |

Containerd imports `/etc/containerd/conf.d/*.toml` when it starts.

### Sysctl files

Katl applies a sysctl file change live by writing each changed key with
`sysctl -w` and reading it back. A key changes when a file adds, changes, or
removes its assignment, including the first time any file sets it. Katl
resolves the value from every file `systemd-sysctl` reads, including KatlOS
files in `/usr/lib/sysctl.d`. Files are applied in file-name order, and the
last explicit assignment wins. A later `-key` line without a value excludes
the key, so it stays unset. The live value is therefore the value the next
boot applies.

The whole request waits for the next boot when any changed key:

- comes from a changed file that uses globs, `-` prefixes, or lines Katl cannot
  parse;
- is not present in `/proc/sys` on the running kernel, such as a setting for a
  network link that does not exist yet;
- is set by a managed file but overridden by a different value or an exclusion
  in a later file, such as two sets assigning the same key or a file named to
  sort before a KatlOS file. The next boot keeps the overriding setting too,
  so Katl warns and names the file to rename or remove;
- loses its last assignment, which would leave the kernel default;
- cannot be restored after it is written, such as `kernel.modules_disabled`
  or `net.ipv6.conf.*.disable_ipv6`, or cannot be both read and written at
  runtime;
- has a value with several fields, such as `net.ipv4.ip_local_port_range`.

With `--mode live`, Katl refuses such a request and names the key and the
reason. Removing an assignment that exposes another file's value applies that
value live. If any step of a live apply fails, Katl restores the previous
files and writes back the kernel values it read before the change.

Name files so that they sort after the KatlOS files they override, for
example `80-*.conf` or later.

Application configuration files, template-wide and type-wide drop-ins need
explicit `onChange.systemd` notifications when their consumer cannot be
inferred. A set may declare a bounded notification for an unprotected existing
unit:

```yaml
files:
  - path: /etc/example/config.yaml
    content: |
      enabled: true
onChange:
  systemd:
    - unit: example.service
      action: try-reload-or-restart
```

The accepted actions are `reload`, `try-reload-or-restart`, `try-restart`,
`reload-or-restart`, and `restart`. The `try-` actions leave stopped units
stopped. `restart` and `reload-or-restart` may start a stopped consumer during
live apply; `reload` requires a running unit that supports reload. At boot,
notifications refresh already running consumers; other units read the new
configuration when they start. Use `enabledUnits` for persistent activation.

### Sysfs settings

`hostConfiguration.sysfs` sets typed sysfs values at boot:

```yaml
spec:
  defaults:
    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
          value: N
        - path: /sys/devices/system/cpu/cpu*/cpufreq/energy_performance_preference
          value: balance_performance
```

Sysfs settings are next-boot-only. Katl renders them to an internal tmpfiles
rule, applies each value, and reads it back before boot health succeeds. Each
`path` must be a unique normalized `/sys/...` path. Paths may use `*`, `?`, and
bracket ranges such as `[0-9]` to match multiple files. At boot, every matched
file receives the same value; verification fails if the pattern matches no
files or any matched file has a different value. Each `value` must be a
non-empty single-line value without leading or trailing whitespace. A
node-level `sysfs` list replaces the defaults list; use `sysfs: []` to clear
inherited settings.

### Enabled units

`hostConfiguration.enabledUnits` declares services, timers, sockets, or
instantiated template units to enable. `katlctl cluster apply` enables and
starts newly listed units without a reboot. Katl honors native `[Install]`
metadata and recreates the booted generation's enablement on every boot, so
enablement follows the generation that boots, including after a fallback.
Native `After=`, `Before=`, and dependency directives determine ordering;
listing units in a particular order does not.

At boot, Katl starts enabled units as soon as the booted generation's
configuration and system extensions are merged. That happens after the state
partition mounts and before `sysinit.target`, `network-pre.target`, and
`systemd-networkd.service`, so each unit starts with its configuration in place
and its native ordering holds against everything that starts later, including
networking, SSH, the `katlc` management agent, and Kubernetes. For example,
the shipped `nftables.service` declares `Before=network-pre.target`, so its
ruleset is loaded before networkd configures links and before SSH or the
management API accept connections. Ordering against units that run before the
merge, such as local file system mounts, cannot hold. Units that a
[system extension](#system-extensions) provides become visible in the same
merge and start the same way.

Ordinary enabled units do not become prerequisites for Katl's boot health. A
unit that fails at boot releases the units ordered after it, as in any systemd
boot, but a unit that never finishes starting holds them back. Oneshot services
have no start timeout by default, so set `TimeoutStartSec=` on a oneshot that
orders itself before networking or management services, and on any unit with
`DefaultDependencies=no` that orders itself before `sysinit.target`. Until such
a unit finishes, the node has no SSH or management API, and Katl's
10-minute boot deadline, which arms after `sysinit.target`, cannot recover it.

To start an enabled unit later in boot, add native ordering in a drop-in. For
example, `After=network-online.target` with `Wants=network-online.target`
waits until networkd reports the network online, and
`After=katl-boot-complete.target` waits until boot health has passed. Omit a
unit from the list when it should not start at boot at all.

A node's list replaces the defaults; `enabledUnits: []` clears inherited
enablement. Removing a unit stops it and removes Katl's runtime enablement.
Vendor enablement and activation through dependencies, sockets, or D-Bus still
apply. Use [`maskedUnits`](#masked-units) when a unit must remain stopped. A unit
cannot be both enabled and masked, or managed through both this list and an
extension's units.

Put native unit definitions and drop-ins in file sets under
`/etc/systemd/system`. Changes to a concrete unit or its drop-ins automatically
reload systemd's configuration and restart that unit if it was running or is
declared enabled. Katl also derives the consumer for supported native systemd
component drop-ins, including `journald.conf.d` and `timesyncd.conf.d`, and
reloads or restarts an active consumer as appropriate. Stopped, undeclared
units stay stopped. Removing a concrete unit's definition file stops the unit
before the file disappears; if a vendor or template definition remains, Katl
starts the unit again under it only when it was running. Configuration and unit changes in an extension also apply
live when the extension itself is unchanged; changing the extension still
requires the next boot.

Live apply groups affected service stops and starts using native systemd
dependency transactions. On failure it restores the previous configuration,
runtime enablement, and observed service state. Unit removal stops the service
before its old files disappear. Native unit dependencies can affect additional
units; use them with the same care as with ordinary systemd administration.

### Masked units

`hostConfiguration.maskedUnits` stops unwanted services and prevents systemd
from starting them again, including through dependencies or manual start
requests. Mask-only changes apply live without rebooting; the masks persist
across reboot and upgrades. Katl refuses masks for its protected,
release-critical units. Masking is stronger than disabling: disabling only
removes enablement links and can still allow activation through sockets,
dependencies, or D-Bus.

A node's `maskedUnits` list replaces the defaults. Set `maskedUnits: []` on a
node to clear inherited masks. Removing a mask restores the vendor unit's
normal activation behavior without starting it as part of the apply. Also mask
triggering socket, timer, or path units when you want to suppress those
activation attempts. Use concrete unit names, including an instance name for
template units.

## Kernel arguments

`kernel.commandLine` adds operator-owned arguments to the installed kernel
command line. It may be set under `spec.defaults` or on a concrete node; a
node-level `kernel` block replaces the default list, and `commandLine: []`
clears inherited additions. Each list item is one whitespace-free kernel
argument, such as `intel_iommu=on` or `iommu=pt`.

Applying a changed list creates a next-boot generation; `katlctl cluster apply`
reports that a reboot is required, and the new arguments become active after
that reboot. Reapplying the same list is a no-op.

Katl preserves release-required arguments and owns root selection, immutable
runtime mounting, generation and machine identity, and recovery targets.
Arguments that would override that policy fail validation with the offending
list entry. Image-required and Katl-owned arguments remain internal and are
always carried alongside the configured additions.

## System disk

`install.systemDisk` selects the whole disk that installation erases. Every
criterion you set must match, and together they must match exactly one whole
disk, which must be writable, unmounted, and at least `minSizeMiB` if you set
it. Mounted and read-only disks still count toward that match, so a selector
never resolves differently because another disk happens to be in use. When no
disk or several disks match, installation stops before touching any disk and
lists each candidate with the identities it reports. Katl never falls back to
the first match, a device name, or the only disk present.

```yaml
install:
  systemDisk:
    byID: /dev/disk/by-id/nvme-Samsung_SSD_990_PRO_2TB_S7KHNJ0X123456
```

Use `byID`, `wwn`, or `serial` whenever the installer reports one. These name
the disk hardware itself, so they keep selecting the same disk however it is
attached or enumerated, and `katlctl config init` and `config add-node` record
one of them for every disk that has it. Neither command chooses a disk without
a hardware identity on its own; name such a disk with `--node` or `--disk`.
Other criteria select disks that report no hardware identity, such as many
virtual disks, or narrow a class of disks, and each gives a weaker guarantee:

| Field | Matches | Guarantee |
| --- | --- | --- |
| `byID` | A `/dev/disk/by-id/` alias of the disk | Stable hardware identity; recommended. Aliases udev derives from LVM, MD RAID, or device-mapper metadata, such as `lvm-pv-uuid-`, `md-uuid-`, or `dm-name-`, name disk contents that installation erases, so Katl never records them and refuses them in `--node` and `--disk` |
| `wwn` | The device world-wide name | Stable hardware identity; recommended |
| `serial` | The device serial number | Stable hardware identity; recommended |
| `byPath` | A `/dev/disk/by-path/` alias, such as `/dev/disk/by-path/pci-0000:00:04.0` | Identifies the attachment slot: a replacement disk in the same slot matches, and moving the disk or changing the controller or VM topology stops it matching |
| `gptGUID` | The GPT disk GUID of the current partition table | Identifies disk contents: cloned disks share it, and installation replaces it |
| `name` | A kernel device path such as `/dev/sda` or `/dev/nvme0n1` | Enumeration order can change between boots or when disks are added |
| `model` | The device model, case-sensitively; `*` matches any run of characters and `?` one character | Often shared by every disk of a model |
| `size` | The device size, such as `">= 500GB"`, `"<= 2TiB"`, or `"64GiB"` | Ambiguous whenever several disks fit |
| `type` | `nvme`, `ssd`, or `hdd` | Ambiguous whenever several disks fit |

`size` accepts `>=`, `<=`, `>`, `<`, or `==` followed by a number and a
required unit: `B` for bytes, a decimal unit (`KB`, `MB`, `GB`, `TB`), or a
binary unit (`KiB`, `MiB`, `GiB`, `TiB`). Without a comparison the size must
match exactly. The installer inventory reports each disk's `sizeBytes`.

Combine weak criteria so they identify one disk. For example, a machine whose
only NVMe disk is the system disk can use:

```yaml
install:
  systemDisk:
    type: nvme
    size: ">= 500GB"
```

`katlctl config init`, `config add-node`, and `install apply` print a warning
for a node whose system disk is selected without `byID`, `wwn`, or `serial`.
`spec.defaults.install.systemDisk` may set `model`, `size`, `type`, and
`minSizeMiB` to narrow every node's selection. Each node must still set at
least one criterion of its own, so defaults never choose a disk; its fields
layer over the defaults, and an empty string clears an inherited criterion.
Selectors that identify one disk, namely `byID`, `wwn`, `serial`, `byPath`,
`gptGUID`, and `name`, must be set per node. [Data volume](#data-volumes) disks accept only
`byID`, `wwn`, or `serial`.

### Selection during and after installation

The installer resolves the selector once, against the disks present
immediately before it changes anything, and binds the install to that disk.
Immediately before wiping, and again before partitioning, it checks that the
same disk is still attached, unchanged, still matches the selector, and is
still unmounted, and it addresses the destructive commands to that disk's
kernel attachment sequence rather than its name. If the check fails before
wiping, the installer refuses the request without touching the disk and waits
for another `katlctl install apply`; if it fails after wiping, it stops before
partitioning and the installer must be booted again. The GPT disk GUID is the
one criterion not rechecked, because wiping the disk removes it. Once an
attempt has wiped a disk, every later attempt in the same installer boot stays
bound to that disk and configuration, however it fails, and refuses to change
any other disk; booting the installer again clears the binding.

Once it has partitioned the bound disk, the installer records the partition
GUIDs it finds there and mounts, writes, and boots only those partitions. Each
install receives fresh random partition and disk GUIDs, so another disk that
carries Katl partitions, such as the disk of an earlier install left attached
during a reinstall, is never mounted or written. If the bound disk does not
hold exactly one of each Katl partition, or another disk reuses one of its
partition GUIDs, the install stops before mounting anything and the installer
must be booted again.

After installation Katl does not use `install.systemDisk` again. The installed
system treats the disk holding its running root partition as the system disk
and mounts `/efi` and `/var` only from that disk, on every boot. Host upgrades
write the inactive root slot on the same disk, and live volume changes treat
the disk holding the mounted state partition as the system disk. Other disks
that carry Katl partitions may stay attached. [Data volume](#data-volumes)
disks are bound the same way while Katl provisions them: install and live
apply recheck every selected volume disk before the first write and each one
again immediately before writing it, address the disk, or the selected partition on it, by the disk's kernel attachment
sequence, and record the volume's PARTUUID or filesystem UUID only when it is
found on that disk and shared with no other disk. At boot a volume mounts by
that recorded identity without rechecking its disk, so a copy of a data disk
attached later could be mounted in its place. A disk for which udev creates no
`/dev/disk/by-diskseq` link, such as a `nullb` or AoE device, is refused at
planning, as a system disk or a volume disk to write. The node stops in emergency mode
before mounting `/var` when the system disk does not hold exactly one ESP and
one state partition, when another disk shares its partition GUIDs, as a cloned
disk does, or when the firmware booted the ESP of another disk. The console
and `systemctl status katl-system-disk.service` report the cause and the
recovery. A disk that shares partition GUIDs with the system disk may be the
original, so detach it rather than wiping it. Generations from KatlOS
releases before this guarantee still mount by partition label, so roll back to
one only while no other Katl disk is attached. Editing
`install.systemDisk` on an installed node has no effect until the node is
reinstalled.

On a reinstall the installer resolves the selector again from scratch. A
`gptGUID` selector is then stale: the previous install replaced the partition
table, so the selector matches no disk and the installer stops, listing the
disks and their current identities. A `name`, `byPath`, `model`, `size`, or
`type` selector matches whichever disk fits it at that moment, which need not
be the disk installed before. Check the installer inventory with
`katlctl install status --endpoint ADDRESS`, which lists each disk with the
selector configuration generation would choose, and update
`install.systemDisk`, preferably to a stable identity, before reinstalling.

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

### Wipe authorization

`wipe: true` explicitly authorizes erasing the selected disk or partition,
including existing filesystems and partition metadata, when provisioning the
volume. No additional acknowledgement is required for installation or
configuration apply; `cluster apply` validates every selected node before
mutation. With `wipe: false` (the default), Katl reuses a compatible existing
filesystem and refuses changes that require formatting.

For a disk selector, `wipe: true` lets Katl reinitialize the disk and create
its convention-labeled partition with `systemd-repart`. For a partition
selector, Katl formats only the selected partition and never repartitions its
parent disk. Live apply refuses mounted or otherwise active destructive targets
rather than disrupting workloads. If a live apply fails after it started
repartitioning or formatting a volume, that volume is left unmounted and named
in the error; fix the cause and apply again to finish preparing it.

Choose the target with a unique stable selector. Wipe authorization does not
bypass ambiguous selection, active-device checks, or conflicts with the system
disk and other volumes, including volumes already provisioned on the node,
mounted or not. The storage a volume uses before an apply moves or removes it
is also protected: another volume cannot repartition or format it in that same
apply. Make the move or removal in one apply and reuse the storage in a later
one. Once provisioned, an unchanged generation-bound volume
is reused on repeated applies and reboot; retaining `wipe: true` does not erase
its contents again. Replacing a bound volume additionally requires
[`--rebind-volume`](#replace-a-bound-volume).

### Select a disk or partition

A disk-backed entry with `wipe: true` uses `systemd-repart` to create and
format its convention-labeled partition:

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
filesystem and Katl preserves it.

Selectors are used only while provisioning or explicitly replacing a volume.
Once Katl resolves a selector, the resulting PARTUUID (or filesystem UUID when
no PARTUUID exists) is stored in that node generation and the systemd mount
uses that exact identity. A second disk carrying the same `u-<name>` label
therefore cannot redirect an existing mount. If no prior binding exists, an
ambiguous label blocks planning and must be replaced with a unique `byID`,
`partUUID`, or `filesystemUUID` selector.

### Replace a bound volume

Replacing a generation-bound volume is a separate decision from overwriting
its contents. If a selector changes to a different device, planning fails until
the operator supplies the reported one-shot `--rebind-volume NODE/VOLUME`
authority. Change the selector to the replacement's exact `byID`, `partUUID`,
or `filesystemUUID`, plan the change, inspect the reported target, then
authorize that transition; an ambiguous `byVolumeName` label remains an error
even with rebind authority:

```console
katlctl cluster apply --config ./cluster.yaml --node worker-1 \
  --rebind-volume worker-1/data
```

`--rebind-volume` is one-shot operation authority and is not persisted in
ClusterConfig. Set `wipe: true` if the replacement must be formatted. Removing
a volume does not rebind or erase it: Katl retains an unmounted generation binding for that logical
name, so re-adding the same exact identity preserves its data and selecting a
different identity still requires `--rebind-volume`.

### Volume identity checks

Each generation records the PARTUUID and filesystem UUID of every volume it
mounts. Before mounting volumes at boot or after a live apply, and before
`katlctl node generations select`, Katl checks that exactly one attached
partition carries the recorded PARTUUID, that the partition is not on the
system disk, and that its filesystem UUID still matches. At boot it waits up
to a minute for a recorded partition that has not appeared yet. Volumes
recorded before this check keep their bindings and keep mounting.

When a check fails at boot, Katl mounts none of the node's volumes; during a
live apply, volumes that are already mounted stay mounted. Node status
reports boot health failed, while the node stays reachable and manageable
through `katlctl`. The generation itself is not marked failed and the boot
default does not change, because rolling back cannot change which disks are
attached. `katlctl node status` names the volume, the cause, and what to do
next, and marks the other volumes as held back. The common causes are:

- Another disk carries the same PARTUUID, such as a `dd` clone attached beside
  the original. Do not wipe either disk; detach the copy, or give it new
  partition GUIDs with `sgdisk --randomize-guids`, then reboot.
- The filesystem was recreated after the generation recorded it. If it is now
  the volume's storage, apply the node's configuration with
  `--rebind-volume NODE/VOLUME`. KatlOS 2026.11.1 and earlier could record
  the previous filesystem's UUID when formatting an existing partition in
  place, so such a volume can be refused this way after an upgrade even though
  it holds the expected data; the same rebind records the current filesystem.
- The disk is detached. Attach it and reboot.

A disk replaced by a `dd` clone, with the original detached, carries the same
identity and mounts as before. Selecting an older generation whose volume was
since replaced or reformatted is refused before the boot selection changes;
select a generation recorded after that change instead. Selecting a generation
of KatlOS 2026.11.1 or earlier is also refused while a disk other than the
system disk carries a `KATL_ESP` or `KATL_STATE` partition, because those
runtimes mount `/efi` and `/var` by partition label; detach that disk first.

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
it only after that node's kube-apiserver is ready, creating the stable
`katl-api` dummy interface and adding the VIP only while the local
kube-apiserver is healthy. If the VIP must be routed, configure routing
separately with a native system extension; see
[Route the API VIP with BIRD](#route-the-api-vip-with-bird).

## System extensions

The typed `systemExtensions` list is generic: Katl resolves each extension's
OCI reference to immutable bytes while compiling, carries it in the
self-contained configuration bundle, installs its sysext and optional confext in
the selected generation, and applies native files and unit drop-ins from that
same entry. Configuration source paths are relative to `cluster.yaml`.

The full OCI repository identifies each list item; there is no separate `name`.
Defaults merge into nodes by repository, excluding the tag or digest. A node
item replaces the complete default item for that repository. To remove an
inherited item, repeat its selector with `state: absent`; Katl does not download
the artifact for removal.

### Route the API VIP with BIRD

This operator-owned extension advertises the
[Katl-managed API VIP](#kubernetes-api-endpoint):

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

Katl does not interpret BIRD configuration or infer that an extension is
related to the VIP.

### Release-owned extensions

KatlOS releases build and publish two release-owned extensions, DRBD9
(`drbd9`) and NVIDIA (`nvidia`); the Extensions table in each release's notes
lists the versions that release provides. BIRD is not release-owned: select it
with a `bundle` entry as [shown above](#route-the-api-vip-with-bird).

For a release-owned extension, use `release` instead of `bundle` and provide
the full repository without a tag or digest. The target KatlOS release selects
the release-advertised digest, for either kernel flavour, without an extra
field. Katl does not infer a registry or organization, or select the newest
published artifact. For example:

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

A host upgrade resolves each `release` selection again against the target
release, so the node moves to the extension build for that release. If the
target release does not provide the extension, the upgrade plan fails before
the node changes; choose a release that includes it.

### Apply and inspect extensions

Changing or removing an extension selects a next-boot generation. If the
extension itself is unchanged, configuration, unit drop-ins, and
enablement changes apply live. Katl reloads or restarts running consumers;
unit definition changes restart them so the new definition takes effect.
Native `[Install]` metadata controls enablement, and enabled extension units
start at boot like [enabled units](#enabled-units). `requiredForBootHealth`
also makes the unit a boot health prerequisite; ordinary enabled units are not. Use `katlctl system-extension inspect` or `validate` for an
extension bundle and `katlctl system-extension status --node NODE [NAME]`
after apply. Registry credentials, when needed during workstation compilation,
come from the normal Docker credential store and are never embedded in config
or node state.

### Pin bundle references

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
configuration bundle (`.katlcfg`). It selects control-plane and worker
documents internally and injects bootstrap tokens and certificate material only
when the explicit bootstrap operation runs. Omit the entire `kubeadm` block for
Katl's complete defaults.

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

## Generation retention

`generationRetention` configures automatic cleanup of old
[generations](../concepts/generations.md#retention) in ClusterConfig defaults
or on an individual node:

```yaml
spec:
  defaults:
    generationRetention:
      keepLast: 5
      maxAge: 30d
```

These are also the defaults when the policy is omitted. A node's
`generationRetention` replaces the complete defaults policy. `keepLast` must be
at least one. `maxAge` accepts whole days (`30d`) or a duration (`720h`); `0d`
disables the age floor.

For each installed OS version, the `katlc` management agent keeps
its newest `keepLast` generations **and** all generations younger than
`maxAge`. A generation is eligible for cleanup only when it is outside both
groups. Protected generations survive regardless of age or count, so these
settings are retention floors, not a hard space limit. Records invalidated
by slot replacement are eligible immediately, subject to shared-artifact
protection.

Policy changes can apply online through `katlctl cluster apply`. `katlc`
evaluates the active generation's policy at startup and hourly, skipping
cleanup during active operations or unsettled boot health. Booting an older
generation also restores its retention configuration. Cleanup removes
generation-owned files and boot entries; shared slot kernels and root
partitions are managed by OS upgrades.

## Use the configuration

Use `cluster.yaml` directly for [ISO installation](../installing.md#iso-or-local-handoff),
[bootstrap](../operations/bootstrap-kubernetes.md), and
[configuration changes](../operations/configure-nodes.md). For network boot
or offline provisioning, use the
[configuration bundle procedure](../install-pxe-matchbox.md#compile-one-cluster-configuration-bundle).
