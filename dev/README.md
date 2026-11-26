# Katl development tools

Use the [development guide](../docs/developing.md) to set up the build and test
environment and the [agent workflow](agents/README.md) for delivery roles
and hand-offs.

## `katl-queue`

Check whether a PR fits the GitHub merge queue, follow its progress, diagnose
removals, or plan queue work across a milestone. [katl-queue reference](../cmd/katl-queue/README.md)

## `katl-release`

Prepare a release tag, retrieve its final Actions bundle, or check release
integrity and qualification evidence. [katl-release reference](../cmd/katl-release/README.md)

## `codex-role`

Launch a scoped Codex job as a tracked systemd user service and capture its
report and exit status. [Launch and recover Codex jobs](agents/roles/orchestrator.md)

## `hermetic`

Run Go tests with Bubblewrap to isolate the test process from workstation
files. [Run tests in the Nix development shell](../docs/developing.md#nix-dev-shell)

## `vmtest-run`

Run automated VM tests in a libvirt world with recorded host capabilities and
per-run evidence. [Run VM tests on a capable host](../docs/developing.md#vm-tests-on-a-capable-host)

## `vmtest-clean`

Remove a retained VM test run and its recorded libvirt resources.
[VM test cleanup](../docs/developing.md#vm-tests-on-a-capable-host)

## `worktree-clean`

Remove a linked worktree after checking its identity and whether its tracked
work has been preserved. [Worktree cleanup](../docs/developing.md#vm-tests-on-a-capable-host)

## `katldev`

Keep a persistent libvirt VM world for hands-on install and lifecycle journeys
through the public `katlctl` interface.
[Work with a persistent development VM](../docs/developing.md#nix-dev-shell)

## `programs.katl-dev`

Import `katl.homeManagerModules.katl-dev` on a Linux development host to
install the packaged `katl-queue` and checkout-based development tools with
`enable`. Use `watcher.enable` to install the packaged queue command and its
systemd user service and socket, and `runner.enable` to install
`katl-dev-run` and its user slice. [Home Manager setup](../docs/developing.md#set-up-release-coordination-with-home-manager)
and [module source](../contrib/home-manager/katl-dev.nix).

## `katl-dev-run`

Run `katl-dev-run CMD [ARG...]` to put a development command in a delegated
systemd user scope under `katl-dev.slice`. The wrapper sets a Podman
configuration override so rootless containers use `cgroupfs` inside that
scope. [Runner setup and example](../docs/developing.md#set-up-release-coordination-with-home-manager)
and [runner source](../contrib/home-manager/katl-dev.nix).
