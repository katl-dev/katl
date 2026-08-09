# Build Your First KatlOS Cluster

This journey installs KatlOS from the release ISO, verifies generation 0,
bootstraps kubeadm, and hands the cluster to you for CNI installation. It is
appropriate for one or several x86-64 UEFI machines on a trusted home-lab
network.

Installation erases the selected system disk. Use disposable machines or make
independent backups before continuing. Keep installer TCP `8080` and management
TCP `9443` on trusted networks; installed management traffic is automatically
authenticated and encrypted.

## 1. Download One Release

Download the matching CLI and installer ISO from a single
[Katl release](https://github.com/katl-dev/katl/releases). This example uses a
placeholder beta version; substitute the release you selected:

```sh
VERSION=2026.7.0-beta.1
TAG="v$VERSION"
gh release download "$TAG" --repo katl-dev/katl \
  --pattern "katlctl-$VERSION-linux-amd64" \
  --pattern 'katl-installer.iso' \
  --pattern 'katl-installer.iso.sha256'
install -m 0755 "katlctl-$VERSION-linux-amd64" ~/.local/bin/katlctl
katlctl version
sha256sum --check katl-installer.iso.sha256
```

The CLI and KatlOS artifacts must come from the same release. Checksums are a
quick transport-integrity check; the optional
[release verification guide](operations/verify-release.md) adds GitHub build
attestation verification.

Attach the ISO as virtual media or write it to removable media with a tool that
performs a raw image copy. Boot every target in UEFI mode.

## 2. Discover the Waiting Installers

The installer boots into a non-mutating wait state and shows its address and
disk inventory on the console. From the operator workstation:

```sh
katlctl install discover
```

To create a starter config directly from all uniquely discovered installers:

```sh
katlctl install discover ./cluster.yaml
```

If discovery cannot cross the local network boundary, use the addresses shown
on the consoles:

```sh
katlctl config init ./cluster.yaml \
  --installer 192.0.2.11 \
  --installer 192.0.2.21
```

You can also describe nodes explicitly. The generated source is ordinary YAML
and remains the retained source of truth:

```sh
katlctl config init ./cluster.yaml \
  --node cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-KATL_CP_1_ROOT \
  --node worker-1=worker,192.0.2.21,/dev/disk/by-id/ata-KATL_WORKER_1_ROOT
```

## 3. Review the Destructive Inputs

Open `cluster.yaml` and verify, for every node:

- the node name and `controlPlane` role;
- the management address reachable from the workstation;
- the SSH public keys;
- the concrete Kubernetes version;
- the stable system-disk selector; and
- the stable control-plane endpoint for a multi-control-plane cluster.

Use `/dev/disk/by-id`, WWN, or serial identity. Never replace it with a
transient `/dev/sda` or `/dev/vda` name. Then validate the complete source:

```sh
katlctl config validate ./cluster.yaml
```

The [installation reference](installing.md#author-one-clusterconfig) documents
static networking, routed API advertisement, data volumes, native kubeadm
configuration, kernel arguments, and system extensions.

The first config or install preparation prints the path of a newly created
cluster management identity. Back up that `.katlkey` file. Katl discovers it
automatically for routine work; it is not another command-line input.

## 4. Install Each Node

Optional: enable the selected node's configured SSH keys in the live installer
without accepting an install or touching the disk:

```sh
katlctl install ssh --config ./cluster.yaml --node cp-1
ssh root@192.0.2.11
```

Submit one selected node and wait for autonomous installation and reboot:

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
katlctl install apply --config ./cluster.yaml --node worker-1
```

If a waiting installer has a temporary DHCP address different from its retained
management address, add `--endpoint TEMPORARY_ADDRESS`. Katl still selects the
logical node and disk from the config.

An existing signature on a destructive data-volume target causes a refusal
that names the exact `--acknowledge-storage-wipe NODE/VOLUME` flag. Inspect the
hardware and contents before repeating the command. System-disk installation is
always destructive once its validated plan proceeds.

## 5. Verify Generation 0

Remove or detach ISO media if firmware would otherwise boot it again. For a PXE
first boot order, use the installed-disk guard in the
[PXE guide](install-pxe-matchbox.md).

Check all nodes through the public management path:

```sh
katlctl cluster status --config ./cluster.yaml
katlctl node status cp-1 --config ./cluster.yaml
ssh katl@192.0.2.11
```

Expected generation 0 state is host health `OK`, an active management agent,
and Kubernetes `not-configured` or waiting for bootstrap. Kubernetes services
and Node readiness are not generation 0 requirements.

Enroll the installed nodes before bootstrap or any host mutation:

```sh
katlctl context save --config ./cluster.yaml
katlctl context show
```

This records each node's install-generated enrollment identity and machine ID.
`ClusterConfig` remains authoritative for desired state; the context is the
operator's durable binding between that inventory identity and its current
management address.

If this is a deliberate reinstall using the same backed-up management identity,
the old context will refuse the replacement machine. Re-enroll only that node
with `katlctl context save --config ./cluster.yaml --replace-node NODE`.

## 6. Bootstrap kubeadm

For a cluster you may rebuild, first create and independently back up its
Kubernetes trust identity. The name must match `metadata.name` in
`cluster.yaml`:

```sh
katlctl kubernetes identity create \
  --cluster-name homelab \
  --output ./homelab-kubernetes-identity.katlkey
katlctl kubernetes identity inspect \
  ./homelab-kubernetes-identity.katlkey
```

This mode-`0600` file contains CA private keys. Do not publish it with PXE
assets or keep its only copy on a cluster node. Read [Preserve Kubernetes
identity](operations/kubernetes-identity.md) for its exact scope and backup
requirements.

First inspect the non-mutating plan:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --init-node cp-1 --dry-run
```

Then perform bootstrap:

```sh
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey \
  --init-node cp-1
```

The command stages the release-compatible Kubernetes payload, creates the
cluster, joins the other listed nodes, trial-boots the resulting generation,
checks kubeadm and local control-plane health, and writes a mode-`0600`
`./kubeconfig`. Repeating the unchanged command observes or resumes the same
durable work instead of blindly starting over.

Verify the handoff:

```sh
kubectl --kubeconfig ./kubeconfig get nodes -o wide
kubectl --kubeconfig ./kubeconfig get pods -A
```

At this point the API should answer, but nodes normally remain `NotReady` and
CoreDNS remains pending. That is expected until you install a CNI.

## 7. Install Your Cluster Network

Choose and manage a CNI through your own Helm, CLI, or GitOps workflow. Katl
does not install one. If you choose Cilium, follow the tested
[KatlOS Cilium settings](operations/cilium.md); the immutable host requires
`sysctlfix.enabled=false`.

After installing networking, verify the platform outcome rather than only the
host:

```sh
kubectl --kubeconfig ./kubeconfig wait \
  --for=condition=Ready node --all --timeout=5m
kubectl --kubeconfig ./kubeconfig get pods -A
```

Deploy a small application with your normal workflow and prove DNS, Service
endpoints, and cross-node traffic before moving real workloads.

## 8. Retain Recovery Inputs

Keep the release tag and checksums, `cluster.yaml`, any referenced native files,
the kubeconfig, and independent etcd/application/data backups. Do not commit the
kubeconfig or private keys. Continue with the
[operator guide](operations/README.md), especially its lifecycle and recovery
boundaries.
