# Developing Katl

This document describes the supported local build, persistent development VM,
automated VM-test, and release-tooling workflows. Product behavior belongs in
the public [KatlOS documentation](README.md); this page is for contributors.

## Find project documentation

- [CI and validation](development/ci.md) explains which jobs exercise a change.
- [Generation ownership](development/generation-ownership.md) explains the
  boundary between offline installation, boot health, and node operations.
- [Release KatlOS](development/releases.md) covers preparation, qualification,
  and the release guidance in `docs/releases`.
- [Publish Kubernetes bundles](development/kubernetes-releases.md) explains
  how the producer selects, publishes, and verifies each version.
- [Base service security](development/service-security.md) identifies the policy
  owners and explains how to validate a change.
- [Current product direction](internal/north-star.md) and
  [architecture decisions](internal/adrs/) explain why Katl works as it does.
  Design records in `docs/internal` and lab working material in `docs/private`
  serve contributors.

For operator documentation, use [the docs index](README.md) to place a guide
where readers first need it. Give each page a goal or question and connect a
procedure to the next task. Keep examples and links relative to their
Markdown source. Put prerequisites and destructive effects before the steps
that depend on them. Use
[SimpleEnglish Plain mode](https://github.com/AminBlg/SimpleEnglish) for the
README, docs index, and first-cluster guide.

## Build release-owned extensions

Local builds and CI use the same commands and verification. CI discovers
`extensions/*/recipe.json`, builds each recipe for the standard and LTS runtime
targets, and transfers the resulting immutable closures to image assembly.
The workflow does not list individual drivers.

Use the container builder required by `scripts/mkosi`. The host also needs Go,
`sudo`, `systemd-dissect`, `depmod`, and SquashFS tools. Image inspection uses
`sudo` for read-only mounts and module-index composition; it does not load
drivers into the host.

Run the same build stages as CI for one target:

```bash
export KATL_FLAVOUR=lts
export KATL_VERSION=2026.9.0-dev.16
export KATL_BUILD_COMMIT=local-extension-test
export KATL_MKOSI_BUILD_DIR="$PWD/_build/release-lts"
export KATL_VMTEST_IMAGE_SUPPORT=0

scripts/mkosi build-runtime
scripts/build-release-extensions
scripts/assemble-release-extensions
```

Use `KATL_FLAVOUR=standard` and a separate build directory to test the other
target. Keep the release version and build ID distinct to exercise their
separate metadata contracts.

`scripts/build-release-extensions` builds every discovered recipe. To reproduce
one CI matrix entry, pass its recipe name, for example
`scripts/build-release-extensions drbd9`. Both forms use the same builder,
source checksums, runtime-owned kernel inputs, and module-composition verifier.

The runtime build exports `katl-kernel-inputs.tar` and its JSON metadata beside
the root image and UKI. The archive contains the prepared kernel tree, including
headers, configuration, and symbol-version information. Its metadata binds it
to the runtime root digest. CI transfers these files with the runtime; local
builds consume the same files from the build directory.

Extension jobs do not resolve or install a kernel development package. Missing,
corrupt, or mismatched inputs fail the build with no package-lookup fallback.
Rebuild the runtime to regenerate them. Build packages and exported headers
are excluded from the shipped runtime image.

Assembly requires an output for every inventoried recipe, verifies that all
outputs target the same runtime, checks their combined module composition, and
writes `$KATL_MKOSI_BUILD_DIR/release-extensions.json`. Missing, corrupt, or
incompatible outputs fail the command. To embed the verified closures, set
`KATL_EXTENSION_RELEASE` to that manifest when running
`scripts/build-katlos-install-image`. These commands do not publish artifacts.

### Add a release-owned recipe

Add `extensions/NAME/recipe.json` with a payload `version`, a full OCI
`repository`, and a `sources` map. Each source must declare an HTTPS `url`
and a pinned `sha256`. Add the matching
`mkosi.profiles/kernel-extension-NAME` profile and its build hook. Keep the
extension-specific compiler and payload layout in
`internal/releaseextensions/NAME`; the shared command handles source integrity,
runtime binding, and the build record.

The common builder acquires the pinned sources, supplies the runtime base
and its prepared kernel tree, and invokes the profile.
The hook invokes that compiler. It must produce `katl-NAME.raw`
and `katl-NAME.build.json`, containing the recipe, kernel contract, and prepared
input digests. The common builder validates and packages these outputs.
No workflow change is required to add a recipe.

Inspect the inventory with:

```bash
go run ./cmd/katl-mkosi-artifacts inventory-release-extensions
```

VM lifecycle qualification runs through the same libvirt harness locally and
in [GitHub Actions](development/ci.md).

See [base service security](development/service-security.md) before changing
service privileges or applying systemd sandbox restrictions.

## Run VM tests with libvirt

Use the libvirt-backed vmtest world as the supported automated VM layer:

1. Build current artifacts with `scripts/mkosi` or let `scripts/vmtest-run`
   build the current repo artifacts by default.
2. Run VM smokes with `scripts/vmtest-run` so world setup, host capability
   recording, libvirt lifecycle, and serial capture are handled consistently.
3. Use libvirt tools such as `virsh` for manual inspection of domains,
   networks, storage pools, and serial consoles.

`virt-manager` is useful for interactive debugging, but it is not a project
dependency and should not be required by automated tests.

## Local artifacts and boot contract

Local builds produce the same release-shaped installer, KatlOS, and upgrade
artifacts exercised by the public user journeys. Automated coverage must use
the libvirt-backed runner so capability evidence, disk mutation boundaries,
serial output, lifecycle, and cleanup are consistent.

- Build tool: `mkosi 26`, run inside the project's containerized builder.
- Base distribution: Fedora, chosen for current systemd and mkosi support.
- Output format: a bootable `disk` image with systemd-boot/UKI support.
- Output directory: `_build/mkosi/`.
- Primary artifact name: `katl-installer.raw`.
- Build command: `scripts/mkosi build-installer`.
- Required smoke path: `scripts/vmtest-run` with libvirt system VM execution
  and deterministic serial capture.
- Firmware expectation: the runner is given readable OVMF/edk2 pflash images.
- Console settings: the guest kernel command line includes both
  `console=ttyS0,115200n8` and `console=tty3`. `tty3` receives kernel and
  direct console logs, separate from the dashboard on `tty1` and login on
  `tty2`. The installer journal is also mirrored to `ttyS0` for the runner's
  deterministic captured console log.
- Installer graphics: native Intel (`i915`, `xe`) and AMD (`amdgpu`) drivers
  and their firmware are included, along with common server and virtual GPU
  drivers. Kernel mode setting lets the console use the display's native
  resolution; the dashboard follows terminal size changes automatically.
  Intel firmware references are checked individually. AMD uses Fedora's
  packaged firmware set: its module metadata also lists optional sideloaded
  files, so requiring every declared filename would reject usable images.
- Dashboard colors use [Catppuccin Mocha](https://catppuccin.com/palette/).
  The native Linux console has 16 programmable RGB slots, rather than
  per-cell true color. The dashboard programs its own VT's palette with
  exact theme colors and resets it on exit; login and log VTs retain their
  own palettes. Plain-text snapshots contain no palette or color escapes.
- Vertical dashboard rules begin at the bottom of their parent horizontal
  rule, or at the pane's top when there is no parent rule, and run to the next
  horizontal boundary. They do not extend into the parent title row. The
  Node/Kubernetes divider joins their underline to the Journal underline;
  full-width alerts remain unobstructed.
- Network continuation rows align with the field value column. VRF annotations
  reflect current kernel master relationships, including bond and bridge
  masters, rather than configured intent. Installed nodes awaiting cluster
  bootstrap show a pending state and a workstation action even when OS health
  checks have passed.
- Generated build artifacts belong under `_build/`; VM-test worlds use the
  temporary path reported by `scripts/vmtest-run`.

The libvirt-backed vmtest path is the automation contract because it gives the
smoke harness stable process control, serial output, timeout handling, network
leases, storage setup, and exit details. KVM should be used when available, and
the runner records missing `/dev/kvm` access as a host capability gap.

Installer and lifecycle scenarios deliberately mutate only runner-created VM
disks. Test agents and fixtures are allowed only in instrumented test artifacts
and are rejected from production release images.

Console validation must inspect the Linux virtual-terminal screen, not just
the dashboard's saved text snapshot. `console=tty3` routes `/dev/console`;
the dashboard also uses `TIOCL_SETKMSGREDIRECT` to keep kernel messages off
the foreground terminal. `TestKernelLogRouting` exercises this against a real
kernel: compile `cmd/katl-console` tests with `CGO_ENABLED=0 go test -c`, copy
the test binary into a throwaway VM, stop `katl-console`, select `tty1`, and
run `KATL_TEST_KERNEL_LOG_ROUTING=1 ./katl-console.test -test.run=TestKernelLogRouting`.
Restart the dashboard afterwards. Include framebuffer resizing, log traffic,
terminal switching, and reboot in the persistent VM journey.

## Required development tools

- `scripts/mkosi`: builds installer, runtime, and KatlOS
  image artifacts through the containerized mkosi builder.
- `scripts/build-kubernetes-sysext`: explicitly builds one Kubernetes sysext
  from the selected release manifest and package versions. VM upgrade fixtures
  call the same builder with explicit version and output inputs.
- `scripts/vmtest-run`: runs enabled VM, first-install, installed-runtime, and
  multinode kubeadm smokes through a runner-created world.
- `libvirt`/`virsh`: defines, starts, observes, and tears down local VM test
  domains.
- KVM access: the VM test runner should see `/dev/kvm`.
- UEFI firmware for libvirt VM pflash boot, such as edk2/OVMF images.
- `git commit-wrapped`: provided by the development shell (`scripts/git-commit-wrapped`); takes the title as its argument and the body on standard input.

The supported top-level script surface is intentionally small. Use
`scripts/mkosi` for KatlOS image artifacts,
`scripts/build-kubernetes-sysext` for Kubernetes payload artifacts, and
`scripts/vmtest-run` for enabled test worlds. Other scripts are compatibility
wrappers, debug aids, or temporary validators for scaffolding work; do not
treat the whole `scripts/` directory as the public developer interface.

VM gates build and consume artifacts under the checkout's `_build/mkosi`.
Unset `KATL_MKOSI_BUILD_DIR` when switching from a separate development or release
build directory to `scripts/vmtest-run`. The harness rejects alternate output
directories and explicit artifact-path overrides before building, so its
builds, recorded provenance, and VM consumers cannot select different roots.

## Nix dev shell

On NixOS or any host with flakes enabled, enter the default development shell:

```sh
nix develop
```

The repository also includes `.envrc`:

```sh
direnv allow
```

Use `direnv` plus `nix-direnv` if you want the flake shell loaded
automatically when entering the repository.

That shell provides the Go and protobuf toolchain, Git, OpenSSH, `jq`, `curl`,
Podman, libvirt clients, QEMU image tooling, OVMF firmware, and the filesystem
and artifact-inspection tools used by VM tests. Image construction stays behind
`scripts/mkosi`; mkosi and Fedora package installation run inside its builder
container.

`katlctl` is also available in the development shell. It runs from the current
checkout so local source changes are used immediately:

```sh
nix develop --command katlctl --help
```

The flake also builds a standalone `katlctl` package from its pinned source for
`x86_64-linux` and `aarch64-darwin`. A downstream flake can pin a Katl release
tag and include `katl.packages.${system}.katlctl` in its shell packages instead
of downloading a CLI binary separately. Replace `RELEASE_VERSION` with a KatlOS
release that includes these flake outputs:

```nix
inputs.katl.url = "github:katl-dev/katl/vRELEASE_VERSION";
# In the system-specific dev shell:
packages = [ katl.packages.${system}.katlctl ];
```

The source-built CLI reports the pinned Git commit in `katlctl version`. The
development-shell command above continues to run the checkout source directly.

`katldev` owns persistent developer workflows. It keeps one persistent VM
world per checkout: a set of installer VMs on a libvirt NAT network that the
world creates for itself, with one shared `_build/katldev/cluster.yaml`. To boot
the current checkout's installer ISO in a one-node world:

```sh
katldev installer start
```

The command cache-checks and builds the self-contained installer ISO, waits for
the installer API, and creates `_build/katldev/cluster.yaml` from the waiting
VM's reserved address and target disk. Its output leads directly to
`katlctl install apply`. A config that lists any of the world's VMs is retained
so it can be edited for repeated user journeys. If it lists none of them,
katldev fails with recovery guidance; pass `--force-config` only when replacing
it is intentional. The world's VMs, 32 GiB target disks, and network state live
under `_build/katldev/world`; they are not owned or cleaned up by automated
vmtest runs.

To exercise the automatic install paths, start or add a VM with boot inputs.
They apply to the VMs that command boots, so use `world add-node` to give each
node different inputs:

| Flag | Effect |
| --- | --- |
| `--preseed-dir DIR` | Attach `DIR` as `KATLSEED` media, such as an `install-input.json` with its files. |
| `--manifest FILE` | Build `KATLSEED` media that installs automatically from an install manifest, including the file its `katlosImage.localRef` names, resolved beside the manifest. |
| `--cmdline ARG` | Append `ARG` to the installer kernel command line; repeatable. `{gateway}` expands to the host address on the world network. |

`--preseed-dir` and `--manifest` are mutually exclusive. Command-line arguments
reach the installer UKI through SMBIOS (`io.systemd.stub.kernel-cmdline-extra`),
so they also apply when the installed system boots from the same VM.

To install from a bundle served by the host, serve a directory on the world
network and point the installer at it:

```sh
katldev installer start \
  --cmdline 'katl.bundle.url=http://{gateway}:8080/cluster.katlcfg' \
  --cmdline katl.node=cp-1 --cmdline katl.install.mode=auto
katldev world serve ./bundles    # leave running; Ctrl-C stops it
```

`katldev world serve` listens on the world network's host address, which exists
once the world has started. Automatic installs do not wait for configuration, so
use `katldev world console` to follow them.

The world's `katlctl` contexts live in `_build/katldev/katlctl.yaml`, so a dev
journey's `katlctl context save` cannot change or replace the contexts for
your own clusters. The development shell, entered with `nix develop` or
direnv, exports `KATLCTL_CONFIG` with that path and clears any other inherited
`KATLCTL_*` settings. Inside the shell, `katlctl` therefore manages only the
dev world. Your own config stays in effect in every other shell. katldev
prints the world's config path, and from a shell outside the development
shell it also prints the `export KATLCTL_CONFIG=...` line that selects it. To
manage a real cluster from inside the development shell, set `KATLCTL_CONFIG`
to your own config for that command.

VM tests are isolated in the same way. `scripts/vmtest-run` points
`KATLCTL_CONFIG` at the run directory, and every test runs `katlctl` through
`vmtest.Katlctl.Command`, which gives the command a config, `HOME`, and
`XDG_CONFIG_HOME` inside the test's own directory. A unit test rejects VM test
code that builds, runs, or configures `katlctl` any other way.

`katldev installer` is the one-node form of `katldev world`. Start a multi-node
world, or add VMs to an existing one, with:

```sh
katldev world start --nodes 2     # cp-1 and worker-1
katldev world add-node --control-plane   # cp-2
katldev world add-node            # the next worker-N
```

A new world's config lists every VM that was waiting when it was created. A VM
added later is not written into `cluster.yaml`; katldev prints the
`katlctl config add-node` command for it, with the VM's installer address, name,
and role. That command prints the `katlctl install apply` and `katlctl node
join` steps from
[Add one node](operations/change-cluster-nodes.md#add-one-node). This keeps
multi-node journeys on the operator's path.

Use the same world for repeated manual install and management checks:

```sh
katldev world status
katldev world console cp-1
katldev world stop                # or: stop worker-1
katldev world start
katldev world reset
katldev world remove
```

`start` resumes stopped VMs and recreates a missing VM with a fresh disk.
`status` reports each VM's libvirt state, observed address, installer or KatlOS
management state, and the `cluster.yaml` node at that address. Both warn when
the host's `libvirt-guests.service` is disabled or suspends guests at host
shutdown. A KatlOS node resumed from suspend finds its clock past DHCP leases
and certificate checks, so set `ON_SHUTDOWN=shutdown`.

`stop` simulates pressing the power button through ACPI and waits for each
guest to power off. On an installed Kubernetes node, this lets kubelet use its
[configured graceful shutdown budgets](operations/maintain-node.md#configure-native-graceful-shutdown).
Katl does not drain the node or enable those budgets automatically. Use
`--timeout 5m` when a guest needs more than the default three minutes.
If shutdown is not confirmed, inspect `status` and `console`; the guest might
still be shutting down. `stop --force` cuts power immediately and bypasses
guest shutdown.

Each VM has a DHCP reservation in the world network, with an infinite lease.
To test [worker re-addressing](operations/change-cluster-nodes.md#re-address-a-worker),
move the reservation:

```sh
katldev world readdress worker-1               # first free address
katldev world readdress worker-1 --address 10.200.4.40
```

The VM keeps its current address until it next requests one, which normally
happens when it boots. That address stays reserved for the VM until `status`
or another katldev command sees it answer at the new one, so a VM added in the
meantime never receives it. katldev prints the `cluster.yaml` change and the
`katlctl cluster apply --endpoint` and `katlctl node reboot --endpoint`
commands that complete the re-address.

To test a node behind NAT, whose `management.address` no VM holds, give it a
host-translated address outside the world network:

```sh
katldev world nat cp-1 --address 198.18.0.10
katldev world nat cp-1 --remove
```

The host then translates connections from this machine to that address to the
VM's reserved address, and katldev prints the `management.address` and
`kubernetes.address` to set in `cluster.yaml`. The rule is an iptables DNAT in
the world's own nat-table chain, so `nat` needs `sudo` that does not prompt.
`start`, `add-node`, and `readdress` rewrite it after a readdress or a host
firewall reload, and warn with the command to rerun if they cannot; `status`
does not touch it. `reset` and `remove` delete it, but
keep the record and say what to rerun if sudo fails.

To test a multihomed node, whose `management.address` is on a second interface
that the node holds, give its VM a second NIC:

```sh
katldev world multihome cp-1
katldev world multihome cp-1 --remove
```

The second NIC joins an isolated libvirt network that the world owns on a free
subnet; the host reaches the VM there, and the VM's primary network keeps the
default route. katldev prints the `management.address` for the second NIC and
the `kubernetes.address` for the primary one to set in `cluster.yaml`. A running
VM gets the NIC live, `reset` drops it with the VM, and `remove` deletes the
network.

`reset` first prepares the current checkout's ISO, then removes the world and
boots fresh installers, keeping the VM count unless `--nodes` changes it.
`remove` deletes the world without recreating it. Both remove only libvirt
domains and the network carrying this checkout's world ownership metadata,
plus the world's disks and state; `cluster.yaml` is kept. They need no
destructive confirmation because they can only address the checkout-scoped
development world. The world's identity is a random ID in
`_build/katldev/world-id`, which both keep, so a world is usually recreated on
the same subnet with the same addresses and a retained `cluster.yaml` keeps
matching. If `start` cannot start the world network because another network,
VPN, or route now uses its subnet, `reset` chooses a free one. Pass `--no-build`
only when deliberately reusing the existing `_build/mkosi/katl-installer.iso`.

A checkout that still has the single VM from an earlier katldev refuses
`start`; run `katldev world reset` to remove that VM and boot a world.

Build and verify the current checkout's self-contained installer ISO through
the discoverable development command:

```sh
nix develop --command katldev build iso
```

The command composes the existing source-fingerprinted `scripts/mkosi`
pipeline, runs the full ISO verifier, and prints absolute paths for the ISO,
metadata, and checksum together with the digest and byte size. By default it
derives a checkout-scoped identity such as `2026.7.0-local.1a2b3c4` from the
nearest KatlOS release line and the current commit. Use `--version` when the
installed development image needs a particular KatlOS identity:

```sh
nix develop --command katldev build iso --version 2026.7.0-dev.12
```

The default command omits release-owned system extensions to keep the routine
development build focused. To build a release-shaped ISO, build and include
every discovered release-owned extension:

```sh
nix develop --command katldev build iso --release-extensions
```

This option builds the runtime, builds and verifies each extension recipe,
assembles their aggregate manifest, and embeds the verified closure in the
KatlOS install image. The command fails if the final image metadata does not
contain that exact manifest.

Copy the reported ISO to a hypervisor under a versioned name and independently
compare the reported digest after transfer. Published images should continue
through `katlctl node upgrade` where possible; use the ISO path for clean
installation, wipe/reinstall recovery, and current-checkout changes which do
not yet have a published upgrade source.

Build the matching local upgrade image without repeating the derived version:

```sh
nix develop --command katldev build upgrade
```

Use `--release-extensions` when the upgrade must carry the release-owned
extensions for its target runtime:

```sh
nix develop --command katldev build upgrade --release-extensions
```

### Container runtime packages

`scripts/mkosi build-runtime` builds containerd and crun RPMs before assembling
the runtime. The image installs the exact Katl package versions; a missing
package fails the build. To build only the RPMs:

```sh
scripts/mkosi build-runtime-rpms
```

The RPMs, source pins, build identity, and generated mkosi configuration are
under `_build/mkosi/runtime-rpms`. Verified RPMs are cached with the package
cache. Changes to source pins, RPM specs, build tooling, or the builder's
installed packages invalidate that cache.

`packages/containerd/source.json` pins the upstream tag, its commit, and the Go
toolchain. `packages/crun/source.json` pins the upstream release archive's
SHA-256. The corresponding specs define the compiled features and package
contents. `renovate.json` includes managers for these version and integrity
fields; containerd updates stay on the selected 2.3 release line. Self-hosted
Renovate needs no post-upgrade command to update these pins.

Package inventory verification rejects missing, duplicate, stale, or Fedora
containerd and crun packages. Before landing an update, exercise Kubernetes
bootstrap and workloads, reboot, upgrade, and rollback with the resulting
images. The runtime remains part of the OS image and follows the ordinary
KatlOS upgrade path.

### Runtime component review

The runtime image build checks `mkosi.profiles/runtime/components.lock` before
publishing the root filesystem. Adding a component is an intentional product
change: review its purpose, dependencies, services, privileges, and supported
lifecycle before adding its name to the lock. This includes components added by
distribution updates inside an existing package.

The lock records resolved package names, executable entrypoints, and systemd
system and user unit names. It does not pin package versions, file contents,
shared-library filenames, firmware, kernel module paths, translations, or other
data files. Removing a component does not fail verification. These boundaries
keep ordinary updates independent of incidental file and version changes.
The resolved package list also includes build dependencies removed before
publishing the image; their additions still require review.
The unpacked build tree retains package-manager state for mkosi's extension
package resolution. Published runtime artifacts exclude the RPM database and
DNF transaction and tool metadata.

Inspect a completed build with:

```sh
go run ./cmd/katl-runtime-components record \
  --root _build/mkosi/katl-runtime-root \
  --packages _build/mkosi/katl-runtime.packages.tsv \
  > _build/runtime.components
diff -u mkosi.profiles/runtime/components.lock _build/runtime.components
```

Review the additions, then edit the lock to approve only the intended names.
Do not refresh it automatically during builds or dependency updates. The shared
lock covers both kernel flavours; retain the approved kernel package names for
both when reviewing one build. When intentionally removing a component, remove
its approval too so that its reintroduction requires review. Check the edited
lock with the same command,
replacing `record` with `verify` and omitting the output redirection.

Instrumented VM images additionally use
`internal/vmtest/image/components.lock`. This separate lock approves their test
entrypoints without permitting them in production verification. To inspect an
instrumented image, pass
`--test-lock internal/vmtest/image/components.lock` to `verify`. Production image
checks also enforce the existing boundary against shipping VM-test support.

### VM host requirements

The development shell includes the complete supported VM test toolchain. It
does not configure host libvirt, `/dev/kvm`, `/dev/net/tun`, networks, storage
pools, or polkit access; keep those in the host configuration.

The host must provide:

- A running system libvirt daemon reachable at `qemu:///system`, or an explicit
  `KATL_VMTEST_LIBVIRT_URI` override.
- User access to the system libvirt daemon through the host's libvirt group,
  polkit policy, or an explicit privileged manual session.
- An active libvirt network named `default`, or
  `KATL_VMTEST_LIBVIRT_NETWORK` set to the active test network.
- Permission to define NAT networks in that daemon, which must be libvirt 9.7 or
  newer so networks keep their metadata. Each VM test scenario creates and
  removes its own network on a free /24 in 10.200.0.0/13.
- An active libvirt storage pool named `default`, or
  `KATL_VMTEST_LIBVIRT_STORAGE_POOL` set to the active test pool.

## Optional development tools

- `virt-install`: useful for manual libvirt VM creation.
- `virt-manager`: useful GUI for inspecting and debugging local VMs.
- `/dev/net/tun` and `vhost_net`: useful for libvirt networks and richer VM
  networking.

## VM test worlds

Enabled VM, first-install, installed-runtime, and multinode kubeadm smokes run
through one runner-created world. Use `scripts/vmtest-run` instead of preparing
fixture environment files or invoking `scripts/vmtest-exec` directly:

```sh
scripts/vmtest-run ./internal/vmtest \
  -run 'FirstInstallTargetDisk|InstalledRuntime|ConfigApply' -count=1
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run 'TwoNodeOperationBackedBootstrap|ThreeControlPlaneStackedEtcd' \
  -timeout 60m -count=1
```

To exercise the release boundary rather than locally built Kubernetes
artifacts, provide digest-pinned source and target OCI bundles. The upgrade
scenario fetches and verifies both bundles through the product bundle path:

```sh
KATL_KUBERNETES_VERSION=v1.36.1 \
KATL_VMTEST_KUBERNETES_UPGRADE_VERSION=v1.36.2 \
KATL_VMTEST_KUBERNETES_BUNDLE='ghcr.io/katl-dev/kubernetes@sha256:f6060cd025aa28db3ac76c82c45f8aac858baa198d09829e36ff9c920bb1e281' \
KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE='ghcr.io/katl-dev/kubernetes@sha256:6362d1f3b79fd73323b9f77be8348ac2c76db83b95eb9f818d710eacc89493fb' \
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^TestKubeadmUpgradeOperationSmoke$' \
  -count=1 -failfast -timeout 75m
```

The runner creates a temporary world under `${TMPDIR:-/tmp}/katl-vmtest/`, probes
host capabilities, records `world.json`, `host-capabilities.json`, `run.json`,
and `go-test.log`, exports the world environment, and then executes `go test`
with the caller's package patterns and flags. Argument meaning belongs to
`go test`; `scripts/vmtest-run` only adds the harness execution needed to route
compiled package test binaries through `scripts/vmtest-exec`. VM test package
binaries are serialized by default because they share world fixtures and host
resource pools; pass an explicit `-p` value to override that policy.

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

## GitHub checks

The low-cost pull-request workflow runs formatting, whitespace, unit/golden, and
delivery fixture checks through the same command used locally. Before pushing a
branch, run:

```sh
scripts/check-fast origin/main...HEAD
```

The command checks all tracked Go formatting and patch whitespace, runs `go vet ./...`, and runs the
complete Go suite with `-count=1` so cached test results cannot hide failures.
Changes reach `main` through pull requests and the merge queue after
[CI](development/ci.md) passes. Pull requests run fast checks, the Nix build, and
kernel driver gates. The merge queue repeats them against the merged result and
also runs the VM suites for both kernel flavours; none of this runs again after
the merge. To run the VM suites on a pull request before queueing it, add the
`vmtest` label; later pushes to the branch keep running them while the label is
present. Pull requests into a `release/[0-9]*` branch run them without the label.
Adding `vmtest` reruns the head commit's CI; other labels do not affect CI, and
fork pull requests cannot be restarted this way. CI names the base it tested, and
the release branch ruleset waits for `CI (release)`, so open release pull
requests with `gh pr create --base release/...`; after retargeting an existing
pull request onto a release branch, close and reopen it or push a commit. Agents still
run the relevant local `scripts/vmtest-run` gates before opening a pull request,
as `AGENTS.md` requires. Kubernetes producer validation shares the VM image
build and cluster lane. Direct pushes are not part of the supported development
loop.

The merge queue marks an entry `UNMERGEABLE` when it conflicts with one ahead
of it, but evicts it only when it reaches that entry, so a doomed PR can wait in
the queue and hold up the PRs behind it. `katl-queue`, which the development
shell provides (or `scripts/katl-queue` outside it), checks for this before a
PR joins and follows the queue afterwards. It needs an authenticated `gh`:

```sh
katl-queue status            # queued entries in order, and PRs waiting to join
katl-queue check 123         # merge 123 over origin/main and the queue ahead of it
katl-queue enqueue 123       # check, then enable auto-merge; refuses on conflict
katl-queue watch 123         # one line per state change until 123 merges or leaves
katl-queue why 123           # removal reason, failing jobs and their first errors
katl-queue plan [--milestone M]   # order ready PRs for the queue; report held ones and their blockers
katl-queue plan --milestone M --apply   # make an autopilot pass: the plan, rebases and explanations
katl-queue plan --next       # open, unassigned issues whose blockers are all closed
katl-queue autopilot         # report what a pass would rebase, enqueue, dequeue or explain
katl-queue prune-runs        # report CI runs of merge groups the queue no longer holds; --apply cancels them
```

`check` merges with `git merge-tree` in a throwaway object directory, so it
does not change any branch, ref or working tree; it exits 1 and names the
conflicting PR and paths when the merge conflicts. `watch` with no arguments
follows everything queued or waiting with auto-merge, polls every minute
(`-interval` changes this), keeps going through transient `gh` failures, and
exits 1 when a watched PR leaves the queue without merging.

`plan` simulates every ready, non-draft PR over `main` plus the queue and the
PRs it has already chosen. It orders by `release-blocker`, `dev-experience`,
everything else, and age. A PR that declares a dependency (GitHub's blocked-by
on the PR and on the issue it closes, and `Depends on #N` lines in the PR body)
is held until the dependency has merged, so dependents join the queue in a
later pass; a blocker in another repository cannot be checked and holds the PR
too. A conflicting PR is held behind the PR it conflicts with. Like GitHub, it
gates on required checks only: a PR whose required checks failed, that the
queue removed for failed checks, or that someone dequeued by hand is held
rather than re-enqueued until its head changes. katl-queue runs as your own
login, so GitHub records its dequeues and yours alike as manual removals; it
posts a hidden marker naming the head before it dequeues an unmergeable entry,
and treats every other manual removal as yours. It records the head it first
saw with each such removal in a comment, so any new head, including its own
rebase, settles the removal. A failing optional check is reported once per head
and holds the PR only when another PR declares a dependency on it.

When the queue rebuilds a group (a PR leaves, or one is added ahead), GitHub
deletes the old group's `gh-readonly-queue` ref but leaves its `ci.yml` run going,
and that run would hold or wait for a runner. `.github/workflows/prune-merge-runs.yml`
runs `katl-queue prune-runs --apply` on each deleted group
branch and every ten minutes, and cancels unfinished merge-group runs whose
ref no longer exists or points at a different commit, and cancels nothing when
the ref listing is empty but the merge queue is not.

A `Hold: <reason>` line in a PR body, outside a code fence, holds the PR with
that reason before any other check, and autopilot disables its auto-merge. The
orchestrator adds `Hold:` while a PR awaits VM gates or a journey, and deletes
it when they pass. Editing the body does not trigger `ci.yml`, which runs on
opened, synchronize, reopened and labeled, so the hold lifts on the next pass.

`autopilot` rebases only a PR that the plan would otherwise enqueue and that
conflicts with `main`, pushing a clean result with `--force-with-lease` over
HTTPS through `gh auth git-credential`. It labels a conflicting rebase
`needs-rebase` with a comment naming the files, disables auto-merge on PRs held
for required checks, an ejection, a dequeue or a `Hold:` (never for an optional
failure or a rebase), dequeues entries the queue marks `UNMERGEABLE`, and posts
the `why` excerpt on a PR the queue removed for failed or timed-out checks. It
never retries checks and never touches `release/*` branches or PRs targeting
them. `plan` and `autopilot` only report; `--apply` makes the changes (both
need `--milestone`; `plan --apply` is one autopilot pass), and
`autopilot --once` runs a single pass instead of polling.

Fast checks skip mkosi builds, libvirt/KVM setup, VM scenarios, and publishing.
Run focused VM gates locally with `scripts/vmtest-run` on a capable host and
record the result with the change or release evidence.

## Fedora package inputs

The selected Fedora base release is set in `mkosi.conf`. The builder image,
runtime identity, and build cache derive from it. Builds consume signed Fedora
repositories without committing the complete transitive package closure as
source policy.

The mkosi build records resolved package inventories under `_build/mkosi/`.
Release and VM preparation copy those identities into their resource manifests
alongside tool, profile, and artifact digests. These inventories are diagnostic
release evidence for auditing or reproducing a problem, not acceptance policy;
normal Fedora movement does not require a repository PR.

Pin an individual package only when a documented Katl compatibility or security
constraint requires it. See `docs/internal/testing-contract-policy.md` for the
distinction between product contracts and build evidence.

## GitHub release artifacts

Follow [Release KatlOS](development/releases.md) to prepare a version, download
and verify a candidate, and monitor automated qualification and publication.
Start with `scripts/katl-release prepare VERSION`; it performs read-only checks
and prints the commands for starting the tag build.

`.github/workflows/release-artifacts.yml` builds Katl artifacts for pushed `v*`
KatlOS tags. It can also be dispatched manually with an explicit version for
build verification. Manual runs
retain one GitHub Actions artifact; tag runs additionally publish those exact
files as assets on the matching GitHub Release.

Tag runs publish only after **Release validation** passes baseline tests, the
regression suite for both flavours, and lifecycle tests against the final
production bundle. Publication uses those qualified artifacts without rebuilding.
See the [release procedure](development/releases.md) for candidate inspection,
qualification coverage, and failure handling. Final bundles remain available as
Actions artifacts for 30 days.

Manual builds qualify the source and packaging but produce separate bytes from
a later tag run. Rebuilding or rerunning producer jobs invalidates the recorded
artifact qualification and requires repeating the affected journeys.

Pushes to `main` run only the fast checks and the Nix build. Release-artifact
builds restore caches that only Release Artifacts saves, so they do not depend on
CI caches. They run only for tags or explicit manual
candidates, and later runs do not cancel them.

Release production uses a dependency-aware job graph. Release Artifacts calls the
reusable `.github/workflows/production-build.yml` once per flavour (`build-standard`,
`build-lts`) with the inputs `flavour`, `version`, `previous-tag`, and `publication`.
Within a build, the runtime and installer jobs start concurrently and the
extension closures follow the runtime. An assemble job then packages the install
and upgrade images concurrently, builds `katlctl`, assembles the installer ISO,
and stages the flavour's release set. Each flavour's Production VM qualification
starts as soon as its own build ends, and a final job in Release Artifacts attests
the combined bundle. Runtime and installer jobs each own
separate Go, mkosi package, and Docker builder-layer caches, avoiding competing
writes. Go and package caches save a fresh snapshot per source commit and restore
the latest compatible snapshot. The builder is loaded through Docker Buildx;
it checks for an updated Fedora base and reuses unchanged tool-install layers.
Mkosi incremental filesystem caches and finished release images are not reused
between runs. Published-asset provenance checks use bounded concurrency
because every release asset has an independent attestation.

KatlOS release versions use `YYYY.M.PATCH` calendar versions with `-dev.N`,
`-alpha.N`, `-beta.N`, and `-rc.N` prereleases. The first public alpha for the
July 2026 line is `2026.7.0-alpha.1`; the stable identity is `2026.7.0`. Use
tags such as `v2026.7.0-alpha.1`; release series branches such as
`release/2026.12.x` carry no version. The workflow strips an optional `v` before embedding
the version and rejects noncanonical versions. See
`docs/internal/adrs/adr-009-katlos-calendar-versioning.md` for the policy.

The published set contains Linux amd64 and Darwin arm64 `katlctl` binaries, the KatlOS
install and upgrade SquashFS images, and the installer UKI, kernel, initrd, and
UEFI-bootable ISO variants, each with
adjacent JSON metadata and SHA-256 files. The ISO embeds the matching KatlOS
install image and its metadata, so it is the primary self-contained install
artifact. The installer initrd contains an uncompressed early CPU microcode
archive followed by the zstd-compressed normal initramfs inside both the loose
initrd and UKI; its metadata records that combined format while filenames remain
stable.
It remains generic because node identity, disk selection, network,
and bootstrap configuration are supplied separately at boot. The loose
installer and install-image artifacts remain available for PXE deployments.
Loose runtime root/UKI intermediates and Kubernetes payload bundles are not
published through this workflow. Kubernetes bundles have a separate producer
contract. Tag publications create keyless SLSA build-provenance attestations
for every release asset using the `katl-dev/katl` workflow identity. The
workflow verifies each attestation against its exact signer workflow, tag ref,
and source commit before publishing, using the same `scripts/katl-release`
verification as release download. `SHA256SUMS` covers the staged asset set and
`PROVENANCE.md` documents user verification and the remaining trust boundary.
The staged `RELEASE_NOTES.md` is generated from commits since the previous
canonical KatlOS CalVer tag. It links each commit, includes the full comparison,
and becomes the GitHub release body. Its component tables use the staged RPM
inventories and the digest-pinned release extension bundles, including each
extension payload version for each kernel flavour. Kubernetes bundle tags are
deliberately excluded when selecting the previous KatlOS release.
This provenance does not provide Secure Boot signatures or implement Katl's
future node-side trust-root, revocation, and downgrade policy.

## Kubernetes bundle artifacts

`.github/workflows/kubernetes-bundles.yml` discovers upstream Kubernetes releases
on a schedule. Each upstream patch is published once, using a short version tag
such as `ghcr.io/katl-dev/kubernetes:v1.37.0`. Katl releases, source changes and
RPM rebuilds do not create new Kubernetes release identities or replace an
existing publication. See [Kubernetes release delivery](development/kubernetes-releases.md)
for discovery, interrupted-publication recovery and compatibility selection.

Pull requests validate affected build recipes without publishing. Manual
workflow dispatch defaults to build-only validation; set `publish: true` on
main to publish a missing upstream version or resume an interrupted publication.
Published versions are checked before package resolution or image builds, and
existing digests are retained.

The checked-in supported-version manifest and recipe fingerprint are local
build fixtures. `prepare-supported` and `refresh-rebuilds` maintain those
fixtures; they do not request new public versions. New bundles use `vVERSION-1` build tags alongside `vVERSION` aliases.
The updated Katl release resolves tags once and records digest-only references
with the Kubernetes version stored separately. Operators select upstream versions; they do not track Katl rebuild
counters. A future runtime interface requires a separate compatibility-policy
decision.

The OCI manifest carries the standard source, description, and MIT license
annotations that GHCR renders on the package page, plus title, documentation,
revision, and version metadata for other OCI clients.

GitHub creates a new container package as private. After the first publication,
an organization owner must make the `kubernetes` package public in its package
settings so uncredentialed KatlOS nodes can fetch it. This is a one-time GHCR
namespace operation. The workflow summary prints the readable and digest-pinned
OCI bundle references for release audit and expert diagnostics; ordinary
ClusterConfig continues to contain only the Kubernetes version. Published
development bundles remain unsigned until the signing policy lands; the GitHub
attestation records build provenance but is not yet a trust decision enforced
by `katlc`.

The scheduled public-bundle workflow derives every supported readable tag from
the committed policy, resolves each immutable digest anonymously, verifies
every OCI blob, and verifies each GitHub attestation. A patch release therefore
does not require a second commit just to update hard-coded verifier digests.

## VM tests on a capable host

GitHub-hosted Ubuntu runners execute the libvirt suite with KVM, OVMF, and
vsock. See [VM continuous integration](development/ci.md) for the automated
journeys, required results, and optional runner configuration. The commands
below run the same harness on a capable development host.

The [Kubernetes release producer](development/kubernetes-releases.md) discovers
stable upstream versions and publishes verified bundles automatically. Its
artifact checks do not substitute for the capable-host bootstrap and upgrade
gates below; VM validation in the unattended producer is explicitly deferred.

Run the applicable gates directly on a capable development or release host.
The installer-media gate includes a complete ISO install and observes the
installer's autonomous reboot into generation 0; the short ISO and PXE checks
remain fast boot-media diagnostics:

```sh
scripts/vmtest-run --artifact-set=runtime ./internal/vmtest \
  -run '^TestDirectRuntimeVMTestAgentSmoke$' \
  -count=1 -failfast -timeout 20m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestFirstInstallTargetDiskBootSmoke$' \
  -count=1 -failfast -timeout 35m
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^(TestInstallerISOBootSmoke|TestInstallerPXEBootSmoke|TestInstallerISOFirstInstallSmoke)$' \
  -count=1 -failfast -timeout 35m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestInstalledRuntimeConfigApplyModesSmoke$' \
  -count=1 -failfast -timeout 45m
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke$' \
  -count=1 -failfast -timeout 60m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestInstalledRuntimeSysupdateRootUKITransfer$' \
  -count=1 -failfast -timeout 45m
KATL_KUBERNETES_VERSION='<source-version>' \
KATL_VMTEST_KUBERNETES_UPGRADE_VERSION='<target-version>' \
KATL_VMTEST_KUBERNETES_BUNDLE='<digest-pinned-source-bundle>' \
KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE='<digest-pinned-target-bundle>' \
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^TestKubeadmUpgradeOperationSmoke$' \
  -count=1 -failfast -timeout 75m
```

The Kubernetes upgrade gate must use deliberately selected, digest-pinned
source and target OCI references. It proves etcd snapshot capture,
control-plane-first kubeadm upgrade, live sysext replacement with a kubelet-only
restart, durable generation promotion without reboot, worker upgrade, and final
cluster health. Record the selected bundle pair with the retained gate evidence.

The runtime artifact set is intentionally limited to direct-runtime tests.
Installed-runtime tests consume the KatlOS install image and reject
`--artifact-set=runtime`; run them with `--artifact-set=default` or
`--artifact-set=install` so fixture invalidation follows the installed payload.

Use delete-on-success retention for routine gates. While debugging, use
`--debug-on-failure` with a single narrow scenario and retain the run directory
as evidence.

Clean up retained state with two commands, and only these:

```sh
scripts/vmtest-clean /tmp/katl-vmtest/<run-id>
scripts/worktree-clean <worktree-path>
```

`scripts/vmtest-clean` destroys and undefines the run's recorded domains,
releases the networks the run owns, confirms with libvirt that every domain is
gone, and then deletes the run directory, including read-only files left by
mkosi and libvirt. It refuses any directory that is not a direct child of a
`katl-vmtest` directory containing a `result.json`, and repeating it on a
removed run succeeds. Given a single `result.json` it removes that node's
domains and leaves the directory.

`scripts/worktree-clean` removes a linked git worktree and its untracked build
output, such as subuid-owned `_build/` trees. It refuses the main worktree, a
worktree with tracked modifications or staged changes, and one whose HEAD
commit is on no remote branch after fetching `origin`, unless GitHub reports a
merged pull request whose head commit is exactly HEAD (squash merges and
deleted head branches leave no remote ref). The message names what is modified
or unpushed, and says so when GitHub could not be asked, which is never treated
as safe. It then deletes the local branch only when that branch is merged into
`origin/main`, contained in a remote branch of the same name, or at the head of
that merged pull request, and otherwise keeps it and says so.

Run each command on its own, without a leading `cd … &&` and without a pipe, so
that an allow rule for the command matches it. Do not clean up with `rm -rf`,
`chmod -R`, `git worktree remove --force` or `virsh destroy`.

The focused external-worker regression uses one Katl control-plane VM and an
independently provisioned [external worker](operations/external-workers.md). It
removes kubeadm cluster configuration and stops the control plane's Katl
management agent before joining the worker, then checks readiness, repeat
operations, and cross-node Service traffic through the public interfaces:

```sh
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run '^TestExternalWorkerJoin$' -count=1 -v -timeout 30m
```

### Prove host capabilities

Run the full enabled world suite from the Nix development shell on a host with
readable OVMF firmware, `/dev/kvm`, `/dev/vhost-vsock`, `/dev/net/tun`,
libvirt system access, an active libvirt network, and an active libvirt storage
pool:

```sh
nix develop -c env \
  KATL_VMTEST_RUN_ID=capable-host-$(date -u +%Y%m%dT%H%M%SZ) \
  scripts/vmtest-run ./... -count=1
```

A capable-host run exits zero after `go test` completes. The runner records
`world.json` and `host-capabilities.json` before handing off to Go; enabled
tests write their own per-scenario artifacts under the world directory. A
restricted host should fail during setup with explicit host capability gaps
rather than fixture generation errors.

## Sanity checks

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

## Common issues

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

### Kernel flavours

Use `KATL_FLAVOUR=standard` (the default) or `KATL_FLAVOUR=lts` with
`scripts/mkosi`. Both runtime and installer profiles use the selected track:

```sh
KATL_FLAVOUR=lts KATL_VERSION=2026.9.0-dev.1 scripts/mkosi build-katlos-install-image
KATL_FLAVOUR=lts KATL_VERSION=2026.9.0-dev.1 scripts/mkosi build-installer
```

Builds use `_build/mkosi` by default; switching flavours invalidates artifact
stamps and uses a separate incremental cache. `KATL_MKOSI_BUILD_DIR` overrides
the output directory; use separate directories for concurrent builds. Internal
filenames are identical inside these isolated directories. Release staging adds `-lts` to published LTS
assets and rewrites their adjacent metadata and checksum filenames.

Both flavours consume the reviewed lock in
`kernels/fedora-<release>/x86_64/<flavour>.json`. Runtime, installer, and extension
builds use the retained RPM bytes identified by that lock. The matching `devel`
package supplies the runtime's exported headers. Generic `kernel-headers` and
userspace packages continue to resolve from their existing repositories.

A Fedora release bump requires retained locks for both flavours and
qualification against the target base. Builds fail if a selected lock or retained
package is missing; they do not fall back to a moving kernel repository.

### Qualify retained kernel inputs

The Kernel Updates workflow captures each flavour's signed kernel RPM set and
opens a separate PR after retaining the signed inputs and verifying anonymous
retrieval. Standard and LTS updates run independently, so a failure in one does
not block the other. Retention and verification also run for unchanged inputs;
the publisher reuses existing OCI content. The workflow runs daily or on manual
dispatch. Automatic
discovery refuses versions older than the committed lock. An explicit release
permits a deliberate rollback. To qualify an exact kernel pair instead of
discovering newer versions:

```sh
gh workflow run kernel-updates.yml --ref main \
  -f standard_release=7.3.3-300.fc45.x86_64 \
  -f lts_release=6.18.60-200.fc45.x86_64
```

To update only one flavour, select it with `flavour`:

```sh
gh workflow run kernel-updates.yml --ref main -f flavour=lts \
  -f lts_release=6.18.60-200.fc45.x86_64
```

PR CI qualifies each changed kernel flavour with runtime and installer builds,
every release extension, complete module composition, VM regression, and
production-driver DRBD replication. Unchanged App-authored kernel-lock-only
proposals can merge automatically after CI and branch protection pass. These
virtual machines do not qualify NVIDIA hardware.

To prepare inputs for a Fedora base upgrade before changing `mkosi.conf`, run
the trusted-main workflow with the target release:

```sh
gh workflow run kernel-updates.yml --ref main -f fedora_release=45
```

The workflow publishes and anonymously verifies the captured RPMs. When the
target differs from the selected base, it creates no kernel-only proposal:
that proposal's CI would still build the selected base. After the run succeeds,
download its verified locks into the migration checkout:

```sh
gh run download RUN_ID --name kernel-lock-fedora-45-standard --dir kernels/fedora-45/x86_64
gh run download RUN_ID --name kernel-lock-fedora-45-lts --dir kernels/fedora-45/x86_64
```

Include both locks with the `mkosi.conf` release change and any required recipe
updates in the migration branch. Input preparation does not qualify the new
base. Use the existing release workflow to build and qualify complete artifacts
on that branch:

```sh
gh workflow run release-artifacts.yml --ref FEDORA_UPGRADE_BRANCH \
  -f version=2026.11.0-dev.45
```

Wait for **Release validation**, then use the final Actions bundle for the
persistent `katldev` and public `katlctl` journey. This manual branch run does
not publish a KatlOS release or release-extension OCI artifacts. See
[GitHub release artifacts](#github-release-artifacts) for the build contract.

For local candidate qualification, capture the selected flavour inside the
builder and select its retained OCI layout explicitly:

```sh
scripts/mkosi box -- go run ./cmd/katl-kernel-release candidate \
  --fedora-release "$(scripts/fedora-release)" --flavour lts \
  --kernel-release 6.18.60-200.fc45.x86_64 --output _build/kernel-candidates/lts

export KATL_FLAVOUR=lts
export KATL_KERNEL_LOCK="$PWD/_build/kernel-candidates/lts/lock.json"
export KATL_KERNEL_LAYOUT="$PWD/_build/kernel-candidates/lts/layout"
export KATL_MKOSI_BUILD_DIR="$PWD/_build/kernel-qualified-lts"
scripts/mkosi build-runtime
scripts/mkosi build-installer
scripts/build-release-extensions
scripts/assemble-release-extensions
```

The lock fixes `core`, `modules`, `modules-core`, and matching `devel` RPM bytes.
Other packages retain their existing repository resolution. The builder verifies
the RPM cache before image-cache reuse and uses a digest-specific mkosi cache
key. If verification reports a corrupt RPM cache, remove only the directory
named in the error and repeat the build.

The dedicated `katl/kernel-inputs` GHCR package needs public visibility after its
first publication. Before selecting a retained lock for ordinary builds, verify
anonymous retrieval from a fresh cache without supplying a local layout:

```sh
go run ./cmd/katl-kernel-release verify \
  --lock _build/kernel-candidates/lts/lock.json --cache _build/kernel-public-check
```

Keep immutable digest-tagged kernel inputs available while committed locks
reference them. Actions artifacts and caches are temporary workflow handoff;
they do not replace retained RPM storage. The design and bootstrap boundary are
specified in `docs/internal/adrs/adr-017-retained-kernel-inputs.md`.
