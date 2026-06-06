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

## Optional During The Current Loop

- `libvirt` and `virsh`: useful for later persistent VM definitions, networks,
  multi-node tests, and longer integration tests.
- `virt-install`: useful for manual libvirt VM creation.
- `virt-manager`: useful GUI for inspecting and debugging local VMs.
- `/dev/net/tun` and `vhost_net`: useful once tests need richer VM networking.

## Installed Runtime VM Fixture

The opt-in installed-runtime vmtest-agent smoke expects a real installed runtime
disk, a rendered ESP artifact tree, and a fixture manifest that binds those
artifacts by checksum. Package completed install-to-runtime outputs into that
fixture contract and generate a sourceable smoke-test environment with:

```sh
scripts/create-installed-runtime-fixture \
  --disk build/local/cp-1.qcow2 \
  --esp-artifacts build/local/cp-1-esp \
  --node-metadata build/local/cp-1-node.json \
  --format qcow2
```

The command copies the disk, ESP tree, and optional node metadata under
`build/installed-runtime-fixture/`, writes an
`InstalledRuntimeVMTestFixture` manifest with disk and ESP checksums, preflights
the ESP loader entries through `scripts/check-installed-disk-smoke
--preflight-only`, and writes generated files under the same directory. Source
the generated `vmtest.env` or run the generated wrappers to execute
`TestInstalledRuntimeVMTestAgentSmoke` and
`TestInstalledRuntimeKubeadmReadySmoke`. Use `--artifact-mode reference` when
the fixture should bind existing paths instead of copying a large local disk.

If a checksum-bound fixture manifest already exists, validate and resolve it
directly with:

```sh
scripts/resolve-installed-runtime-fixture \
  --disk build/local/cp-1.qcow2 \
  --esp-artifacts build/local/cp-1-esp \
  --fixture build/local/cp-1-fixture.json \
  --format qcow2
```

Neither command manufactures placeholder disks; the disk and ESP tree should
come from the real install-to-runtime flow.

The generated kubeadm-ready wrapper boots the installed runtime with the vmtest
agent and checks the local handoff boundary: `katl-kubeadm-ready.target`,
`/etc/kubernetes` projection, kubeadm tooling, and container runtime readiness.
It does not run `kubeadm init` or the API-server smoke.

To run the opt-in first-install target-disk fixture contract smoke, resolve
the installer UKI, KatlOS install image, optional node metadata, and install
manifest into a sourceable environment with:

```sh
scripts/resolve-first-install-katlos-image-fixture \
  --installer-uki build/mkosi/katl-installer.efi \
  --katlos-image build/mkosi/katlos-install-0.0.0-dev-x86_64.squashfs \
  --node-metadata build/local/cp-1-node.json \
  --install-manifest docs/internal/examples/minimal-install-manifest.json
```

The command verifies the KatlOS image contract and manifest image binding,
extracts the runtime-root component for the host-side VM harness, records
SHA-256 bindings, and writes generated files under
`build/first-install-katlos-image-fixture/`. The generated smoke runs in
installed-ESP mode: it extracts the ESP from the target disk written by the
installer and uses that tree for the packaged runtime boot. Source the generated
`vmtest.env` or run the generated wrapper to revalidate inputs and execute
`TestFirstInstallTargetDiskFixtureContract`.

If you already have a loose runtime root and rendered runtime ESP tree from a
lower-level development loop, `scripts/resolve-first-install-runtime-fixture`
can still resolve those inputs directly.

The smoke keeps the target disk from the first-install harness, packages it with
`scripts/create-installed-runtime-fixture`, includes node metadata when
`KATL_RUNTIME_NODE_METADATA` or `KATL_INSTALLED_NODE_METADATA` is set,
revalidates the generated fixture manifest, then boots the packaged fixture with
`RequireVMTestAgent=true` and records the vsock health transcript. The current
harness still owns whether the target disk was actually written by the in-guest
installer; this smoke fails if that path does not leave a bootable installed
runtime disk.

After the generated first-install wrapper passes, it publishes a packaged copy of
the installed runtime under `<state-dir>/published-installed-runtime/`. Source
that directory's `vmtest.env` to run the direct installed-runtime smokes, or use
its `published-first-install-runtime-fixture.json` manifest as an input to the
two-node and three-control-plane fixture resolvers. The published directory keeps
the installed disk, disk format, ESP tree, fixture manifest, and optional
`node.json` together so later VM smokes do not depend on the source run layout.

## Two-Node Kubeadm VM Fixtures

The opt-in two-node kubeadm join smoke expects two already installed runtime
disks, rendered per-node ESP artifact trees, per-node `/etc/katl/node.json`
metadata files, per-node fixture manifests that bind those artifacts by checksum,
and bridge-reachable addresses for the guests. The control-plane disk must be
installed from `cp-1` materials, and the worker disk must be installed from
`worker-1` materials.

If each node came from a published first-install fixture directory, resolve the
two-node inputs with:

```sh
scripts/resolve-two-node-kubeadm-fixtures \
  --control-plane-published-dir build/first-install-cp-1/published-installed-runtime \
  --worker-published-dir build/first-install-worker-1/published-installed-runtime \
  --control-plane-address 10.88.0.11 \
  --worker-address 10.88.0.12 \
  --bridge katlbr0
```

To resolve loose local inputs instead, pass the disk, ESP, metadata, and fixture
manifest paths directly:

