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

KatlOS enables Kubernetes IP forwarding. Reverse-path filtering depends on the
CNI datapath and routing topology, so Katl does not override the distribution's
filtering policy. Cilium-specific filtering settings belong in the cluster's
retained host configuration.

## Configure Cilium Host Settings

Copy the [example sysctl file](../examples/cilium/90-cilium.conf) next to your
ClusterConfig and include it in the existing defaults:

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

Cilium's default `apply-sysctl-overwrites` init container tries to write
`/etc/sysctl.d/99-zzz-override_cilium.conf`, but Katl owns immutable `/etc`.
Disable that writer with `sysctlfix.enabled=false` after declaring the retained
host settings above. Do not make `/etc` writable to accommodate it.

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

For node-local API access, set `k8sServiceHost=127.0.0.1` and
`k8sServicePort=7445`. Katl's proxy forwards TLS to a healthy control-plane
backend, and new clusters include `127.0.0.1` in the API serving certificate.
Keep certificate verification enabled. These settings have also been exercised
with Cilium 1.20.2.

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

Katl's default DHCP fallback does not match virtual netdev kinds, so
`cilium_host`, Cilium veth links, and overlay devices remain unmanaged by
systemd-networkd. If a node uses operator-authored networkd files instead of
the fallback, keep their matches limited to host-owned links so they do not
claim Cilium interfaces.

Repeat the health, sysctl, and connectivity checks after a node reboot. This
proves both KatlOS boot-time policy and Cilium's handling of newly created
interfaces.

## Change Host Sysctls Deliberately

If another CNI or a site-specific network design requires different persistent
host settings, declare a native `/etc/sysctl.d/*.conf` file through
`hostConfiguration.fileSets` and apply the complete `ClusterConfig`; see
[Apply cluster configuration](configure-nodes.md#configure-native-linux-facilities).
Katl validates and carries the file in the selected generation.

Do not use a privileged workload to create persistent host configuration.
A CNI that requires a writable `/etc` and does not provide a supported
disable or redirect setting is not compatible with KatlOS until that behavior
can be changed.

## Repair An Existing API Certificate

An older cluster may lack the loopback certificate address. On a control-plane
node, test the actual TLS identity rather than the canonical-name override in
the admin kubeconfig:

```sh
kubectl --kubeconfig /etc/kubernetes/admin.conf \
  --server=https://127.0.0.1:7445 --tls-server-name=127.0.0.1 \
  get --raw=/readyz
```

A certificate-address error requires replacing the API serving certificate;
an OS or Cilium upgrade does not rewrite existing cluster PKI. Until repaired,
Cilium can use a reachable node management address already in the certificate
with port `7445`.

Use kubeadm's native certificate procedure, one control-plane node at a time:

1. Back up `apiserver.crt`, `apiserver.key`, and the `ClusterConfiguration` from
   the `kube-system/kubeadm-config` ConfigMap in a root-only writable recovery
   directory. Record every current certificate SAN and the node's advertised
   API address. Keep the cluster CA unchanged.
2. Add `127.0.0.1` to `apiServer.certSANs` in a copy of that configuration,
   preserving existing SANs. Create a separate staging directory with references
   to the existing `ca.crt` and `ca.key`. Set `certificatesDir` to that staging
   directory and include an `InitConfiguration` with this node's existing
   name, API advertise address, and bind port.
3. Run `kubeadm init phase certs apiserver --config staged-config.yaml`.
   Independently verify the generated certificate against the existing CA and
   check that it includes every old SAN plus `127.0.0.1` before replacing
   anything live. With an external CA, have that CA issue the replacement.
4. Upload the updated cluster configuration using
   `kubeadm init phase upload-config kubeadm --config cluster.yaml --kubeconfig
   /etc/kubernetes/admin.conf`, with `certificatesDir` restored to
   `/etc/kubernetes/pki`. This retains the SAN for future control-plane joins.
5. Install the staged serving certificate and key into the existing PKI
   directory, retaining root ownership and private-key mode `0600`. Restart
   the API-server container using `crictl stop` with its running container ID;
   kubelet recreates it. Allow for a brief API interruption on a single-node
   control plane. If it fails to recover, restore the saved pair and restart
   that container again before proceeding.
6. Repeat the TLS readiness check above, then verify node readiness, Cilium,
   cluster DNS, and service traffic. Set Cilium's retained API host to loopback
   only after all control-plane certificates pass. Repeat verification after
   a public `katlctl node reboot`.

`kubeadm certs renew apiserver` preserves SANs from the old certificate, so it
does not add the missing address. See Kubernetes'
[certificate management](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/)
and [API certificate phase](https://kubernetes.io/docs/reference/setup-tools/kubeadm/generated/kubeadm_init/kubeadm_init_phase_certs_apiserver/).

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
