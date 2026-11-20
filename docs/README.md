# KatlOS documentation

Install KatlOS on your home-lab machines and use `katlctl` to manage them.
Katl can set up Kubernetes on those machines. You choose the cluster network
and manage the applications that run on it.

[Build your first KatlOS cluster](getting-started.md). Start with the release
ISO and finish with two Kubernetes nodes ready to run workloads. If you are
still choosing an operating system, [check what KatlOS supports](support.md).

## Install and provision

- [Install KatlOS](installing.md) for ISO, network boot, and installer options.
- [Configure advanced installer networking](install-advanced.md) for static
  addresses, seed media, and routed installation.
- [Install katlctl](install-katlctl.md) for the release binary, Homebrew, and Nix.
- [Install with PXE and Matchbox](install-pxe-matchbox.md) to boot machines over
  the network.
- [Join an existing Kubernetes cluster](operations/external-workers.md) with a
  KatlOS worker.
- [Verify release artifacts](operations/verify-release.md) if you want to
  check the files you downloaded.

## Operate and recover

- [Operate KatlOS](operations/README.md) to find the task for your node or
  cluster, including configuration, maintenance, and upgrades.
- [Troubleshoot KatlOS](operations/troubleshoot.md) to find where an operation
  failed and collect the information you need for recovery.

## Understand KatlOS and look up details

- [What KatlOS owns](concepts/ownership.md) explains which parts of the system
  Katl manages and what a host rollback restores.
- [Cluster configuration](reference/cluster-config.md) explains the fields in
  `cluster.yaml`.
- [katlctl command map](reference/katlctl.md) helps you find a command or use
  it in automation.
- [Kubernetes release delivery](concepts/kubernetes-releases.md) explains how
  Katl selects a compatible Kubernetes version for your nodes.
- [Release artifacts](reference/release-artifacts.md) explains which files you
  need for each install or upgrade method.

## Support and releases

- [Support and compatibility](support.md) covers tested environments,
  management access, upgrade limits, and reporting problems.
- [Release notes](https://github.com/katl-dev/katl/releases) describe changes
  and any steps you need to take for a specific release.
