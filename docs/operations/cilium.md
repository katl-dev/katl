# Run Cilium on KatlOS

KatlOS prepares kubeadm nodes but does not install or manage a CNI. This
procedure records the KatlOS-specific Cilium setting needed when Cilium is
installed through the operator's own Helm, Cilium CLI, or GitOps workflow.

The procedure has been exercised with Cilium 1.19.6 and Cilium CLI 0.19.6.
Review Cilium's
[kubeadm installation guide](https://docs.cilium.io/en/stable/installation/k8s-install-kubeadm/)
and release notes for the version being installed rather than treating this
page as a complete Cilium configuration.

## Preserve The Immutable Host Boundary

KatlOS owns `/etc` as part of the selected, versioned host generation. A
privileged Kubernetes workload may change live kernel state through
`/proc/sys`, but it must not persist configuration by writing directly beneath
`/etc`.

KatlOS already supplies the Kubernetes node forwarding and reverse-path-filter
defaults needed by the Cilium datapath. Cilium nevertheless enables an
`apply-sysctl-overwrites` init container by default. That container attempts to
create `/etc/sysctl.d/99-zzz-override_cilium.conf`, which is redundant on
KatlOS and cannot write to the immutable `/etc`.

Disable that init container with Cilium's supported `sysctlfix.enabled` Helm
value. Do not make `/etc` or `/etc/sysctl.d` writable to accommodate it.

## Install Cilium

After [bootstrapping Kubernetes](bootstrap-kubernetes.md), install the selected
Cilium release with `sysctlfix.enabled=false`. With the Cilium CLI:

```sh
cilium install \
  --kubeconfig ./kubeconfig \
  --version 1.19.6 \
  --set sysctlfix.enabled=false
```

With Helm:

```sh
helm install cilium oci://quay.io/cilium/charts/cilium \
  --kubeconfig ./kubeconfig \
  --version 1.19.6 \
  --namespace kube-system \
  --set sysctlfix.enabled=false
```

Carry the same value in the retained Helm values or GitOps source used for
later Cilium upgrades. Other Cilium settings remain cluster-specific and
operator-owned.

The setting is part of Cilium's public
[Helm values](https://docs.cilium.io/en/stable/helm-reference/#sysctlfix-enabled).
When disabled, the chart omits the `apply-sysctl-overwrites` init container;
Cilium can still program live sysctls for interfaces it creates.

## Verify The Handoff

Wait for the Cilium components, nodes, and cluster DNS:

```sh
cilium status --kubeconfig ./kubeconfig --wait
kubectl --kubeconfig ./kubeconfig wait \
  --for=condition=Ready node --all --timeout=5m
kubectl --kubeconfig ./kubeconfig get nodes
kubectl --kubeconfig ./kubeconfig -n kube-system get pods
```

Confirm the redundant init container was not rendered:

```sh
kubectl --kubeconfig ./kubeconfig -n kube-system \
  get daemonset cilium \
  -o jsonpath='{.spec.template.spec.initContainers[*].name}{"\n"}'
```

The output must not contain `apply-sysctl-overwrites`.

On each node, confirm the effective host settings after Cilium has created its
interfaces:

```sh
sysctl net.ipv4.ip_forward
sysctl net.ipv4.conf.all.rp_filter
sysctl net.ipv4.conf.default.rp_filter
sysctl net.ipv4.conf.cilium_host.rp_filter
sysctl net.ipv4.conf.cilium_net.rp_filter
```

Forwarding must be `1`; every listed `rp_filter` value must be `0`. Then run a
Cilium connectivity test appropriate for the cluster:

```sh
cilium connectivity test --kubeconfig ./kubeconfig
```

Repeat the health, sysctl, and connectivity checks after a node reboot. This
proves both KatlOS boot-time policy and Cilium's handling of newly created
interfaces.

## Change Host Sysctls Deliberately

If another CNI or a site-specific network design requires different persistent
host settings, declare a native `/etc/sysctl.d/*.conf` file through
`hostConfiguration.sets` and apply the complete `ClusterConfig`; see
[Apply cluster configuration](configure-nodes.md#configure-native-linux-facilities).
Katl validates and carries the file in the selected generation.

Do not use a privileged workload to create persistent host configuration.
A CNI that requires a writable `/etc` and does not provide a supported
disable or redirect setting is not compatible with KatlOS until that behavior
can be changed.

## Diagnose The Default Cilium Setting

An installation that omitted `sysctlfix.enabled=false` may log:

```text
unable to create cilium sysctl overwrites config:
open /etc/sysctl.d/99-zzz-override_cilium.conf: read-only file system
```

Cilium 1.19.6 treats that write failure as non-fatal, but relying on the warning
is not the supported KatlOS procedure. Add the Helm value to the installation's
retained desired state and perform the normal Cilium upgrade or reconciliation.
Do not edit the active node filesystem.
