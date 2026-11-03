# Join a worker to an external control plane

This experimental workflow provisions a KatlOS worker for an existing Kubernetes
control plane managed outside Katl, including Talos. Katl manages the worker OS,
containerd, kubelet, and its persistent generation. Your existing cluster manages
bootstrap authentication, certificate approval and renewal, CNI, kube-proxy or its
replacement, and workloads.

Katl uses [kubelet TLS bootstrap](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/).
It does not run `kubeadm join`, create kubeadm ConfigMaps, manage Talos machines,
or need a Katl control-plane node. Do not run `katlctl cluster bootstrap` for this
workflow.

## Prerequisites

- A reachable Kubernetes API endpoint with a certificate trusted by the cluster
  CA, and a Kubernetes version available in the Katl compatibility catalog.
  Select a kubelet version compatible with your API server's
  [version skew policy](https://kubernetes.io/releases/version-skew-policy/).
- Administrator `kubectl` access to that cluster on your workstation. Keep this
  administrator kubeconfig separate from the bootstrap kubeconfig sent to Katl.
- Bootstrap-token authentication and a functioning kubelet client certificate
  signer on the external control plane. The cluster must authorize bootstrap
  CSRs and subsequent client certificate renewal.
- The cluster's DNS service IP and DNS domain, plus a CNI deployment that supports
  both Talos and ordinary Linux workers. Check DaemonSet selectors, host paths,
  mounts, and kube-proxy replacement settings. Talos-specific CNI settings must
  not exclude the Katl worker or assume a Talos filesystem layout.
- Worker connectivity to the API, registries, DNS, other nodes, and the CNI's
  required ports. The control plane must reach kubelet on TCP `10250` for logs,
  exec, and metrics. Katl's management interface on TCP `9443` remains separate.

For Talos, inspect API server arguments through Kubernetes and manage any
required changes through your normal Talos machine configuration workflow:

```sh
kubectl -n kube-system get pods -l component=kube-apiserver -o yaml
```

If bootstrap-token authentication is disabled, the Talos 1.14 configuration
document is:

```yaml
apiVersion: v1alpha1
kind: KubeAPIServerConfig
extraArgs:
  enable-bootstrap-token-auth: "true"
```

Earlier Talos versions use `cluster.apiServer.extraArgs` for this flag.
Apply it using the configuration procedure for your Talos version. See the
[Talos machine configuration reference](https://docs.siderolabs.com/talos/v1.14/reference/configuration/v1alpha1/config).
Katl does not change your control-plane configuration. A Talos machine-join token
is not the Kubernetes bootstrap token used below.

### Make Talos add-ons portable

Talos can generate add-on configuration that connects to KubePrism at
`127.0.0.1:7445`. That address belongs to each Talos host. External Katl workers
need their CNI and service-routing agents to use a shared reachable API endpoint.
A running kube-proxy pod and a `Ready` Node do not prove that service routing
works.

For the Talos default Flannel and kube-proxy deployment, inspect both:

```sh
kubectl -n kube-system get ds kube-flannel -o yaml
kubectl -n kube-system get cm kubeconfig-in-cluster -o yaml
```

In an experimental cluster using these resources, the following changes select
the shared endpoint. Replace the example address and retain equivalent desired
configuration in your Talos or GitOps workflow; generated manifests can be
replaced during later control-plane operations.

```sh
API_ENDPOINT=https://api.lab.example:6443
kubectl -n kube-system set env ds/kube-flannel \
  KUBERNETES_SERVICE_HOST=api.lab.example KUBERNETES_SERVICE_PORT=6443

kubectl -n kube-system get cm kubeconfig-in-cluster -o json |
  jq --arg endpoint "$API_ENDPOINT" \
    '{data:{kubeconfig:(.data.kubeconfig | sub("server: [^\n]+"; "server: " + $endpoint))}}' \
    > kubeconfig-in-cluster-patch.json
kubectl -n kube-system patch cm kubeconfig-in-cluster --type merge \
  --patch-file kubeconfig-in-cluster-patch.json
kubectl -n kube-system rollout restart ds/kube-proxy
```

Talos also supplies the Flannel CNI binary in its OS. Its default DaemonSet
installs configuration without copying that binary to other operating systems.
Install the plugin on Katl workers through your retained CNI deployment. The
[example Flannel CNI installer](../examples/external-workers/flannel-cni.yaml)
targets only nodes labeled `katl.dev/cni: flannel` and writes to Katl's writable
CNI directory. Set that label in each worker's `kubernetes.labels` before
installation, or use `kubectl label node worker-1 katl.dev/cni=flannel` for an
already registered worker. Apply the example with:

```sh
kubectl apply -f flannel-cni.yaml
```

Keep the plugin image version aligned with your Flannel deployment. Do not mount
or overwrite Talos's packaged CNI binaries with this installer. A
`FailedCreatePodSandBox` event naming a missing `flannel` plugin indicates that
this installation is incomplete.

For Cilium or another CNI, make the equivalent API endpoint selection in its
retained configuration. Keep TLS verification enabled and review its host
configuration requirements, including KatlOS's
[immutable host boundary](cilium.md#preserve-the-immutable-host-boundary).

## Configure and provision the worker

Create `cluster.yaml`. Replace the version, addresses, and disk identity with
values for your cluster and worker. Replace `YOUR_PUBLIC_KEY` with your SSH
public key. Include only Katl workers in `nodes`:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: external-lab
spec:
  managementAuthentication: trusted-network
  kubernetes:
    version: v1.36.1
    external:
      endpoint: https://api.lab.example:6443
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ssh-ed25519 YOUR_PUBLIC_KEY
    kubernetes:
      kubelet:
        configFile: kubelet.yaml
  nodes:
    - name: worker-1
      management:
        address: 192.168.1.51
      kubernetes:
        address: 192.168.1.51
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-worker-root
```

External mode excludes `controlPlane: true`, `spec.controlPlaneEndpoint`, and
`spec.kubernetes.kubeadm`. Use the existing cluster's reachable HTTPS endpoint,
including its port. Katl workers connect directly to it; they do not advertise
an API VIP or depend on Katl's local API proxy.

Create the adjacent `kubelet.yaml`, setting the actual cluster DNS and domain:

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
clusterDNS:
  - 10.96.0.10
clusterDomain: cluster.local
```

Other native kubelet fields can be added. Katl supplies the systemd cgroup driver,
containerd socket, client certificate rotation, webhook authentication and
authorization, and the cluster CA for kubelet client authentication. This
experiment assumes the API's CA also trusts the control plane's kubelet client
certificate. Configure serving-certificate approval separately if you enable
`serverTLSBootstrap: true`; client certificate approval does not approve serving
certificates.

Boot the worker's installer media and follow the
[installation procedure](../installing.md). Use this same configuration for both
installation and enrollment:

```sh
katlctl config validate ./cluster.yaml
katlctl install apply --config ./cluster.yaml --node worker-1
katlctl node status worker-1 --config ./cluster.yaml
```

The worker must have a healthy committed installed generation before the first
join. Keep the source configuration and its referenced kubelet file together;
bootstrap credentials do not belong in either file or in a `.katlcfg` bundle.

## Prepare a bootstrap identity

Use your administrator kubeconfig for the following cluster operations. Set the
correct `KUBECONFIG` or `kubectl --context` before running them. These changes
create bootstrap authorization and one short-lived token in the external cluster.
Review existing cluster policies first; an existing approver might already
handle initial or renewal CSRs.

Authorize the Katl bootstrap group to request client certificates, and allow
existing node identities to renew their own client certificates:

```yaml
# katl-worker-rbac.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: katl-worker-bootstrap
subjects:
  - kind: Group
    name: system:bootstrappers:katl-external
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: system:node-bootstrapper
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: katl-worker-renewal
subjects:
  - kind: Group
    name: system:nodes
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:selfnodeclient
  apiGroup: rbac.authorization.k8s.io
```

Apply the bindings:

```sh
kubectl apply -f katl-worker-rbac.yaml
```

The commands below require OpenSSL and `jq`. They create a token valid for one
hour and a self-contained bootstrap kubeconfig for the selected cluster. Set
`API_ENDPOINT` to the exact URL in `cluster.yaml`. The administrator's credentials
are not copied into the resulting file:

```sh
umask 077
API_ENDPOINT=https://api.lab.example:6443
TOKEN_ID=$(openssl rand -hex 3)
TOKEN_SECRET=$(openssl rand -hex 8)
EXPIRES=$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)

kubectl -n kube-system create secret generic "bootstrap-token-$TOKEN_ID" \
  --type=bootstrap.kubernetes.io/token \
  --from-literal=token-id="$TOKEN_ID" \
  --from-literal=token-secret="$TOKEN_SECRET" \
  --from-literal=expiration="$EXPIRES" \
  --from-literal=usage-bootstrap-authentication=true \
  --from-literal=auth-extra-groups=system:bootstrappers:katl-external

kubectl config view --raw --flatten --minify -o json |
  jq --arg endpoint "$API_ENDPOINT" --arg token "$TOKEN_ID.$TOKEN_SECRET" '
    {apiVersion: "v1", kind: "Config",
     clusters: [{name: "external", cluster: {
       server: $endpoint,
       "certificate-authority-data": .clusters[0].cluster["certificate-authority-data"]}}],
     users: [{name: "bootstrap", user: {token: $token}}],
     contexts: [{name: "bootstrap", context: {cluster: "external", user: "bootstrap"}}],
     "current-context": "bootstrap"}' > worker-bootstrap.conf
unset TOKEN_SECRET
```

The expiry command uses GNU `date`; on macOS use `date -u -v+1H` instead of
`date -u -d '+1 hour'`. The input kubeconfig must trust the same CA as
`API_ENDPOINT`. Katl accepts one embedded CA, token user, and selected context.
It rejects insecure TLS, external file references, credential plugins, and client
certificates. The external cluster owns token permissions and expiration; Katl
cannot infer privileges from an opaque token.

## Join and approve the worker

Start enrollment:

```sh
katlctl node join worker-1 --config ./cluster.yaml \
  --bootstrap-kubeconfig ./worker-bootstrap.conf
```

Katl activates the Kubernetes runtime and starts kubelet. If the cluster does not
automatically approve the initial client CSR, use a second terminal to inspect it:

```sh
kubectl get csr
kubectl get csr CSR_NAME -o jsonpath='{.spec.username}{"\n"}{.spec.signerName}{"\n"}'
kubectl get csr CSR_NAME -o jsonpath='{.spec.request}' |
  base64 --decode | openssl req -noout -subject -text
```

Replace `CSR_NAME` with the request you inspected. Confirm its bootstrap identity
is `system:bootstrap:TOKEN_ID`, its signer is
`kubernetes.io/kube-apiserver-client-kubelet`, its organization is `system:nodes`,
and its common name is `system:node:worker-1`. Then approve that request:

```sh
kubectl certificate approve CSR_NAME
```

The join waits for the rotating kubelet client credential, a healthy persistent
generation, and Kubernetes Node readiness. CNI failures can leave a registered
worker `NotReady`; registration alone does not complete the workflow. A healthy
join does not reboot the worker. Repeating a completed join is a no-op and no
longer needs the bootstrap file.

After success, revoke the bootstrap token and remove its local file:

```sh
kubectl -n kube-system delete secret "bootstrap-token-$TOKEN_ID"
rm worker-bootstrap.conf
```

Katl removes its temporary bootstrap material after enrollment. While a CSR is
pending, the node retains its bootstrap kubeconfig on writable state so kubelet
can continue enrollment. Its generated client identity and key remain on the
worker for restart and renewal.

## Verify the resulting worker

Check the worker through both management and Kubernetes:

```sh
katlctl node status worker-1 --config ./cluster.yaml
kubectl wait --for=condition=Ready node/worker-1 --timeout=5m
kubectl get node worker-1 -o wide
katlctl node join worker-1 --config ./cluster.yaml
katlctl node reboot worker-1 --config ./cluster.yaml
kubectl wait --for=condition=Ready node/worker-1 --timeout=5m
```

Also schedule a representative workload on this worker and verify pod-to-pod
traffic, service routing, DNS, and `kubectl logs` or `exec`. These checks exercise
CNI coverage and API-to-kubelet connectivity that Node readiness alone cannot
prove. Keep renewal authorization in place for the lifetime of the node.

## Recover a pending join

Inspect `katlctl node status`, `katlctl operations list`, and the worker's kubelet
journal before changing its state. Check API reachability, token expiry, CSR
approval, certificate signing, and CNI readiness. Retry the same `node join`
command after correcting the cause. A recorded successful kubelet start can
continue certificate and health checks without replacing its persisted identity,
including after reboot, if its source configuration and generation still match.
If the bootstrap token expired before enrollment, create a replacement token and
bootstrap kubeconfig for the same endpoint and CA, then retry with that file
after the pending operation finishes. Katl refreshes the bootstrap credential
and restarts kubelet while preserving its existing configuration and node key.

If kubelet enrollment failed before that receipt, Katl refuses to treat partial
state as a fresh join. Preserve its state and inspect the reported operation.
Do not wipe a node merely to hide a failed enrollment.

## Configure and upgrade the worker

Use the ordinary Katl commands for host configuration and native kubelet
configuration. Edit the referenced `kubelet.yaml`, then preview and apply:

```sh
katlctl cluster apply --config ./cluster.yaml --plan
katlctl cluster apply --config ./cluster.yaml
```

Katl activates the desired generation, backs up changed kubelet configuration,
restarts kubelet when needed, and checks runtime health and Node readiness.
Enrollment credentials, CA, and the rotating client identity are retained.
Repeating the apply validates convergence without restarting a healthy,
unchanged kubelet. Host settings that require reboot follow the usual
next-boot workflow.

KatlOS updates use the ordinary
[host upgrade workflow](upgrade-host.md). `katlctl node upgrade` preserves the
external enrollment and verifies that the worker returns healthy after reboot.

Upgrade the external control plane through its own management interface first.
Set `spec.kubernetes.version` to the desired supported worker version, then run:

```sh
katlctl kubernetes upgrade --config ./cluster.yaml --plan
katlctl kubernetes upgrade --config ./cluster.yaml
katlctl cluster apply --config ./cluster.yaml
```

Katl verifies the external API server version using the enrolled worker's
credentials. A worker cannot be newer in minor version than the observed API
server or more than three minor versions older. With a load-balanced API,
complete the control-plane upgrade across its backends before upgrading workers;
one endpoint observation does not inventory every backend.

The upgrade verifies and activates the selected Kubernetes payload, restarts
kubelet, waits for the target version and a Ready Node, and promotes the new
persistent generation without reboot. It preserves native kubelet configuration
and enrollment. Katl does not download kubeadm cluster configuration, manage
external etcd, or change external add-ons. The existing optional
`--cordon --kubeconfig ./admin.conf` flags use administrator access only on your
workstation. Repeat the upgrade to confirm that the selected version is current,
and verify workloads after the change as you did after enrollment.

Changing an enrolled worker's endpoint, enrollment identity, or control-plane
ownership requires reprovisioning. Drain and remove a
worker through your external cluster's maintenance procedure before intentionally
replacing it; Katl does not coordinate removal with that control plane.

## Run the focused VM test

The focused regression uses one Katl control-plane VM and an independently
provisioned external worker. It removes kubeadm cluster configuration and stops
the control plane's Katl management agent before joining the worker, then checks
readiness, repeat operations, and cross-node Service traffic through the public
interfaces. Run it on a host with libvirt, KVM, and vsock available:

```sh
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run '^TestExternalWorkerJoin$' -count=1 -v -timeout 30m
```