```sh
scripts/resolve-two-node-kubeadm-fixtures \
  --control-plane-disk build/local/cp-1.qcow2 \
  --worker-disk build/local/worker-1.qcow2 \
  --control-plane-esp build/local/cp-1-esp \
  --worker-esp build/local/worker-1-esp \
  --control-plane-metadata build/local/cp-1-node.json \
  --worker-metadata build/local/worker-1-node.json \
  --control-plane-fixture build/local/cp-1-fixture.json \
  --worker-fixture build/local/worker-1-fixture.json \
  --control-plane-address 10.88.0.11 \
  --worker-address 10.88.0.12 \
  --control-plane-format qcow2 \
  --worker-format qcow2 \
  --bridge katlbr0
```

The command preflights each disk against its ESP tree, verifies the node metadata
matches `cp-1` and `worker-1`, rejects shared disk, ESP, metadata, or address
inputs, checks fixture manifest checksums, checks that the bridge exists with an
IPv4 subnet containing both node addresses, and writes generated files under
`build/two-node-kubeadm-fixtures/`. Source the generated `vmtest.env` or run the
generated wrapper to revalidate the fixture inputs and execute
`TestInstalledRuntimeTwoNodeKubeadmJoinSmoke`.

Each fixture manifest binds one installed node's artifacts:

```json
{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "InstalledRuntimeVMTestFixture",
  "nodeName": "cp-1",
  "systemRole": "control-plane",
  "disk": {"path": "cp-1.qcow2", "format": "qcow2", "sha256": "..."},
  "espArtifacts": {"path": "cp-1-esp", "treeSHA256": "..."},
  "nodeMetadata": {"path": "cp-1-node.json", "sha256": "..."}
}
```

## Three-Control-Plane Stacked-Etcd VM Smoke

The opt-in three-control-plane stacked-etcd smoke expects three already
installed control-plane runtime disks, rendered per-node ESP artifact trees,
per-node `/etc/katl/node.json` metadata files, per-node fixture manifests that
bind those artifacts by checksum, and bridge-reachable addresses. It uses the
operator bootstrap path to run `kubeadm init` on `cp-1`, join `cp-2` and `cp-3`
serially, verify the resulting node objects, verify stacked-etcd health and
membership, and create a restricted etcd snapshot artifact.

Resolve those local inputs into a sourceable environment with:

```sh
scripts/resolve-three-control-plane-kubeadm-fixtures \
  --cp1-published-dir build/first-install-cp-1/published-installed-runtime \
  --cp2-published-dir build/first-install-cp-2/published-installed-runtime \
  --cp3-published-dir build/first-install-cp-3/published-installed-runtime \
  --cp1-address 10.88.0.11 \
  --cp2-address 10.88.0.12 \
  --cp3-address 10.88.0.13 \
  --bridge katlbr0
```

To resolve loose local inputs instead, pass each node's disk, ESP, metadata, and
fixture manifest paths directly:

```sh
scripts/resolve-three-control-plane-kubeadm-fixtures \
  --cp1-disk build/local/cp-1.qcow2 \
  --cp2-disk build/local/cp-2.qcow2 \
  --cp3-disk build/local/cp-3.qcow2 \
  --cp1-esp build/local/cp-1-esp \
  --cp2-esp build/local/cp-2-esp \
  --cp3-esp build/local/cp-3-esp \
  --cp1-metadata build/local/cp-1-node.json \
  --cp2-metadata build/local/cp-2-node.json \
  --cp3-metadata build/local/cp-3-node.json \
  --cp1-fixture build/local/cp-1-fixture.json \
  --cp2-fixture build/local/cp-2-fixture.json \
  --cp3-fixture build/local/cp-3-fixture.json \
  --cp1-address 10.88.0.11 \
  --cp2-address 10.88.0.12 \
  --cp3-address 10.88.0.13 \
  --cp1-format qcow2 \
  --cp2-format qcow2 \
  --cp3-format qcow2 \
  --bridge katlbr0
```

The command preflights each disk against its ESP tree, verifies each node
metadata file matches `cp-1`, `cp-2`, or `cp-3`, rejects shared disk, metadata,
fixture, or address inputs, checks fixture manifest checksums, checks that the
bridge exists with an IPv4 subnet containing all three node addresses, and
writes generated files under `build/three-control-plane-kubeadm-fixtures/`.
Source the generated `vmtest.env` or run the generated wrapper to revalidate the
fixture inputs and execute `TestInstalledRuntimeThreeControlPlaneStackedEtcdSmoke`.

To run the smoke directly after sourcing the generated env:

```sh
GOCACHE="${GOCACHE:-/tmp/katl-go-cache}" go test ./cmd/katlctl \
  -run '^TestInstalledRuntimeThreeControlPlaneStackedEtcdSmoke$' \
  -count=1 -timeout 60m -katl.vmtest.run
```

If all three nodes share one ESP artifact tree, set
`KATL_INSTALLED_ESP_ARTIFACTS` instead of the per-node ESP variables. The
legacy `KATL_CONTROL_PLANE_INSTALLED_DISK`,
`KATL_CONTROL_PLANE_INSTALLED_ESP_ARTIFACTS`,
`KATL_CONTROL_PLANE_NODE_METADATA`, `KATL_CONTROL_PLANE_FIXTURE_MANIFEST`,
`KATL_CONTROL_PLANE_INSTALLED_DISK_FORMAT`, and `KATL_CONTROL_PLANE_ADDRESS`
variables may be used for `cp-1` only.

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
- `bd 1.0.3`

At that time, `qemu:///system` worked from this shell and `libvirtd` was active.
`/dev/kvm` and `/dev/net/tun` were not visible from the Codex sandbox even
though the host reported hardware virtualization support and the `kvm_intel`,
`kvm`, and `tun` modules were loaded.
