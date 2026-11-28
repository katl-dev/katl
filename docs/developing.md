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

Use the [VM test runner](../scripts/README.md#vmtest-run) for automated
coverage and [katldev](../cmd/katldev/README.md) for persistent, hands-on
journeys. Use libvirt tools such as `virsh` for manual inspection.

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
- Required smoke path: [libvirt VM test runner](../scripts/README.md#vmtest-run).
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
- Generated build artifacts belong under `_build/`.

The libvirt-backed vmtest path is the automation contract because it gives the
smoke harness stable process control, serial output, timeout handling, network
leases, storage setup, and exit details.

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
- `libvirt`/`virsh`: defines, starts, observes, and tears down local VM test
  domains.
- KVM access: the VM test runner should see `/dev/kvm`.
- UEFI firmware for libvirt VM pflash boot, such as edk2/OVMF images.
- `git commit-wrapped`: provided by the development shell (`scripts/git-commit-wrapped`); takes the title as its argument and the body on standard input.

The supported top-level script surface is intentionally small. Use
`scripts/mkosi` for KatlOS image artifacts,
`scripts/build-kubernetes-sysext` for Kubernetes payload artifacts, and
[vmtest-run](../scripts/README.md#vmtest-run) for enabled test worlds. Other scripts are compatibility
wrappers, debug aids, or temporary validators for scaffolding work; do not
treat the whole `scripts/` directory as the public developer interface.

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

`nix develop .#ci` enters a smaller shell holding only the tools that tests
and the scripts they run look up. The default shell is that shell plus the
interactive tools, VM host clients and checkout-backed commands, so a test
sees the same tool versions in both. CI's Fast checks, release unit job, VM
lanes, Production VM and driver qualification run their steps in the `ci`
shell through `.github/scripts/ci-shell`. Only host services (libvirtd, QEMU
and OVMF) come from the runner's packages, and each VM job records their
versions and the flake revision in its evidence.

The shell puts checkout-backed commands on `PATH`. Find their references in
the [development tool index](../dev/README.md).

Use [hermetic](../scripts/README.md#hermetic) for isolated Go tests and
the [VM test runner](../scripts/README.md#vmtest-run) for enabled VM suites.

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

See [katldev](../cmd/katldev/README.md) for checkout artifact builds and
persistent development VM journeys.

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

## Optional development tools

- `virt-install`: useful for manual libvirt VM creation.
- `virt-manager`: useful GUI for inspecting and debugging local VMs.
- `/dev/net/tun` and `vhost_net`: useful for libvirt networks and richer VM
  networking.

## GitHub checks

The low-cost pull-request workflow runs formatting, whitespace, unit/golden, and
delivery fixture checks through the same command used locally. Before pushing a
branch, run:

```sh
scripts/check-fast origin/main...HEAD
```

Inside the hermetic wrapper, the command checks Go formatting and patch
whitespace, runs `go vet ./...`, and runs the complete Go suite with `-count=1`.
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
run the relevant [local VM gates](../scripts/README.md#capable-host-gates) before
opening a pull request, as `AGENTS.md` requires. Kubernetes producer validation shares the VM image
build and two-node lane. Direct pushes are not part of the supported development
loop.

Use [katl-queue](../cmd/katl-queue/README.md) to check for conflicts before
queueing a PR, track progress, and diagnose removals.

### Set up release coordination with Home Manager

On a Linux development host with a systemd user manager, add Katl to your
flake inputs.

```nix
inputs.katl.url = "github:katl-dev/katl";
```

Pass the `katl` input through `extraSpecialArgs` if you configure Home Manager
in a separate module. Import the Katl module and enable the tools, watcher, and
runner.

```nix
{ katl, ... }:
{
  imports = [ katl.homeManagerModules.katl-dev ];
  programs.katl-dev = {
    enable = true;
    watcher.enable = true;
    runner.enable = true;
  };
}
```

`programs.katl-dev.enable` puts `katl-queue` and the checkout-based
development commands on PATH. Use the Nix development shell for build and
test dependencies. Use `programs.katl-dev.runner.enable` to install
`katl-dev-run` and its user slice. From the Katl checkout, wrap your agent
with `katl-dev-run claude`. To start the coordinator, run:

```sh
katl-dev-run claude --agent katl-coordinator
```

The runner puts the agent in a delegated scope in `katl-dev.slice`, where
rootless Podman can create cgroups for its containers and VM workloads.
The coordinator resumes work from its local checkpoint. See the
[agent workflow](../dev/agents/) for checkpoint details, role definitions,
and hand-offs.

For watcher settings and background waits, see the
[katl-queue reference](../cmd/katl-queue/README.md#watchd).

Fast checks skip mkosi builds, libvirt/KVM setup, VM scenarios, and publishing.

## Fedora package inputs

The selected Fedora base release is set in `mkosi.conf`. The builder image tag,
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

Follow [Release KatlOS](development/releases.md) to release a version through
the **Release** workflow and verify what it published.
See the [katl-release reference](../cmd/katl-release/README.md) for the
commands used in that procedure.

`.github/workflows/release-artifacts.yml` builds and qualifies Katl artifacts
and publishes nothing. It runs nightly on `main` at a development version, a
manual dispatch builds an explicit version for build verification, and the
**Release** workflow (`release.yml`) calls it to build a candidate or final
version. Release tags come only from the Release action, and neither workflow
starts on a tag push at a commit made since the removal of the tag trigger.

Release publishes only after **Release validation** passes baseline tests, the
regression suite for both flavours, and lifecycle tests against the final
production bundle. Publication uses those qualified artifacts without rebuilding.
See the [release procedure](development/releases.md) for candidate inspection,
qualification coverage, and failure handling. Final bundles remain available as
Actions artifacts for 30 days.

Nightly and manual builds qualify the source and packaging but produce separate bytes from
a later Release build. Rebuilding or rerunning producer jobs invalidates the recorded
artifact qualification and requires repeating the affected journeys.

Pushes to `main` run only the fast checks and the Nix build. Release-artifact
builds restore caches that only Release Artifacts saves, so they do not depend on
CI caches. They run only for tags or explicit manual
candidates, and later runs do not cancel them.

Release production uses a dependency-aware job graph. Release Artifacts calls the
reusable `.github/workflows/production-build.yml` once per flavour (`build-standard`,
`build-lts`) with the inputs `flavour`, `version`, and `retention-days`.
Within a build, the runtime and installer jobs start concurrently and the
extension closures follow the runtime. An assemble job then packages the install
and upgrade images concurrently, builds `katlctl`, assembles the installer ISO,
and stages the flavour's release set. Each flavour's Production VM qualification
starts as soon as its own build ends, and a final job in Release Artifacts attests
the combined bundle. Runtime and installer jobs each own
separate Go and mkosi package caches, avoiding competing
writes. Go and package caches save a fresh snapshot per source commit and restore
the latest compatible snapshot. Every job pulls the builder image that
`mkosi-builder.lock` pins; see [Builder image](development/ci.md#builder-image).
Mkosi incremental filesystem caches and finished release images are not reused
between runs. Published-asset provenance checks use bounded concurrency
because every release asset has an independent attestation.

KatlOS release versions use `YYYY.M.PATCH` calendar versions with `-dev.N`,
`-alpha.N`, `-beta.N`, and `-rc.N` prereleases. The first public alpha for the
July 2026 line is `2026.7.0-alpha.1`; the stable identity is `2026.7.0`. Use
tags such as `v2026.7.0-alpha.1`. A release branch is named for the one final
version released from it, such as `release/2026.12.1`; a series branch such as
`release/2026.12.x` names no version. The workflow strips an optional `v` before embedding
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
and source commit before publishing. `SHA256SUMS` covers the staged asset set and
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

See the [capable-host gates](../scripts/README.md#capable-host-gates) for
local commands and [VM continuous integration](development/ci.md) for
automated coverage. Use [vmtest-clean](../scripts/README.md#vmtest-clean)
and [worktree-clean](../scripts/README.md#worktree-clean) for retained state.

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
