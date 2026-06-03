# kubeadm Config Input Design

Status: current design.

This document defines how Katl accepts kubeadm configuration without hiding
kubeadm behind a lossy abstraction and without letting users write arbitrary
runtime `/etc` content.

Katl installs validated kubeadm input. It does not decide whether a node runs
`kubeadm init` or `kubeadm join`, and it does not own cluster bootstrap
lifecycle. An operator or test harness runs kubeadm against the installed input
after the node reaches the kubeadm-ready handoff.

## Decision

Kubeadm configuration is a separate Katl configuration object that points to
native kubeadm YAML files in the user's config repository.

Node configuration only references a named kubeadm config:

```yaml
kubernetes:
  kubeadm:
    configRef: control-plane
```

The referenced object owns file locations in the config repository:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: KubeadmConfig
metadata:
  name: control-plane
spec:
  configFile: kubeadm/control-plane.yaml
  patchesDir: kubeadm/patches/control-plane
```

`kubeadm/control-plane.yaml` is native kubeadm YAML, not YAML embedded as a
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
/etc/katl/kubeadm/<name>/config.yaml
/etc/katl/kubeadm/<name>/patches/
```

For the example above:

```text
/etc/katl/kubeadm/control-plane/config.yaml
/etc/katl/kubeadm/control-plane/patches/
```

The rendered path is stable enough for an operator or test harness to run:

```text
kubeadm init --config /etc/katl/kubeadm/control-plane/config.yaml
```

or:

```text
kubeadm join --config /etc/katl/kubeadm/worker/config.yaml
```

Katl may later provide `katlctl kubeadm init --config control-plane` and
`katlctl kubeadm join --config worker` as thin helpers, but those commands are
operator actions. They are not implied by node configuration, install manifest
processing, or the kubeadm-ready target.

## API Boundary

Katl owns:

```text
KubeadmConfig object naming and references
repository-local file resolution
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

The operator owns:

```text
when to run kubeadm init or join
when to run kubeadm upgrade or other cluster reconfiguration
cluster add-ons, CNI, GitOps, workloads, and ongoing Kubernetes lifecycle
```

## Validation

Katl should parse the referenced kubeadm YAML as multi-document YAML and reject
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

For kubeadm patches, Katl should copy only regular files from the declared
`patchesDir`, reject path traversal and symlinks, and render them under the
selected config directory. If the kubeadm YAML declares an explicit patch
directory, it must either match the rendered `/etc/katl/kubeadm/<name>/patches`
path or fail validation. Katl should not allow arbitrary patch directories.

For kubeadm `extraVolumes` or other host-path-like fields, Katl should allow
only paths that are already part of the kubeadm contract or explicitly
allowlisted for a tested scenario. Any hostPath under the denied path list must
fail validation.

`kubernetesVersion` may be omitted or set to the selected sysext version. If it
is present and conflicts with the selected Kubernetes sysext payload version,
validation must fail before install or runtime config activation.

The CRI socket should default to containerd's socket. A different CRI socket is
deferred until Katl intentionally supports another runtime.

## Rendered Files

Generated confext renders kubeadm input files as regular read-only config under
`/etc/katl`:

```text
/etc/katl/kubeadm/<name>/config.yaml
/etc/katl/kubeadm/<name>/patches/<patch-files>
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

## Runtime Updates

A runtime configuration update can change the desired kubeadm input:

```text
new Katl config repository state
runtime agent validates KubeadmConfig
runtime agent renders a new generated confext generation
/etc/katl/kubeadm/<name>/config.yaml changes after activation
```

That does not reconfigure a running cluster by itself.

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
```

Applying desired kubeadm changes to a running cluster must be an explicit
kubeadm-aware operator action. Later `katlctl` commands may help plan or apply
those operations, but they must report which kubeadm and Kubernetes objects
will change and must not be hidden inside normal confext activation.

## Test Harness Use

The single-node API smoke should use a test fixture `KubeadmConfig` and then
run kubeadm explicitly from the harness:

```text
wait for katl-kubeadm-ready.target
verify /etc/katl/kubeadm/control-plane/config.yaml exists
run kubeadm init --config /etc/katl/kubeadm/control-plane/config.yaml
assert kubeadm output under projected /etc/kubernetes
assert kubectl can reach /readyz
```

This keeps the smoke test honest: Katl proves it can deliver validated kubeadm
input and a kubeadm-ready OS; kubeadm proves it can bootstrap the control
plane.

## Open Questions

1. Should `katlc` allow multiple `KubeadmConfig` objects to be installed on one
   node?

   Initial recommendation: yes, as long as node config selects one default
   `configRef`. Installing multiple named configs is useful for test fixtures
   and for operators who want both init and join material available, but Katl
   should not choose the action.

2. Should Katl rewrite `kubernetesVersion: sysext` in native kubeadm YAML?

   Initial recommendation: avoid sentinel values inside kubeadm YAML. Prefer
   omitting `kubernetesVersion` or writing the concrete selected sysext version
   during `katlc` generation. If a shorthand is needed, make it a Katl wrapper
   field on `KubeadmConfig`, not a fake kubeadm value.

3. How should sensitive kubeadm bootstrap values be supplied?

   Initial recommendation: keep them out of the first API. Operator-run
   bootstrap tools can pass tokens and certificate keys at action time. A later
   secret design can add encrypted or external secret references.
