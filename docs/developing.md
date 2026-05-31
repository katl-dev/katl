# Developing Katl

This document describes the local tooling expected for early Katl development.
The immediate goal is M1: build a minimal installer OS with mkosi, boot it in a
local VM, and prove the boot by matching deterministic serial output.

## Current VM Stance

Use the simplest layer that proves the OS boots:

1. Build the image with `mkosi`.
2. Boot with `mkosi vm` if it exposes enough console and log control.
3. Fall back to a small direct `qemu-system-x86_64` wrapper when the test
   harness needs deterministic serial capture.
4. Add `libvirt`/`virsh` after the direct boot smoke loop works.

`virt-manager` is useful for interactive debugging, but it is not a project
dependency and should not be required by automated tests.

## M1 Boot Contract

M1 proves only the local installer OS build/boot/test loop.

- Build tool: `mkosi 26`.
- Base distribution: Fedora, chosen for current systemd and mkosi support.
- Output format: a bootable `disk` image with systemd-boot/UKI support.
- Output directory: `build/mkosi/`.
- Primary artifact name: `katl-installer.raw`.
- Build command: `mkosi -f build`.
- Interactive boot command: `mkosi vm --firmware uefi --console console`.
- Required smoke path: direct `qemu-system-x86_64` with EFI firmware and
  deterministic serial capture.
- Firmware expectation: QEMU can find OVMF/edk2 firmware descriptors, or the
  runner is given explicit firmware paths.
- Serial settings: the guest kernel command line includes
  `console=ttyS0,115200n8`; QEMU exposes that serial device to a stable log.
- Stable boot signal: `Katl hello`.
- Generated VM logs and scratch state belong under `build/`.

Direct QEMU is the automation contract because it gives the smoke harness
stable process control, serial output, timeout handling, and exit details.
`mkosi vm` remains the preferred manual first look when it exposes enough
console output on the developer machine. KVM should be used when available, but
the runner must detect missing `/dev/kvm` and either explain the missing
acceleration or use QEMU TCG for the first functional smoke test.

M1 is explicitly not a real installer. It must not partition, format, or mutate
host disks. It also excludes `katlc`, kubeadm/etcd persistence, A/B root
updates, libvirt as a requirement, GUI tools, and end-user asset publishing.

## Required For M1

- `scripts/mkosi`: builds the installer OS image.
- `qemu-system-x86_64`: boots the image locally.
- KVM access: the VM test runner should see `/dev/kvm`.
- UEFI firmware for QEMU, such as edk2/OVMF firmware descriptors.
- `bd`: tracks project tasks.
- `git commit-wrapped`: required for agent-authored commits.

Go is reserved for Katl product code, including installer/runtime code that
runs in the built image. Early build and boot wrappers should stay thin shell.

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

For manual VM work through `mkosi vm`, use the optional VM shell:

```sh
nix develop .#vm
```

The VM shell adds QEMU/KVM and OVMF firmware packages. It does not configure
host libvirt, `/dev/kvm`, `/dev/net/tun`, bridges, or polkit access; keep those
in the NixOS host configuration.

## Optional During M1

- `libvirt` and `virsh`: useful for later persistent VM definitions, networks,
  multi-node tests, and longer integration tests.
- `virt-install`: useful for manual libvirt VM creation.
- `virt-manager`: useful GUI for inspecting and debugging local VMs.
- `/dev/net/tun` and `vhost_net`: useful once tests need richer VM networking.

## Sanity Checks

Run these from the same shell/session that will build and test Katl:

```sh
mkosi --version
qemu-system-x86_64 --version
virsh --version
virt-install --version
virt-manager --version
bd --version
git commit-wrapped --help
```

Check virtualization access:

```sh
ls -l /dev/kvm
virt-host-validate qemu
virsh -c qemu:///system list --all
```

Check QEMU can discover UEFI firmware:

```sh
ls /run/current-system/sw/share/qemu/firmware
ls /run/current-system/sw/share/qemu/edk2-x86_64-code.fd
```

The exact firmware path is host-specific. On some distributions the equivalent
files live under `/usr/share/OVMF` or `/usr/share/qemu`.

## Common Issues

If `/dev/kvm` is missing, load the host KVM module and confirm virtualization is
enabled in firmware. If `/dev/kvm` exists but cannot be opened, add the user
running VM tests to the host's KVM access group or equivalent policy.

If `virsh -c qemu:///system list --all` fails with a polkit error, run it from a
session with a polkit agent, configure the host's libvirt access policy, or use
an explicit privileged manual check. Katl automation should not depend on a GUI
polkit prompt.

If `qemu:///session` fails under Codex or another sandbox, prefer
`qemu:///system` for manual libvirt checks. The direct QEMU smoke harness should
remain the first required path.

## Current Tooling Snapshot

The local environment was checked on 2026-05-31:

- `mkosi 26`
- `qemu-system-x86_64 10.2.2`
- `virsh 12.2.0`
- `virt-install 5.1.0`
- `virt-manager 5.1.0`
- `go 1.26.3`
- `bd 1.0.3`

At that time, `qemu:///system` worked from this shell and `libvirtd` was active.
`/dev/kvm` and `/dev/net/tun` were not visible from the Codex sandbox even
though the host reported hardware virtualization support and the `kvm_intel`,
`kvm`, and `tun` modules were loaded.
