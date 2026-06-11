# Developing Katl

This document describes the local tooling expected for early Katl development.
The immediate goal is to build a minimal installer OS with mkosi, boot it in a
local VM, and prove the boot by matching deterministic serial output. Katl should
move toward a usable system one working step at a time instead of carrying named
phase labels in docs.

Read `docs/internal/north-star.md` for the product direction that grounds the
local development loop.

## Current VM Stance

Use the libvirt-backed vmtest world as the supported automated VM layer:

1. Build current artifacts with `scripts/mkosi` or let `scripts/vmtest-run`
   build the current repo artifacts by default.
2. Run VM smokes with `scripts/vmtest-run` so world setup, host capability
   recording, libvirt lifecycle, and serial capture are handled consistently.
3. Use libvirt tools such as `virsh` for manual inspection of domains,
   networks, storage pools, and serial consoles.

`virt-manager` is useful for interactive debugging, but it is not a project
dependency and should not be required by automated tests.

## Local Boot Contract

The current local boot contract proves only the installer OS
build/boot/test loop.

- Build tool: `mkosi 26`.
- Base distribution: Fedora, chosen for current systemd and mkosi support.
- Output format: a bootable `disk` image with systemd-boot/UKI support.
- Output directory: `_build/mkosi/`.
- Primary artifact name: `katl-installer.raw`.
- Build command: `mkosi -f build`.
- Interactive boot command: `mkosi vm --firmware uefi --console console`.
- Required smoke path: `scripts/vmtest-run` with libvirt system VM execution
  and deterministic serial capture.
- Firmware expectation: the runner is given readable OVMF/edk2 pflash images.
- Serial settings: the guest kernel command line includes
  `console=ttyS0,115200n8`; libvirt exposes that serial device to the runner's
  captured console log.
- Stable boot signal: `Katl hello`.
- Generated VM logs and scratch state belong under `build/`.

The libvirt-backed vmtest path is the automation contract because it gives the
smoke harness stable process control, serial output, timeout handling, network
leases, storage setup, and exit details. KVM should be used when available, and
the runner records missing `/dev/kvm` access as a host capability gap.

The current local boot loop is explicitly not a real host installer. It must not
partition, format, or mutate host disks. It also excludes `katlc`, A/B root
updates, GUI tools, and end-user asset publishing.

## Required For The Current Loop

- `scripts/mkosi`: builds installer, runtime, Kubernetes sysext, and KatlOS
  image artifacts through the containerized mkosi builder.
- `scripts/vmtest-run`: runs enabled VM, first-install, installed-runtime, and
  multinode kubeadm smokes through a runner-created world.
- `libvirt`/`virsh`: defines, starts, observes, and tears down local VM test
  domains.
- KVM access: the VM test runner should see `/dev/kvm`.
- UEFI firmware for libvirt VM pflash boot, such as edk2/OVMF images.
- `git commit-wrapped`: required for agent-authored commits.

The supported top-level script surface is intentionally small. Use
`scripts/mkosi` for local build artifacts and `scripts/vmtest-run` for enabled
test worlds. Other scripts are compatibility wrappers, debug aids, or temporary
validators for scaffolding work; do not treat the whole `scripts/` directory as
the public developer interface.

## Nix Dev Shell

On NixOS or any host with flakes enabled, enter the minimum mkosi build shell:

```sh
nix develop
```

The repository also includes `.envrc`:

```sh
direnv allow
```

Use `direnv` plus `nix-direnv` if you want the flake shell loaded
automatically when entering the repository.

That shell provides mkosi, Fedora package tooling (`dnf5` and `rpm`), UKI
tooling, filesystem tools, compression tools, crypto tools, and CA
certificates needed by the current Fedora-based `mkosi.conf`.

Check the mkosi tool view from inside the shell:

```sh
nix develop --command mkosi dependencies
nix develop --command mkosi summary
```

Build the current installer image:

```sh
nix develop --command mkosi -f build
```

For manual VM work and VM tests, use the optional VM shell:

```sh
nix develop .#vm
```

The VM shell adds libvirt client tools, QEMU image tooling, and OVMF firmware
packages. It does not configure host libvirt, `/dev/kvm`, `/dev/net/tun`,
networks, storage pools, or polkit access; keep those in the NixOS host
configuration.

The host must provide:

- A running system libvirt daemon reachable at `qemu:///system`, or an explicit
  `KATL_VMTEST_LIBVIRT_URI` override.
- User access to the system libvirt daemon through the host's libvirt group,
  polkit policy, or an explicit privileged manual session.
- An active libvirt network named `default`, or
  `KATL_VMTEST_LIBVIRT_NETWORK` set to the active test network.
- An active libvirt storage pool named `default`, or
  `KATL_VMTEST_LIBVIRT_STORAGE_POOL` set to the active test pool.

## Optional During The Current Loop

- `virt-install`: useful for manual libvirt VM creation.
- `virt-manager`: useful GUI for inspecting and debugging local VMs.
- `/dev/net/tun` and `vhost_net`: useful for libvirt networks and richer VM
  networking.

