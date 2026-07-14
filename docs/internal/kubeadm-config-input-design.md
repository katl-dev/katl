# kubeadm Config Input Design

Status: current design.

This document defines how Katl accepts kubeadm configuration without hiding
kubeadm behind a lossy abstraction and without letting users write arbitrary
runtime `/etc` content.

Katl renders validated kubeadm input into Kubernetes-capable generations. Node
configuration and normal `katlc apply` do not decide whether a node runs
`kubeadm init` or `kubeadm join`. An explicit operator action, such as
`katlctl cluster bootstrap`, may ask `katlc` to prepare kubeadm-ready candidate
generations and then coordinate kubeadm init/join.

## Decision

The ordinary path is Katl-owned complete kubeadm configuration selected from a
node's `systemRole`. The bounded advanced path adds one native input to
`ClusterConfig` without exposing Katl's internal names or references:

```yaml
spec:
  kubernetes:
    version: v1.36.1
    kubeadm:
      configFile: ./kubeadm.yaml
      patchesDir: ./kubeadm-patches
```

The paths are resolved relative to the `ClusterConfig` file and embedded into
the compiled `.katlcfg`. They are not runtime host paths. `patchesDir` is
optional; setting it requires `configFile`.

`kubeadm.yaml` is native kubeadm YAML, not YAML embedded as a
string inside Katl YAML:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: katl
networking:
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
```

Katl renders the selected config into generated confext under:

```text
/etc/katl/kubeadm/control-plane/config.yaml
/etc/katl/kubeadm/control-plane/patches/
/etc/katl/kubeadm/worker/config.yaml
/etc/katl/kubeadm/worker/patches/
```

Katl splits the native documents by role. Control-plane input contains
`InitConfiguration`, `ClusterConfiguration`, and any common kubelet or
kube-proxy document. Worker input contains `JoinConfiguration` and common
documents. Missing role documents, the selected Kubernetes version, the
containerd CRI socket, and safe patch paths are supplied by Katl. Control-plane
join material is derived from the selected init input. Tokens, discovery
hashes, and certificate keys are injected only by the accepted bootstrap
operation and are never read from ClusterConfig.

The rendered path is stable for node-local `katlc` operation wrappers:

```text
kubeadm init --config /etc/katl/kubeadm/control-plane/config.yaml
```

or:

```text
kubeadm join --config /etc/katl/kubeadm/worker/config.yaml
```

Operators and tests do not run kubeadm directly as Katl-managed bootstrap. They
request an explicit bootstrap or join operation, and node-local `katlc` runs
kubeadm under the accepted `OperationRecord`. Manual kubeadm remains out of
band unless a later explicit import or repair operation records and validates
that state. Kubeadm-aware operations remain explicit operator actions and are
never implied by node configuration, install manifest processing, normal
generation activation, or the kubeadm-ready target.

## API Boundary

Katl owns:

```text
complete default control-plane and worker kubeadm input
internal profile naming and role selection
ClusterConfig-relative file resolution and bundle embedding
kubeadm config parsing and compatibility validation
safe render paths under /etc/katl/kubeadm/
generated confext staging and generation selection
```

Kubeadm owns:

```text
bootstrap action semantics
/etc/kubernetes output
control-plane static pod manifests
certificates and kubeconfigs
kube-system kubeadm and kubelet ConfigMaps
kubeadm upgrade and reconfiguration behavior
```

The greenfield stacked-etcd ownership and data policy for kubeadm control-plane
bootstrap is defined in
`docs/internal/stacked-etcd-bootstrap-data-policy.md`.

The operator owns:

```text
optional native kubeadm settings and patches
when to run kubeadm init or join
when to run kubeadm upgrade or other cluster reconfiguration
cluster add-ons, CNI, GitOps, workloads, and ongoing Kubernetes lifecycle after
bootstrap
```

## Validation

Katl parses the referenced kubeadm YAML as multi-document YAML and rejects
inputs that violate the runtime OS boundary.

Allowed document families for the first implementation:

```text
kubeadm.k8s.io/v1beta4 InitConfiguration
kubeadm.k8s.io/v1beta4 JoinConfiguration
kubeadm.k8s.io/v1beta4 ClusterConfiguration
kubelet.config.k8s.io/v1beta1 KubeletConfiguration
kubeproxy.config.k8s.io/* KubeProxyConfiguration, when kube-proxy tests need it
```

Unknown document kinds should fail validation until there is a concrete test or
user need. The kubeadm API version must be compatible with the selected
Kubernetes sysext. When the sysext Kubernetes minor changes, Katl should
validate or require review of the kubeadm config API version instead of silently
reusing an old schema.

Katl should reject kubeadm config or patches that try to own Katl-managed,
immutable, or kubeadm-output paths. Denied host paths include:

```text
/etc/kubernetes
/etc/passwd
/etc/shadow
/etc/group
/etc/gshadow
/etc/sudoers
/etc/sudoers.d
/etc/pam.d
/etc/security
/etc/ssh
/usr
/boot
/efi
/run
/tmp
/var/lib/katl/generations
/var/lib/katl/kubernetes
/var/lib/containerd
/var/lib/kubelet
```

Kubeadm may create output under `/etc/kubernetes` at runtime. Katl-generated
confext must not pre-create or overwrite that output.

For kubeadm patches, Katl copies only regular files from the declared
`patchesDir`, rejects path traversal and symlinks, and renders them under the
selected role directory. Operator input omits `patches.directory`; Katl writes
the role-specific `/etc/katl/kubeadm/<role>/patches` path while compiling the
bundle. Control-plane profiles receive the full patch set; worker profiles
receive only `kubeletconfiguration` patches. Arbitrary patch directories are
not accepted.

For kubeadm `extraVolumes` or other host-path-like fields, Katl should allow
only paths that are already part of the kubeadm contract or explicitly
allowlisted for a tested scenario. Any hostPath under the denied path list must
fail validation.

`kubernetesVersion` may be omitted or set to the selected sysext version. If it
is present and conflicts with the selected Kubernetes sysext payload version,
validation must fail before install or runtime config activation. For first
install, the selected payload version comes from Katl bootstrap intent and the
exact matching Kubernetes payload bundle fetched and verified by `katlc`. Katl
may normalize manifest `1.36.0` to kubeadm's `v1.36.0` form for comparison.
Release compatibility resolution selects the bundle outside native kubeadm
YAML; no catalog references or sentinel values are written into it.

The CRI socket should default to containerd's socket. A different CRI socket is
deferred until Katl intentionally supports another runtime.

## Rendered Files

Generated confext renders kubeadm input files as regular read-only config under
`/etc/katl`:

```text
/etc/katl/kubeadm/<role>/config.yaml
/etc/katl/kubeadm/<role>/patches/<patch-files>
```

Suggested modes:

```text
directories: 0755 root:root
config.yaml: 0644 root:root
patch files: preserve executable bit only if kubeadm requires it; otherwise 0644
```

Rendered kubeadm input is not secret storage by default. Bootstrap tokens,
certificate keys, and other sensitive values should be handled through a later
secret input design or injected at operator action time, not committed into the
default Katl config repository format.

## Desired Input And Live Drift

A runtime configuration update can change the desired kubeadm input:

```text
new Katl YAML/configuration
katlctl validates and compiles the bounded native kubeadm input
katlc renders a new generated confext generation
/etc/katl/kubeadm/<role>/config.yaml changes after activation
```

That does not reconfigure a running cluster by itself.

Katl owns the desired kubeadm/kubelet input it renders under `/etc/katl`.
Kubeadm and kubelet own the live state derived from that input. Normal Katl
generation activation must not reconcile, overwrite, or roll back live
kubeadm/kubelet state.

Live kubeadm cluster state lives in Kubernetes objects and node-local kubeadm
outputs, including:

```text
kube-system/kubeadm-config ConfigMap
kube-system/kubelet-config ConfigMap
/etc/kubernetes/manifests
/etc/kubernetes/admin.conf
/etc/kubernetes/pki
/var/lib/kubelet/config.yaml
/var/lib/kubelet/kubeadm-flags.env
/var/lib/etcd
kubeadm-managed Kubernetes API objects
```

Applying desired kubeadm changes to a running cluster must be an explicit
kubeadm-aware operator action. Later `katlctl` commands may request node-local
`katlc` planning or execution, but `katlc` must report which kubeadm and
Kubernetes objects will change and the action must not be hidden inside normal
confext activation.

The same rule applies during Kubernetes upgrades. Rendering new desired kubeadm
or kubelet input into a candidate generation does not make that input live by
itself. A version transition or reconfiguration becomes live only through an
explicit kubeadm-aware operation that records kubeadm phases, mutation evidence,
target kubeadm access, kubelet activation gates, and recovery status.

When rendered desired input differs from live state, Katl reports drift and
records that an explicit kubeadm-aware action is required. The drift report is
advisory until an operator runs a bootstrap, join, upgrade, reset, or
reconfiguration operation.

The supported v0.1 live control-plane reconfiguration surface, serial rollout,
and rollback boundary are defined by
`docs/internal/adrs/adr-010-kubeadm-control-plane-config-operation.md`.

Rolling back rendered `/etc/katl/kubeadm/<role>/config.yaml` restores desired
input only. It does not restore kubeadm output, kubelet runtime config, etcd
contents, or kubeadm-managed ConfigMaps. Applying or undoing those live changes
requires an explicit kubeadm-aware operation.

## Test Harness Use

The single-node API smoke should use a ClusterConfig with bounded native
kubeadm input and then drive the same explicit bootstrap path used by operators:

```text
run katlctl cluster bootstrap against the installed generation 0 node
verify the bootstrap operation asks katlc to create and activate generation 1
wait for katl-kubeadm-ready.target before kubeadm runs
verify /etc/katl/kubeadm/control-plane/config.yaml exists
run kubeadm init through the node-local katlc bootstrap operation with that
  rendered config
assert kubeadm output under projected /etc/kubernetes
assert kubectl can reach /readyz
```

This keeps the smoke test honest: Katl proves it can deliver validated kubeadm
input and a kubeadm-ready OS; `katlctl` proves the explicit control-client
boundary; kubeadm proves it can bootstrap the control plane.

## Deliberate Boundaries

- ClusterConfig exposes one cluster-wide native file and patch directory, not
  multiple named objects or per-node references.
- Katl writes the concrete selected Kubernetes version; native sentinel values
  are not accepted.
- Bootstrap tokens, discovery hashes, and certificate keys remain operation
  inputs and are never stored in source configuration.
- Accepting desired native input does not expand the supported live
  reconfiguration allowlist in ADR-010.
