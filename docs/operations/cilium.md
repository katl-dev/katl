# Run Cilium on KatlOS

Use this guide to prepare KatlOS hosts for Cilium, install Cilium with Helm
or the Cilium CLI, and check that the cluster network works. If you manage
Cilium with GitOps, keep the same KatlOS-specific values in that configuration.

## Before you start

- Choose a Cilium release that supports your Kubernetes version. The
  first-cluster walkthrough uses Cilium 1.20.2 with Kubernetes v1.36.1, and
  this procedure was also exercised with Cilium 1.19.10 and Helm 4.2.4. Check
  the [Cilium compatibility matrix](https://docs.cilium.io/en/stable/network/kubernetes/compatibility/)
  for the release you choose, selecting that release in the version menu if
  it differs from the version on the page.
- Review Cilium's
  [kubeadm installation guide](https://docs.cilium.io/en/stable/installation/k8s-install-kubeadm/)
  and release notes for the version being installed. This page covers only the
  KatlOS-specific settings, not a complete Cilium configuration.
- Upgrade Cilium before moving a node to a kernel that requires a newer Cilium
  patch release. Cilium 1.19.6 and 1.19.7 fail BPF feature probing on Linux 7.2;
  the [upstream fix](https://github.com/cilium/cilium/releases/tag/v1.19.8) is
  included in 1.19.8 and later patches in the 1.19 branch.
- For kube-proxy replacement, set `proxy.disabled: true` in the native kubeadm
  `ClusterConfiguration` before bootstrapping; see [native kubeadm
  configuration](../reference/cluster-config.md#cluster-networking-and-kube-proxy).
  Katl honors that setting when configuring node-local API access. Configure
  Cilium's replacement mode in your own Cilium values; Katl does not select it
  for you.

## Preserve the immutable host boundary

KatlOS owns `/etc` as part of the selected, versioned host generation. A
privileged Kubernetes workload may change live kernel state through
`/proc/sys`, but it must not persist configuration by writing directly beneath
`/etc`.

KatlOS enables Kubernetes IP forwarding. Reverse-path filtering depends on the
CNI datapath and routing topology, so Katl does not override the distribution's
filtering policy. Cilium-specific filtering settings belong in the cluster's
host configuration in `cluster.yaml`.

## Configure Cilium host settings

Copy the [example sysctl file](../examples/cilium/90-cilium.conf) next to
`cluster.yaml` and include it in the existing defaults:

```yaml
spec:
  defaults:
    hostConfiguration:
      fileSets:
        cilium:
          files:
            - path: /etc/sysctl.d/90-cilium.conf
              source: 90-cilium.conf
```

The example disables reverse-path filtering for the supported Cilium journey,
including explicit `lxc*` and `cilium_*` interface rules. Those interface rules
prevent distribution wildcard defaults from being reapplied when Cilium creates
new links. This is an opt-in Cilium configuration, not a requirement imposed on
other CNIs.

Include it before installing nodes. For existing nodes, apply the complete
ClusterConfig with `katlctl cluster apply --config cluster.yaml` and follow the
reported reboot requirement: wildcard interface rules are staged for next boot.
Verify the effective settings below after the reboot and after Cilium creates
its interfaces.

### Disable Cilium's sysctl writer

Cilium's default `apply-sysctl-overwrites` init container tries to write
`/etc/sysctl.d/99-zzz-override_cilium.conf`, but Katl owns immutable `/etc`.
After declaring the host settings above, disable that writer with the public
[Helm value](https://docs.cilium.io/en/stable/helm-reference/#sysctlfix-enabled)
`sysctlfix.enabled=false`. Do not make `/etc` writable to accommodate it. When
the value is disabled, the chart omits the `apply-sysctl-overwrites` init
container; Cilium can still program live sysctls for interfaces it creates.
Carry the same value in the Helm values or GitOps source used for later Cilium
upgrades.

An installation that omitted `sysctlfix.enabled=false` may log:

```text
unable to create cilium sysctl overwrites config:
open /etc/sysctl.d/99-zzz-override_cilium.conf: read-only file system
```

Cilium 1.19.6 and 1.20.2 treat that write failure as non-fatal, but relying on
the warning is not the supported KatlOS procedure. Add the Helm value to the
installation's desired state and perform the normal Cilium upgrade or
reconciliation. Do not edit the active node filesystem.

## Install Cilium

After [bootstrapping Kubernetes](bootstrap-kubernetes.md), install the selected
Cilium release with
[`sysctlfix.enabled=false`](#disable-ciliums-sysctl-writer). Replace the
example version in your command if you selected another release.

### Install with the Cilium CLI

```sh
cilium install \
  --kubeconfig ./kubeconfig \
  --version 1.20.2 \
  --set sysctlfix.enabled=false
```

### Install with Helm

```sh
helm install cilium oci://quay.io/cilium/charts/cilium \
  --kubeconfig ./kubeconfig \
  --version 1.20.2 \
  --namespace kube-system \
  --set sysctlfix.enabled=false
```

Other Cilium settings remain cluster-specific and operator-owned.

For node-local API access, set `k8sServiceHost=127.0.0.1` and
`k8sServicePort=7445`. Katl's proxy forwards TLS to a healthy control-plane
backend, and new clusters include `127.0.0.1` in the API serving certificate;
for an older cluster, first
[repair the API certificate](#repair-the-api-certificate-on-clusters-bootstrapped-before-202690).
Keep certificate verification enabled. These settings have also been exercised
with Cilium 1.20.2.

## Verify Cilium

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

Katl's default DHCP fallback does not match virtual netdev kinds, so
`cilium_host`, Cilium veth links, and overlay devices remain unmanaged by
systemd-networkd. If a node uses operator-authored networkd files instead of
the fallback, keep their matches limited to host-owned links so they do not
claim Cilium interfaces.

Repeat the health, sysctl, and connectivity checks after a node reboot. This
proves both KatlOS boot-time policy and Cilium's handling of newly created
interfaces.

## Change host sysctls deliberately

If another CNI or a site-specific network design requires different persistent
host settings, declare a native `/etc/sysctl.d/*.conf` file through
`hostConfiguration.fileSets` and apply the complete `ClusterConfig`; see
[Apply cluster configuration](configure-nodes.md#configure-native-linux-facilities).
Katl validates and carries the file in the selected generation.

Do not use a privileged workload to create persistent host configuration.
A CNI that requires a writable `/etc` and does not provide a supported
disable or redirect setting is not compatible with KatlOS until that behavior
can be changed.

## Repair the API certificate on clusters bootstrapped before 2026.9.0

Clusters bootstrapped with a Katl release earlier than 2026.9.0-beta.1 may lack
`127.0.0.1` in the API serving certificate. On a control-plane node, test the
loopback TLS identity directly, overriding the canonical name in the admin
kubeconfig:

```sh
kubectl --kubeconfig /etc/kubernetes/admin.conf \
  --server=https://127.0.0.1:7445 --tls-server-name=127.0.0.1 \
  get --raw=/readyz
```

A certificate-address error means the serving certificate must be reissued; an
OS or Cilium upgrade does not rewrite existing cluster PKI, and
`kubeadm certs renew apiserver` keeps the old SANs. Until it is repaired,
Cilium can use a reachable node management address that the certificate
already contains, with port `7445`.

Reissue the certificate with kubeadm, one control-plane node at a time, keeping
the cluster CA unchanged:

1. In a root-only recovery directory, back up `apiserver.crt`, `apiserver.key`,
   and the `ClusterConfiguration` from the `kube-system/kubeadm-config`
   ConfigMap. Record the current SANs and the node's advertised API address.
2. In a copy of that configuration, add `127.0.0.1` to `apiServer.certSANs`
   alongside the existing SANs. Set `certificatesDir` to a separate staging
   directory that references the existing `ca.crt` and `ca.key`, and include an
   `InitConfiguration` with this node's name, API advertise address, and bind
   port.
3. Run `kubeadm init phase certs apiserver --config staged-config.yaml`. Before
   replacing anything live, verify the new certificate against the existing CA
   and confirm it has every old SAN plus `127.0.0.1`. With an external CA, have
   that CA issue the replacement instead.
4. With `certificatesDir` restored to `/etc/kubernetes/pki`, upload the
   configuration so future control-plane joins keep the SAN:
   `kubeadm init phase upload-config kubeadm --config cluster.yaml --kubeconfig
   /etc/kubernetes/admin.conf`.
5. Install the staged certificate and key into the existing PKI directory with
   root ownership and private-key mode `0600`, then restart the API-server
   container with `crictl stop` and its running container ID; kubelet recreates
   it. A single-node control plane has a brief API interruption. If the API
   server does not recover, restore the saved pair and restart the container
   again before going further.
6. Repeat the TLS check above, then verify node readiness, Cilium, cluster DNS,
   and service traffic. Switch Cilium's API host to loopback in its Helm values
   only after every control-plane certificate passes, and verify again after a
   `katlctl node reboot`.

See Kubernetes'
[certificate management](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/)
and [API certificate phase](https://kubernetes.io/docs/reference/setup-tools/kubeadm/generated/kubeadm_init/kubeadm_init_phase_certs_apiserver/).
