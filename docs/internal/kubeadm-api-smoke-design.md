# Single-Node kubeadm API Smoke Design

Status: working design.

This document defines the first post `katl-kubeadm-ready.target` VM proof. The
goal is to run `kubeadm init` inside an installed Katl runtime, persist
kubeadm-owned output, start the kube-apiserver static pod, and prove the
Kubernetes API server responds to `kubectl`.

This is a test and readiness proof, not a product expansion. Katl prepares
kubeadm-ready nodes. The test harness may apply bounded fixtures to prove that
handoff works, but Katl must not become a Kubernetes distribution or own
production cluster lifecycle, CNI, DNS, ingress, storage, GitOps, or workload
add-ons.

## Proof Boundary

The smoke starts after the installed runtime reaches the kubeadm-ready handoff:

```text
runtime OS booted from installed disk
/var mounted from KATL_STATE
selected sysext and confext active
/etc/kubernetes projected from writable state
containerd available
kubeadm, kubelet, kubectl, and crictl available from the Kubernetes sysext
katl-kubeadm-ready.target reached
```

The smoke ends when:

```text
kubeadm init exits successfully
kubeadm output persists under projected /etc/kubernetes
control-plane static pod manifests exist
the kube-apiserver static pod is running
kubectl can query the API server readyz endpoint with admin.conf
```

The smoke does not require a schedulable node, CoreDNS, kube-proxy, a CNI
plugin, workloads, multi-node discovery, join tokens beyond kubeadm defaults, or
Kubernetes version upgrades.

## Prerequisites

The installed runtime must provide the local host prerequisites that kubeadm
expects:

```text
containerd.service running with the Kubernetes CRI socket available
kubelet.service installed and able to start
systemd cgroups selected for containerd and kubelet
swap disabled or absent
required kernel modules and sysctls configured for Kubernetes preflight
node hostname stable
network address selected for the VM control-plane endpoint
/var/lib/kubelet persistent on KATL_STATE
/var/lib/containerd persistent on KATL_STATE
/var/lib/etcd persistent on KATL_STATE or on a future dedicated KATL_ETCD mount
```

The Kubernetes sysext must provide the artifact set currently checked by
`scripts/check-kubernetes-sysext`:

```text
/usr/bin/kubeadm
/usr/bin/kubectl
/usr/bin/kubelet
/usr/bin/crictl
/usr/bin/conntrack
/usr/bin/ethtool
/usr/bin/socat
/usr/bin/iptables-nft
```

The runtime root must not include Kubernetes binaries directly. The artifact
split remains the one checked by `scripts/check-runtime-root`: containerd and
basic runtime services live in the root, while Kubernetes components live in the
selected sysext and are activated through generation metadata.

## Generated Configuration Boundary

Katl-generated configuration may provide kubeadm input under `/etc/katl` by
rendering a selected `KubeadmConfig` object. The kubeadm config itself remains
native kubeadm YAML in the user's config repository; Katl only owns the
reference, validation, and render path.

Examples:

```text
/etc/katl/kubeadm/control-plane/config.yaml
/etc/katl/kubeadm/control-plane/patches/
```

Generated confext must not provide or overwrite kubeadm output:

```text
/etc/kubernetes
/etc/kubernetes/admin.conf
/etc/kubernetes/pki
/etc/kubernetes/manifests
```

Those paths are kubeadm/kubelet-owned mutable state projected from:

```text
/var/lib/katl/kubernetes/etc-kubernetes -> /etc/kubernetes
```

The test may use a generated kubeadm config fixture, but the fixture is still a
Katl input file. It must not pre-seed certificates, static pod manifests, or
admin kubeconfigs under `/etc/kubernetes`. A passing smoke proves kubeadm can
create those files on the writable projection.

The focused input API decision is recorded in
`docs/internal/kubeadm-config-input-design.md`.

## kubeadm Init Configuration

The first smoke should invoke kubeadm with an installed named config file and
explicit test-only add-on skips:

```text
kubeadm init \
  --config /etc/katl/kubeadm/control-plane/config.yaml \
  --skip-phases=addon/coredns,addon/kube-proxy
```

