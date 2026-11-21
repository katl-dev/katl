# KatlOS documentation

Install KatlOS on your home-lab machines and use `katlctl` to manage them.
Katl can set up Kubernetes on those machines. You choose the cluster network
and manage the applications that run on it.

[Build your first KatlOS cluster](getting-started.md). Start with the release
ISO and finish with two Kubernetes nodes ready to run workloads. If you are
still choosing an operating system, [check what KatlOS supports](support.md).
If you already run Talos Linux, [KatlOS for Talos users](concepts/talos.md)
maps the concepts you know to their KatlOS equivalents.

## Install

- [Install `katlctl`](install-katlctl.md) for the release binary, Homebrew, and Nix.
- [Install KatlOS](installing.md) for ISO, network boot, and installer options.
- [Configure advanced installer networking](install-advanced.md) for static
  addresses, seed media, and routed installation.

## Other install journeys

- [Install with PXE and Matchbox](install-pxe-matchbox.md) to boot machines over
  the network.
- [Join an existing Kubernetes cluster](operations/external-workers.md) with a
  KatlOS worker.

## Operate and recover

- [Operate KatlOS](operations/README.md) to find the task for your node or
  cluster, including configuration, maintenance, and upgrades.
- [Secure the management API](operations/management-auth.md) to choose between
  trusted-network and mTLS management and to manage mTLS management secrets.
- [Troubleshoot KatlOS](operations/troubleshoot.md) to find where an operation
  failed and collect the information you need for recovery.

## Understand KatlOS and look up details

- [Generations](concepts/generations.md) explains how KatlOS stages, trials,
  and commits a host change, and when a node falls back.
- [What KatlOS owns](concepts/ownership.md) explains which parts of the system
  Katl manages and what a host rollback restores.
- [Kubernetes release delivery](concepts/kubernetes-releases.md) explains how
  Katl selects a compatible Kubernetes version for your nodes.
- [ClusterConfig reference](reference/cluster-config.md) describes each field
  in `cluster.yaml`.
- [`katlctl` command map](reference/katlctl.md) helps you find a command or use
  it in automation.
- [Release artifacts](reference/release-artifacts.md) explains which files you
  need for each install or upgrade method.

## Support and releases

- [Support and compatibility](support.md) covers tested environments,
  network trust, upgrade limits, and reporting problems.
- [Verify release artifacts](operations/verify-release.md) if you want to
  check the files you downloaded.
- [Release notes](https://github.com/katl-dev/katl/releases) describe changes
  and any steps you need to take for a specific release.
