# KatlOS Documentation

KatlOS is an installable, upgradeable, systemd-native operating system for
kubeadm Kubernetes nodes. These guides cover the supported beta journey from a
blank UEFI machine to a cluster that is ready for you to install a CNI and the
rest of your platform.

KatlOS is experimental home-lab software. Before using real hardware, read the
[support and security boundary](support.md). In particular, the installer and
node-management APIs are designed for a trusted network and must not be exposed
to the Internet.

## Start Here

1. [Understand what Katl owns](concepts/ownership.md).
2. [Build your first cluster](getting-started.md) with the release ISO.
3. For network boot, use the [PXE and Matchbox journey](install-pxe-matchbox.md)
   instead of the ISO handoff.
4. [Bootstrap Kubernetes](operations/bootstrap-kubernetes.md), then install the
   CNI and add-ons you choose.
5. Use the [operator guide](operations/README.md) for routine and recovery
   work.

The [complete installation reference](installing.md) describes every
`ClusterConfig` field and advanced installation option. The focused journeys
above are the better place to begin.

## Install and Bootstrap

| Goal | Guide |
| --- | --- |
| Install a first ISO-based cluster | [Build your first cluster](getting-started.md) |
| Author and validate all installation inputs | [Installation reference](installing.md) |
| Automate UEFI network boot | [Install with PXE and Matchbox](install-pxe-matchbox.md) |
| Verify downloaded artifacts | [Verify release artifacts](operations/verify-release.md) |
| Inspect generation 0 and management access | [Access installed nodes](operations/access.md) |
| Create the kubeadm cluster | [Bootstrap Kubernetes](operations/bootstrap-kubernetes.md) |
| Install Cilium on the immutable host | [Run Cilium on KatlOS](operations/cilium.md) |

## Operate and Recover

| Goal | Guide |
| --- | --- |
| Inspect, reboot, shut down, or use SSH | [Access installed nodes](operations/access.md) |
| Apply host and kubeadm configuration | [Apply cluster configuration](operations/configure-nodes.md) |
| Add, replace, or remove a node | [Change cluster membership](operations/change-cluster-nodes.md) |
| Upgrade KatlOS | [Upgrade a KatlOS host](operations/upgrade-host.md) |
| Upgrade Kubernetes | [Upgrade Kubernetes](operations/upgrade-kubernetes.md) |
| Wipe and reinstall | [Wipe and reinstall KatlOS](operations/wipe-reinstall.md) |
| Diagnose a failure | [Troubleshoot KatlOS](operations/troubleshoot.md) |

For command discovery and automation boundaries, see the
[`katlctl` command map](reference/katlctl.md). The CLI's `--help` output remains
the exact reference for flags in the installed release.

## Project Documentation

- [Support boundary](support.md) defines the tested evaluation surface,
  compatibility promise, trust model, and reporting checklist.
- [Developing Katl](developing.md) covers builds, persistent `katldev` VMs,
  automated VM tests, and release tooling.
- `docs/internal` contains design records and project working material. It is
  not operator guidance or a supported product API and should not be published
  as part of the user documentation site.
