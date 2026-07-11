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
host capabilities, records `world.json`, `host-capabilities.json`, `run.json`,
and `go-test.log`, exports the world environment, and then executes `go test`
with the caller's package patterns and flags. Argument meaning belongs to
`go test`; `scripts/vmtest-run` only adds the harness execution needed to route
compiled package test binaries through `scripts/vmtest-exec`.

The runner keeps terminal output minimal: it prints the world paths, writes the
full `go test` stream to `go-test.log`, and reports a concise final outcome.
Scenario artifacts remain under the world directory for later inspection or
aggregation by another tool. Pass `--no-rebuild` before the `go test` arguments
to skip the runner's automatic `scripts/mkosi` artifact builds and use only
already-discoverable artifacts.

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

## GitHub Fast Checks

The low-cost pull-request workflow runs formatting, whitespace, unit/golden, and
delivery fixture checks through the same command used locally. Before pushing a
branch, run:

```sh
scripts/check-fast origin/main...HEAD
```

The command checks all tracked Go formatting, patch whitespace, and runs the
complete Go suite with `-count=1` so cached test results cannot hide failures.
Changes reach `main` through pull requests after the `Format And Unit Tests`
check passes; direct pushes are not part of the supported development loop.

It intentionally skips mkosi builds, libvirt/KVM setup, VM scenarios, and
publishing. Those host-specific gates belong to the capable-host vmtest workflow
and release gates.

## GitHub Release Artifacts

`.github/workflows/release-artifacts.yml` builds Katl artifacts for pushes to
`release/**` branches and pushed `v*` KatlOS tags. It can also be dispatched manually
with an explicit version for build verification. Release-branch and manual runs
retain one GitHub Actions artifact; tag runs additionally publish those exact
files as assets on the matching GitHub Release.

KatlOS release versions use `YYYY.M.PATCH` calendar versions with `-dev.N` and
`-rc.N` prereleases. The first development and release-candidate identities for
the July 2026 line are `2026.7.0-dev.0` and `2026.7.0-rc.0`; the stable identity
is `2026.7.0`. Use tags such as `v2026.7.0-rc.0` and release branches such as
`release/2026.7.0-rc.0`. The workflow strips an optional `v` before embedding
the version and rejects noncanonical versions. See
`docs/internal/adrs/adr-009-katlos-calendar-versioning.md` for the policy.

The published set contains the KatlOS install and upgrade SquashFS images and
the installer UKI, kernel, initrd, and UEFI-bootable ISO variants, each with
adjacent JSON metadata and SHA-256 files. The ISO embeds the matching KatlOS
install image and its metadata, so it is the primary self-contained install
artifact. It remains generic because node identity, disk selection, network,
and bootstrap configuration are supplied separately at boot. The loose
installer and install-image artifacts remain available for PXE deployments.
Loose runtime root/UKI intermediates and Kubernetes payload bundles are not
published through this workflow. Kubernetes bundles have a separate producer
contract. Tag publications create keyless SLSA build-provenance attestations
for every release asset using the `katl-dev/katl` workflow identity. The
workflow verifies each attestation against its exact signer workflow, tag ref,
and source commit after publishing. `SHA256SUMS` covers the staged asset set and
`PROVENANCE.md` documents user verification and the remaining trust boundary.
The staged `RELEASE_NOTES.md` is generated from commits since the previous
canonical KatlOS CalVer tag. It links each commit, includes the full comparison,
and becomes the GitHub release body; Kubernetes bundle tags are deliberately
excluded when selecting the previous KatlOS release.
This provenance does not provide Secure Boot signatures or implement Katl's
future node-side trust-root, revocation, and downgrade policy.

## Kubernetes Bundle Artifacts

`.github/workflows/kubernetes-bundles.yml` is the independent Kubernetes
payload producer. The normal release path is a reviewed Renovate pull request.
Renovate tracks the selected Kubernetes `v1.36` patch and the exact kubeadm,
kubelet, kubectl, and cri-tools RPM NEVRAs in
`mkosi.profiles/kubernetes-sysext/kubernetes.env`. Merging that lock update to
`main` builds, verifies, publishes, and attests the corresponding immutable
`vMAJOR.MINOR.PATCH-katl.1` GHCR artifact. Earlier patch releases and digests
remain addressable.

Manual dispatch remains the explicit rebuild and dry-run path. Dispatch it with
an exact Kubernetes payload such as
`v1.36.0`, an immutable Katl build identity such as `v1.36.0-katl.1`, and
`publish: false` for a build-only verification run. A successful build uploads
the complete staged bundle as a GitHub Actions artifact.

Set `publish: true` only for a reviewed bundle identity. The workflow refuses
to replace either immutable GHCR tag, publishes the Katl custom bundle manifest
as the OCI config with the sysext and metadata as layers, pulls the config back
for byte verification, and creates a GitHub build-provenance attestation. The
canonical package is `ghcr.io/katl-dev/kubernetes`. Its readable tags use the
bundle build identity directly, for example `v1.36.0-katl.1`, while a
second `sha256-<bundle-manifest-digest>` tag supports exact Katl resolution.
The OCI manifest carries the standard source, description, and MIT license
annotations that GHCR renders on the package page, plus title, documentation,
revision, and version metadata for other OCI clients.

GitHub creates a new container package as private. After the first publication,
an organization owner must make the `kubernetes` package public in its package
settings so uncredentialed KatlOS nodes can fetch it. This is a one-time GHCR
namespace operation. The workflow summary prints the readable and digest-pinned
OCI bundle references for install/bootstrap manifests. Published development
bundles remain unsigned until the signing policy lands; the GitHub attestation
records build provenance but is not yet a trust decision enforced by `katlc`.

## GitHub VM Tests

The heavy pull-request workflow is `.github/workflows/vmtest.yml`. It runs for
manual dispatches and, when the repository variable
`KATL_VMTEST_PR_ENABLED=1` is set, same-repository non-draft pull requests. It
uses self-hosted runners labeled `katl-vmtest`, `libvirt`, `kvm`, `ovmf`, and
`vsock`. The runner must provide the same capable-host tools and environment as
the local VM shell, including `nix develop .#vm`, readable `KATL_OVMF_CODE`, and
readable `KATL_OVMF_VARS`.

The workflow runs a serialized matrix so fail-fast behavior does not leave many
VMs active at once:

```sh
scripts/vmtest-run --artifact-set=runtime ./internal/vmtest \
  -run '^TestDirectRuntimeVMTestAgentSmoke$' \
  -count=1 -failfast -timeout 20m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestFirstInstallTargetDiskSerialSmoke$' \
  -count=1 -failfast -timeout 35m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^(TestInstalledRuntimeKubeadmReadySmoke|TestInstalledRuntimeConfigApplyModesSmoke)$' \
  -count=1 -failfast -timeout 45m
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^TestInstalledRuntimeTwoNodeKubeadmJoinSmoke$' \
  -count=1 -failfast -timeout 60m
```

CI sets `KATL_VMTEST_KEEP=always` only long enough to upload the run directory
as a workflow artifact, excluding large VM disk images, then removes the local
run directory. Live-domain debug preservation is disabled in CI; local debugging
should use `--debug-on-failure` with a single narrow scenario and clean retained
domains with `scripts/vmtest-clean` when inspection is done.

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