## VM Test Worlds

Enabled VM, first-install, installed-runtime, and multinode kubeadm smokes run
through one runner-created world. Use `scripts/vmtest-run` instead of preparing
fixture environment files or invoking `scripts/vmtest-exec` directly:

```sh
scripts/vmtest-run ./internal/vmtest \
  -run 'FirstInstallTargetDisk|InstalledRuntime|ConfigApply' -count=1
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run 'TwoNodeKubeadmJoin|ThreeControlPlaneStackedEtcd' \
  -timeout 60m -count=1
```

The runner creates a temporary world under `${TMPDIR:-/tmp}/katl-vmtest/`, probes
host capabilities, records `world.json` and `host-capabilities.json`, exports the
world environment, and then executes `go test` with the caller's package patterns
and flags. Argument meaning belongs to `go test`; `scripts/vmtest-run` only adds
the harness execution needed to route compiled package test binaries through
`scripts/vmtest-exec`.

`go test` owns the terminal output and exit status. Scenario artifacts remain
under the world directory for later inspection or aggregation by another tool.

Plain `go test ./...` keeps enabled VM scenarios disabled. If an enabled smoke
is invoked directly without the world manifest, it fails with a setup error
naming `scripts/vmtest-run`.

World-backed smokes derive their fixture inputs from repo-controlled artifacts:
mkosi artifact indexes, KatlOS install images, runtime roots, generated install
manifests, per-node metadata, target disks, ESP artifact trees, and
installed-runtime fixtures. First-install smokes publish installed runtime
fixtures inside the world; installed-runtime and multinode kubeadm smokes
consume those world-published fixtures instead of asking the developer to export
disk, ESP, metadata, fixture, or node-address variables.

Build artifacts first when the local world cannot discover suitable outputs:

```sh
scripts/mkosi build-runtime
scripts/mkosi build-installer
scripts/mkosi build-katlos-install-image
```

World run directories and scenario manifests are the supported inspection path
for already-produced artifacts during harness development. Lower-level helper
scripts are not the supported way to run enabled VM or kubeadm suites.

### Capable-Host Proof

Run the full enabled world suite from the Nix VM shell on a host with readable
OVMF firmware, `/dev/kvm`, `/dev/vhost-vsock`, `/dev/net/tun`,
libvirt system access, an active libvirt network, and an active libvirt storage
pool:

```sh
nix develop .#vm -c env \
  KATL_VMTEST_RUN_ID=capable-host-$(date -u +%Y%m%dT%H%M%SZ) \
  scripts/vmtest-run ./... -count=1
```

A capable-host run exits zero after `go test` completes. The runner records
`world.json` and `host-capabilities.json` before handing off to Go; enabled
tests write their own per-scenario artifacts under the world directory. A
restricted host should fail during setup with explicit host capability gaps
rather than fixture generation errors.

## Sanity Checks

Run these from the same shell/session that will build and test Katl:

```sh
mkosi --version
virsh --version
virt-install --version
virt-manager --version
git commit-wrapped --help
```

Check virtualization access:

```sh
ls -l /dev/kvm
virt-host-validate qemu
virsh -c qemu:///system list --all
```

Check UEFI firmware configuration for VM runs:

```sh
test -n "${KATL_OVMF_CODE:-}" && test -r "$KATL_OVMF_CODE"
test -n "${KATL_OVMF_VARS:-}" && test -r "$KATL_OVMF_VARS"
```

Set `KATL_OVMF_CODE` and `KATL_OVMF_VARS` explicitly when the host keeps
OVMF/edk2 firmware somewhere outside the devshell defaults.

## Common Issues

If `/dev/kvm` is missing, load the host KVM module and confirm virtualization is
enabled in firmware. If `/dev/kvm` exists but cannot be opened, add the user
running VM tests to the host's KVM access group or equivalent policy.

If `virsh -c qemu:///system list --all` fails with a polkit error, run it from a
session with a polkit agent, configure the host's libvirt access policy, or use
an explicit privileged manual check. Katl automation should not depend on a GUI
polkit prompt.

On NixOS, the usual local setup is to enable `virtualisation.libvirtd`, enable
the default libvirt network and storage pool, and put the development user in a
group or polkit rule that can access `qemu:///system`.

If `qemu:///session` fails under Codex or another sandbox, prefer
`qemu:///system` for manual libvirt checks. The vmtest runner defaults to
`qemu:///system`.

## Current Tooling Snapshot

The local environment was checked on 2026-05-31:

- `mkosi 26`
- `virsh 12.2.0`
- `virt-install 5.1.0`
- `virt-manager 5.1.0`
- `go 1.26.3`

At that time, `qemu:///system` worked from this shell and `libvirtd` was active.
`/dev/kvm` and `/dev/net/tun` were not visible from the Codex sandbox even
though the host reported hardware virtualization support and the `kvm_intel`,
`kvm`, and `tun` modules were loaded.