Skipping CoreDNS and kube-proxy keeps the proof focused on the Katl handoff and
the control-plane static pods. CNI is not required for host-networked static
control-plane pods, and Katl should not ship or select a production CNI just to
make this smoke pass.

The generated test config should set only the fields needed for a deterministic
single-node VM:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: <vm-node-address>
  bindPort: 6443
nodeRegistration:
  name: <stable-hostname>
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: katl-smoke
kubernetesVersion: <sysext-kubernetes-version>
networking:
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
apiServer:
  certSANs:
    - <vm-node-address>
    - 127.0.0.1
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
```

The kubeadm config API version must match the kubeadm version supplied by the
selected Kubernetes sysext. When the sysext Kubernetes minor changes, the test
fixture must be reviewed with that kubeadm version instead of silently reusing an
old config schema.

Preflight errors should fail the smoke by default. A temporary allowlist is only
acceptable for a known local VM constraint and must be named in the test output,
for example a deliberately undersized debug VM. The normal smoke profile should
allocate enough CPU, memory, disk, and kernel support to pass kubeadm preflight
without broad `--ignore-preflight-errors` usage.

## CNI And Add-On Handling

The first API smoke does not install CNI and does not require `NodeReady`.

Expected cluster state after the smoke:

```text
kube-apiserver live and ready
kube-controller-manager running as a static pod
kube-scheduler running as a static pod
local etcd running as a static pod
node object may exist but may be NotReady because no CNI is installed
CoreDNS absent because addon/coredns was skipped
kube-proxy absent because addon/kube-proxy was skipped
```

Later tests may apply a test-only CNI fixture after the API server is reachable.
That fixture belongs to the test harness, not to Katl runtime configuration or
production cluster ownership. Any such fixture must be labeled and stored as a
test asset so users do not mistake it for Katl's supported cluster add-on
surface.

## Incremental Test Path

The implementation should grow in small gates:

1. Boot the installed runtime and wait for `katl-kubeadm-ready.target`.
2. Verify `/etc/kubernetes` is a mount point backed by
   `/var/lib/katl/kubernetes/etc-kubernetes`.
3. Verify `/etc/kubernetes` starts empty except for mount placeholders.
4. Verify `containerd.service` is active and `crictl info` can reach the CRI.
5. Verify `kubeadm`, `kubelet`, `kubectl`, and `crictl` resolve from the
   selected Kubernetes sysext.
6. Render or install `/etc/katl/kubeadm/control-plane/config.yaml` for the test
   VM.
7. Run `kubeadm init` with add-on phases skipped.
8. Assert kubeadm output appeared under `/etc/kubernetes`.
9. Assert the control-plane static pod manifests exist.
10. Wait for the kube-apiserver container to be running.
11. Run `kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw=/readyz`.

Guest-side `kubectl` is the first required assertion because it avoids host
networking and certificate rewriting. Host-side `kubectl` can be added later by
copying `admin.conf` as a restricted artifact, rewriting the server endpoint to
a QEMU host-forwarded address, and ensuring the API server certificate includes
the host-forwarded name or address.

## API Readiness Assertions

The smoke should use bounded retries and fail with collected diagnostics if the
API never becomes ready.

Required assertions:

```text
test -d /etc/kubernetes/pki
test -f /etc/kubernetes/admin.conf
test -f /etc/kubernetes/manifests/kube-apiserver.yaml
test -f /etc/kubernetes/manifests/kube-controller-manager.yaml
test -f /etc/kubernetes/manifests/kube-scheduler.yaml
test -f /etc/kubernetes/manifests/etcd.yaml
crictl ps shows kube-apiserver
kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw=/livez succeeds
kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw=/readyz succeeds
```

Useful secondary assertions:

```text
kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes succeeds
kubectl --kubeconfig /etc/kubernetes/admin.conf get pods -A succeeds
kubectl reports no CoreDNS pods when coredns is skipped
kubectl reports no kube-proxy pods when kube-proxy is skipped
```

The smoke should not fail merely because the node is `NotReady` before CNI is
installed. That is expected for this proof.

## Kubeconfig Handling

`/etc/kubernetes/admin.conf` is sensitive because it contains administrator
client credentials. The first smoke should use it inside the guest and avoid
printing the file into normal logs.

When artifact extraction is needed, the harness should:

```text
store admin.conf in a restricted artifact directory
avoid uploading it to public logs
redact certificate and key material from summaries
record the server endpoint rewrite, if host-side kubectl is used
delete or isolate the artifact with the rest of the VM work directory
```

For host-side `kubectl`, the harness may copy `admin.conf`, rewrite the first
cluster server endpoint to the host-forwarded address, and run:

```text
kubectl --kubeconfig <restricted-admin.conf> get --raw=/readyz
```

That is a later enhancement. The first durable gate is guest-side `kubectl`
against the in-guest API endpoint.

## Logs And Artifacts

On failure, the smoke should collect enough information to diagnose whether the
problem is Katl boot readiness, mount state, kubeadm preflight, containerd,
kubelet, static pods, or API readiness.

Collect:

```text
QEMU serial log
test command transcript
systemctl status katl-kubeadm-ready.target
systemctl status containerd.service
systemctl status kubelet.service
journalctl -b -u containerd.service -u kubelet.service
journalctl -b -t kubeadm -t katl-runtime-boot
findmnt /var /etc/kubernetes /var/lib/etcd
mount output filtered to Katl and Kubernetes paths
generation metadata for the selected generation
kubeadm init stdout and stderr
crictl info
crictl ps -a
crictl logs for failed control-plane containers when available
kubectl readyz and livez output when kubectl can connect
/etc/kubernetes/manifests/*.yaml
```

Do not collect private key material into general logs:

```text
/etc/kubernetes/admin.conf
/etc/kubernetes/pki/*.key
/etc/kubernetes/pki/sa.key
```

If those files are needed for deeper debugging, collect them only into a
restricted artifact area with clear retention rules.

## Failure Gates

The smoke must fail if any of these happen:

```text
katl-kubeadm-ready.target is not reached before timeout
/etc/kubernetes is not a mount point
/etc/kubernetes is not backed by /var/lib/katl/kubernetes/etc-kubernetes
generated confext owns /etc/kubernetes content
containerd is inactive or CRI is unreachable
required Kubernetes sysext binaries are missing
kubeadm init exits non-zero
kubeadm requires broad ignored preflight errors
admin.conf or static pod manifests are missing after kubeadm init
kube-apiserver container never starts
kubectl livez or readyz does not succeed before timeout
required diagnostics cannot be collected after failure
```

Timeouts should be explicit in the test output. A first reasonable budget is:

```text
installed runtime to kubeadm-ready: existing boot smoke budget
kubeadm init command: 5 minutes
API livez after kubeadm init: 3 minutes
API readyz after livez: 5 minutes
diagnostic collection: 1 minute best effort
```

These values are starting points for local QEMU. They should be tuned from
observed VM performance rather than treated as product API.

## Relationship To Boot Health

`katl-boot-complete.target` and `katl-kubeadm-ready.target` remain local OS
health and handoff signals. They should not require `kubeadm init` or Kubernetes
API availability.

The API smoke is a higher-level test gate layered above those signals:

```text
boot health proves the selected Katl generation is locally usable
kubeadm-ready proves the runtime has prerequisites for kubeadm handoff
API smoke proves kubeadm can initialize a single-node control plane
```

Keeping those gates separate preserves rollback semantics. A KatlOS generation
can be considered boot-good without making Katl responsible for full Kubernetes
control-plane lifecycle after handoff.

## Out Of Scope

Deferred to later designs and tests:

```text
automated kubeadm join
multi-node control-plane or worker topology
CNI selection for production use
CoreDNS and kube-proxy lifecycle ownership
node Ready convergence after installing a CNI fixture
external etcd
high availability control planes
certificate renewal and rotation policy
kubeadm upgrade apply/node
Kubernetes minor version upgrade tests
KatlOS root A/B update with an initialized cluster
Kubernetes sysext-only rollback with an initialized cluster
configuration update tests after kubeadm init
workload conformance or e2e Kubernetes suites
```

Those tests will need stronger harness support, durable cluster artifacts, and
clear upgrade and rollback matrices. This smoke only proves the first usable
single-node API handoff.
