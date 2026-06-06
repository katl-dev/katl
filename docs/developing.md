# Developing Katl

This document describes the local tooling expected for early Katl development.
The immediate goal is to build a minimal installer OS with mkosi, boot it in a
local VM, and prove the boot by matching deterministic serial output. Katl should
move toward a usable system one working step at a time instead of carrying named
phase labels in docs.

Read `docs/internal/north-star.md` for the product direction that grounds the
local development loop.

## Current VM Stance

Use the simplest layer that proves the OS boots:

1. Build the image with `mkosi`.
2. Boot with `mkosi vm` if it exposes enough console and log control.
3. Fall back to a small direct `qemu-system-x86_64` wrapper when the test
   harness needs deterministic serial capture.
4. Add `libvirt`/`virsh` after the direct boot smoke loop works.

`virt-manager` is useful for interactive debugging, but it is not a project
dependency and should not be required by automated tests.

## Local Boot Contract

The current local boot contract proves only the installer OS
build/boot/test loop.

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

The current local boot loop is explicitly not a real installer. It must not
partition, format, or mutate host disks. It also excludes `katlc`, kubeadm/etcd
persistence, A/B root updates, libvirt as a requirement, GUI tools, and end-user
asset publishing.

## Required For The Current Loop

- `scripts/mkosi`: builds installer, runtime, Kubernetes sysext, and KatlOS
  image artifacts through the containerized mkosi builder.
- `scripts/vmtest-run`: runs enabled nspawn, VM, first-install,
  installed-runtime, and multinode kubeadm smokes through a runner-created
  world.
- `qemu-system-x86_64`: boots the image locally.
- KVM access: the VM test runner should see `/dev/kvm`.
- UEFI firmware for QEMU, such as edk2/OVMF firmware descriptors.
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

For manual VM work through `mkosi vm`, use the optional VM shell:

```sh
nix develop .#vm
```

The VM shell adds QEMU/KVM and OVMF firmware packages. It does not configure
host libvirt, `/dev/kvm`, `/dev/net/tun`, bridges, or polkit access; keep those
in the NixOS host configuration.

## Optional During The Current Loop

- `libvirt` and `virsh`: useful for later persistent VM definitions, networks,
  multi-node tests, and longer integration tests.
- `virt-install`: useful for manual libvirt VM creation.
- `virt-manager`: useful GUI for inspecting and debugging local VMs.
- `/dev/net/tun` and `vhost_net`: useful once tests need richer VM networking.

## VM And Nspawn Test Worlds

Enabled nspawn, VM, first-install, installed-runtime, and multinode kubeadm
smokes run through one runner-created world. Use `scripts/vmtest-run` instead of
preparing fixture environment files or invoking `scripts/vmtest-exec` directly:

```sh
scripts/vmtest-run ./internal/vmtest -run Nspawn -count=1
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

Plain `go test ./...` keeps VM and nspawn scenarios disabled. If an enabled
smoke is invoked directly without the world manifest, it fails with a setup
error naming `scripts/vmtest-run`.

World-backed smokes derive their fixture inputs from repo-controlled artifacts:
mkosi artifact indexes, KatlOS install images, runtime roots, generated install
manifests, per-node metadata, target disks, ESP artifact trees, nspawn userspace
roots, and installed-runtime fixtures. First-install smokes publish installed
runtime fixtures inside the world; installed-runtime and multinode kubeadm
smokes consume those world-published fixtures instead of asking the developer to
export disk, ESP, metadata, fixture, or node-address variables.

Build artifacts first when the local world cannot discover suitable outputs:

```sh
scripts/mkosi build-runtime
scripts/mkosi build-installer
scripts/mkosi build-katlos-install-image
```

World run directories and scenario manifests are the supported inspection path
for already-produced artifacts during harness development. Lower-level helper
scripts are not the supported way to run enabled nspawn, VM, or kubeadm suites.

### Capable-Host Proof

Run the full enabled world suite from the Nix VM shell on a host with readable
OVMF firmware, `/dev/kvm`, `/dev/vhost-vsock`, `/dev/net/tun`,
`systemd-nspawn` privileges, and a QEMU bridge ACL that allows the selected
bridge:

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
qemu-system-x86_64 --version
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

Check UEFI firmware configuration for direct QEMU runs:

```sh
test -n "${KATL_OVMF_CODE:-}" && test -r "$KATL_OVMF_CODE"
test -n "${KATL_OVMF_VARS:-}" && test -r "$KATL_OVMF_VARS"
```

If those variables are unset, `scripts/katl-vm` tries common distribution
firmware locations. Set `KATL_OVMF_CODE` and `KATL_OVMF_VARS` explicitly when
the host keeps OVMF/edk2 firmware somewhere else.

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

At that time, `qemu:///system` worked from this shell and `libvirtd` was active.
`/dev/kvm` and `/dev/net/tun` were not visible from the Codex sandbox even
though the host reported hardware virtualization support and the `kvm_intel`,
`kvm`, and `tun` modules were loaded.
