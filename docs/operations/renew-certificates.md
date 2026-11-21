# Renew Kubernetes certificates

Use this procedure to renew the kubeadm-issued certificates on your control
planes before they expire. Kubeadm issues its leaf certificates, including the
credentials in `/etc/kubernetes/admin.conf`, for one year by default. Katl does
not report certificate expiry or renew certificates on a schedule.

A [Kubernetes upgrade](upgrade-kubernetes.md) renews them for you: Katl runs
`kubeadm upgrade apply` and `kubeadm upgrade node` with kubeadm's default
certificate renewal. If you upgrade Kubernetes at least once a year, check
expiry after each upgrade and use this page only when a date is closer than
your next upgrade.

Renew before anything expires. Katl's control-plane health checks and
`katlctl cluster kubeconfig` use `/etc/kubernetes/admin.conf` on the node, so
an expired `admin.conf` breaks Katl's health gates and kubeconfig retrieval as
well as your own access.

## Before you start

- You can log in to each control plane as `root` over SSH; see
  [Access installed nodes](access.md).
- The cluster is healthy. Check with:

  ```sh
  katlctl cluster status --config ./cluster.yaml
  katlctl cluster etcd members --config ./cluster.yaml
  ```

- [Back up etcd](back-up-etcd.md) first if you do not have a recent snapshot.

Katl does not reconcile manual `kubeadm certs` changes away. Kubeadm owns the
files in `/etc/kubernetes`, which persist across host generations, so a host
rollback to a previous generation does not roll the certificates back.

## Renew one control plane at a time

Repeat these steps on each control plane, and finish verification on one node
before you start the next. Restarting etcd and the API server briefly
interrupts a single-control-plane cluster.

1. Log in to the control plane and check the current expiry dates:

   ```sh
   ssh root@192.0.2.11
   kubeadm certs check-expiration
   ```

2. Renew every kubeadm-managed certificate on the node:

   ```sh
   kubeadm certs renew all
   ```

3. Restart the static-pod containers so that they load the new certificates.
   Kubelet recreates each container after `crictl stop`:

   ```sh
   for name in kube-apiserver kube-controller-manager kube-scheduler etcd; do
     crictl stop $(crictl ps --name "^${name}\$" --state Running -q)
   done
   ```

4. Wait for the API server and check the new dates:

   ```sh
   kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw=/readyz
   kubeadm certs check-expiration
   ```

   Every certificate listed should now expire about one year from today. If
   `/readyz` does not return `ok` after a few minutes, inspect the containers
   with `crictl ps -a` before you touch another control plane.

5. From your workstation, confirm that the cluster and etcd are healthy before
   you move to the next control plane:

   ```sh
   katlctl cluster status --config ./cluster.yaml
   katlctl cluster etcd members --config ./cluster.yaml
   ```

## Refresh your kubeconfig

The kubeconfig that `katlctl` wrote embeds a client certificate from one
control plane's `admin.conf`. Renewal issues new credentials without revoking
the old ones, so the old kubeconfig keeps working only until its original
expiry date. After you renew, or after a Kubernetes upgrade, replace it:

```sh
katlctl cluster kubeconfig --config ./cluster.yaml --force
```

`--force` is required because the new kubeconfig differs from the existing
file.

## Certificate authorities

The cluster CAs last ten years from creation, both when
`katlctl kubernetes identity create` generates them and when kubeadm generates
them without a [Kubernetes identity](kubernetes-identity.md); an imported
identity keeps the dates of the CAs you imported. Katl does not
rotate CAs, and `katlctl` refuses to bootstrap with an identity whose CAs are
not currently valid. Before a CA expires, plan to rebuild the cluster with a new
identity, or follow the upstream
[manual CA rotation](https://kubernetes.io/docs/tasks/tls/manual-rotation-of-ca-certificates/)
procedure, which Katl does not run or verify.

## Kubelet certificates

`kubeadm certs renew` does not manage kubelet certificates. Kubelet serving
certificates use the kubeadm default, which is a self-signed certificate that
each kubelet generates, and Katl does not enable `serverTLSBootstrap`. If you
want CA-signed serving certificates, set it in a native `KubeletConfiguration`
document (see
[native kubeadm configuration](../reference/cluster-config.md#native-kubeadm-configuration))
and approve the resulting certificate signing requests with your own tooling.

See Kubernetes'
[certificate management with kubeadm](https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-certs/)
for what each certificate covers.
