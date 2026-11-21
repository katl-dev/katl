# Build your first KatlOS cluster

Install KatlOS on two machines and use them to create a Kubernetes cluster.
The first node, `cp-1`, runs the Kubernetes API. The second node, `worker-1`,
runs workloads. This guide uses Cilium to connect workloads and ends with both
nodes reporting `Ready`.

## Prerequisites

- Use two x86-64 UEFI machines with Secure Boot disabled. Give each machine
  one target disk that you can erase. You can use virtual machines.
- Give your workstation network access to both machines. The ISO installer
  uses DHCP by default. Reserve stable addresses for the nodes so that your
  workstation can reach them after a reboot. For static addresses or
  installation across subnets, see
  [Advanced installer networking](install-advanced.md).
- Know the IP address of each target node before running `katlctl` against
  it. If DHCP assigns the address at boot, read it from the installer console
  or match the machine's MAC address to its DHCP lease.
- Keep an SSH public key available for login to the installed nodes. Install
  `kubectl` and Helm on your workstation. The nodes need access to the
  registries for Kubernetes and Cilium.

## Before you start

Make two decisions before you install. The access mode takes effect when you
install the nodes, and the Kubernetes identity takes effect when you bootstrap
the cluster.

### Choose an access mode

By default, installer port `8080` and management port `9443` use plaintext
connections and grant access without credentials, so keep both on trusted
networks. If you need authenticated management, you must
[choose mTLS](operations/management-auth.md#choose-an-access-mode) before you
install. See [Network trust](support.md#network-trust) for what each port
exposes.

### Decide whether to keep this cluster

If you plan to keep this cluster, create and back up its
[Kubernetes identity](operations/kubernetes-identity.md) at the end of step 2,
once `cluster.yaml` names the cluster, and pass the file with `--identity` in
step 4. A disposable cluster can use keys that Kubernetes generates during
bootstrap.

## 1. Download the installer and boot both machines

Download `katl-installer.iso` from a [KatlOS release](https://github.com/katl-dev/katl/releases)
and [install katlctl](install-katlctl.md#install-the-release-binary) from that
same release on your workstation. The ISO contains the KatlOS image. If you
want to authenticate the downloads, see
[Verify release artifacts](operations/verify-release.md).

On each machine, attach the ISO as virtual media or write it to a USB drive
using a tool that copies the image directly, then boot from the media in UEFI
mode. The installer shows its address and waits for your configuration,
leaving the target disk untouched while it waits.

## 2. Create and review the cluster configuration

From your workstation, pass the IP addresses of the two waiting installers,
replacing the example addresses. Put the machine you want to use as `cp-1`
first and `worker-1` second.

```sh
katlctl config init ./cluster.yaml \
  --installer 192.0.2.11 \
  --installer 192.0.2.21
```

If you chose mTLS, add `--management-authentication mtls` to this command.
If a machine has more than one eligible disk,
[select the disk yourself](installing.md#select-the-target-disk).

Open `cluster.yaml` and review it:

1. Match each name, role, address, and disk to the machine you intend to
   install.
2. Identify each disk by a disk ID, WWN, or serial number, because a name
   such as `/dev/sda` can change across boots.
3. Make sure that the file contains your SSH public key. If the key is
   missing, see [how to select an SSH key](installing.md#select-an-ssh-key).
4. Check the versions. This example uses Kubernetes
   `v1.36.1` with Cilium `1.20.2`. If you change `spec.kubernetes.version`,
   check [Cilium's Kubernetes compatibility](operations/cilium.md#before-you-start)
   first.
5. Add the
   [Cilium host settings](operations/cilium.md#configure-cilium-host-settings),
   which configure how the nodes handle traffic from Cilium.
6. Check the complete file.

   ```sh
   katlctl config validate ./cluster.yaml
   ```

Keep `cluster.yaml`. You will use it to install the nodes, create the cluster,
and apply changes later.

If you plan to keep this cluster, create its Kubernetes identity now. The
cluster name must equal `metadata.name` in `cluster.yaml`, which is `katl-lab`
unless you changed it.

```sh
katlctl kubernetes identity create \
  --cluster-name katl-lab \
  --output ./katl-lab-kubernetes-identity.katlkey
```

Store a protected backup away from the workstation and the nodes, as
[Preserve Kubernetes identity](operations/kubernetes-identity.md#create-an-identity-before-first-bootstrap)
describes.

## 3. Install KatlOS and check both nodes

The next commands erase the selected system disks. Before you run them, make
sure that each target disk is safe to erase.

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
katlctl install apply --config ./cluster.yaml --node worker-1
```

The installer reports when it requests a reboot. Make sure that the machines
boot from the installed disks; if the firmware selects the installer again,
detach the ISO or remove the USB drive and boot from disk.

Check both nodes from your workstation.

```sh
katlctl cluster status --config ./cluster.yaml
```

If a machine is still rebooting, wait and run the command again. Both nodes
must report health `OK`, Kubernetes `not-configured`, and generation `0`,
because the first installed version is
[*generation 0*](concepts/generations.md) and the Kubernetes API cannot
respond until you set it up in the next step. If a node fails to respond, see
[Troubleshoot KatlOS](operations/troubleshoot.md).

Optionally, save a context for the cluster on your workstation. A context
records each installed node's identity and address so that later commands can
use `--context` instead of `--config`, and `katlctl` can detect when a saved
address points to a different machine.

```sh
katlctl context save --config ./cluster.yaml
```

This guide keeps passing `--config ./cluster.yaml`. Commands given `--config`
confirm each node's identity directly, so they do not need a saved context.

## 4. Create the Kubernetes cluster

If you created a Kubernetes identity in step 2, add
`--identity ./katl-lab-kubernetes-identity.katlkey` to the following command.

```sh
katlctl cluster bootstrap --config ./cluster.yaml --init-node cp-1
```

Katl reports progress as it starts Kubernetes on `cp-1` and joins
`worker-1`. It writes `./kubeconfig`, the file that lets `kubectl` connect to
the cluster. Check that the API responds and lists both nodes.

```sh
kubectl --kubeconfig ./kubeconfig get nodes
```

The nodes normally report `NotReady` until you install a cluster network.
If the bootstrap command fails or loses its connection, follow
[If bootstrap fails](operations/bootstrap-kubernetes.md#if-bootstrap-fails).

## 5. Connect workloads to the cluster network

Use Helm to [install Cilium](operations/cilium.md#install-with-helm) with the
KatlOS values. Cilium is the CNI plugin that connects workloads to the cluster
network; you choose and manage the plugin.

Wait for both nodes to become ready, then make sure that the cluster DNS
service starts.

```sh
kubectl --kubeconfig ./kubeconfig wait --for=condition=Ready node --all --timeout=5m
kubectl --kubeconfig ./kubeconfig -n kube-system rollout status deployment/coredns --timeout=5m
```

The nodes can now run workloads. Use the
[Cilium checks](operations/cilium.md#verify-cilium) to test connectivity
before you move workloads to the cluster.

Keep `cluster.yaml` and the files it references. Store your kubeconfig and any
Kubernetes identity file privately. The [operator guide](operations/README.md)
shows how to maintain the nodes, update the cluster, and plan for recovery.
