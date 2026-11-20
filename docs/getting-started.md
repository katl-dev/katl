# Build your first KatlOS cluster

Install KatlOS on two machines and use them to create a Kubernetes cluster.
The first node, `cp-1`, runs the Kubernetes API. The second node, `worker-1`,
runs workloads. This guide uses Cilium to connect workloads and ends with both
nodes reporting `Ready`.

## Before you begin

- Use two x86-64 UEFI machines with Secure Boot disabled. Give each machine
  one target disk that you can erase. You can use virtual machines.
- Put both machines and your workstation on the same trusted network. The
  installer uses DHCP to obtain an address. Reserve stable addresses for the
  nodes so that the configuration can still reach them after a reboot.
- Keep installer port `8080` and management port `9443` on trusted networks.
  The default connections use plaintext and grant access without credentials.
  If you need authenticated management, choose
  [mTLS](operations/access.md#security-boundary) before you install.
- Keep an SSH public key available for login to the installed hosts. Install
  `kubectl` and Helm on your workstation. The nodes need access to the
  registries for Kubernetes and Cilium.

## 1. Download the installer and boot both machines

Download `katl-installer.iso` from a [KatlOS release](https://github.com/katl-dev/katl/releases).
[Install katlctl](install-katlctl.md#install-the-release-binary) from that
same release on your workstation. The ISO contains the KatlOS image.
If you want to authenticate the downloads, see
[Verify release artifacts](operations/verify-release.md).

On each machine, attach the ISO as virtual media or write it to a USB drive
using a tool that copies the image directly. Boot from the media in UEFI mode.
The installer shows its address and waits for your configuration.
It leaves the target disk untouched while it waits.

## 2. Create and review the cluster configuration

From your workstation, find the two waiting installers.

```sh
katlctl install discover --timeout 10s
```

Match the addresses to the machines using their consoles. If discovery misses
a machine, use the address on its console. In the following command, replace
the example addresses. Put the machine you want to use as `cp-1` first and
`worker-1` second.

```sh
katlctl config init ./cluster.yaml \
  --installer 192.0.2.11 \
  --installer 192.0.2.21
```

If a machine has more than one eligible disk,
[select the disk yourself](installing.md#select-the-target-disk).

Open `cluster.yaml`. Match each name, role, address, and disk to the machine
you intend to install. Use a disk ID, WWN, or serial number to identify each
disk. A name such as `/dev/sda` can change across boots. Make sure that the
file contains your SSH public key. This example uses Kubernetes `v1.36.1`
with Cilium `1.20.2`. If you change `spec.kubernetes.version`, check
[Cilium's Kubernetes compatibility](operations/cilium.md#install-cilium) first.
If the key is missing, see
[how to select an SSH key](installing.md#select-an-ssh-key).

Add the
[Cilium host settings](operations/cilium.md#configure-cilium-host-settings)
to this file now. They configure how the hosts handle traffic from Cilium.
Then check the complete file.

```sh
katlctl config validate ./cluster.yaml
```

Keep `cluster.yaml`. You will use it to install the nodes, create the cluster,
and apply changes later.

## 3. Install KatlOS and check both nodes

The next commands erase the selected system disks. Before you run them, make
sure that each target disk is safe to erase.

```sh
katlctl install apply --config ./cluster.yaml --node cp-1
katlctl install apply --config ./cluster.yaml --node worker-1
```

The installer reports when it requests a reboot. Make sure that the machines
boot from the installed disks. If the firmware selects the installer again,
detach the ISO or remove the USB drive and boot from disk.

Check both nodes from your workstation.

```sh
katlctl cluster status --config ./cluster.yaml
```

If a machine is still rebooting, wait and run the command again. Both hosts
must report health `OK`, Kubernetes `not-configured`, and generation `0`.
The first installed version is *generation 0*. The Kubernetes API cannot
respond until you set it up in the next step. If a host fails to respond, see
[Troubleshoot KatlOS](operations/troubleshoot.md).

Record the installed node identities on your workstation before bootstrap.
This step lets `katlctl` detect when a saved address points to a different
machine.

```sh
katlctl context save --config ./cluster.yaml
```

## 4. Create the Kubernetes cluster

If you plan to keep this cluster, [create and back up its Kubernetes trust
keys](operations/kubernetes-identity.md) before bootstrap. Use the resulting
file with `--identity` when you run the following command. A disposable
cluster can use keys that Kubernetes generates during bootstrap.

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
If the bootstrap command fails or loses its connection, follow the
[bootstrap recovery guidance](operations/bootstrap-kubernetes.md#failure-boundary).

## 5. Connect workloads to the cluster network

Use Helm to [install Cilium](operations/cilium.md#install-with-helm) with the
KatlOS values. Cilium is a CNI plugin. It connects workloads to the cluster
network. You choose and manage the plugin.

Wait for both nodes to become ready. Then make sure that the cluster DNS
service starts.

```sh
kubectl --kubeconfig ./kubeconfig wait --for=condition=Ready node --all --timeout=5m
kubectl --kubeconfig ./kubeconfig -n kube-system rollout status deployment/coredns --timeout=5m
```

The nodes can now run workloads. Use the
[Cilium checks](operations/cilium.md#verify-the-handoff) to test connectivity
before you move workloads to the cluster.

Keep `cluster.yaml` and the files it references. Store your kubeconfig and any
trust keys privately. The [operator guide](operations/README.md) shows how to
maintain the nodes, update the cluster, and plan for recovery.
