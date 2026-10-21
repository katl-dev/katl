# Hosted VM scenario experiments

Use the `Hosted VM scenarios` workflow to evaluate Katl's existing libvirt
journeys on ephemeral Ubuntu x64 runners. Pull requests select groups by changed
paths; the release-artifact workflow calls the full suite for main, release
branches, tags, and manual candidate builds. You can also invoke the VM workflow
manually. It does not publish artifacts. GitHub describes nested virtualization as
[technically possible but unsupported](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners).
Blacksmith documents
[KVM support on x64 Linux](https://docs.blacksmith.sh/blacksmith-runners/overview).
The workflow defaults to the four-core Blacksmith runner; select `ubuntu-24.04`
with its runner input for the GitHub baseline. The four-core Blacksmith runner has 16 GB RAM and 80 GB storage;
the eight-core runner has 32 GB RAM and 160 GB storage.

Set the repository Actions variable `KATL_VMTEST_RUNNER` to choose the hosted
default: `ubuntu-24.04` or a Blacksmith Ubuntu 24.04 label with 2, 4, or 8 vCPUs.
An explicit workflow `runner` input overrides repository settings. Planning and
final result checks continue on GitHub-hosted runners.

For the dedicated cluster pool, set `KATL_VMTEST_MAIN_RUNNER=katl-vmtest` after
the rollout checks below pass. This override applies only to `katl-dev/katl`
`main` revisions. Pull requests, release branches, tags, and other candidate
refs retain the hosted default. Requests for `katl-vmtest` on other refs fail
in planning before any VM job is scheduled. Keep a hosted provider available
for those refs even after switching main.

The hosted preparation path expects an ephemeral Ubuntu 24.04 x64 host with
Docker, passwordless sudo, and systemd. It installs the remaining tools. The
`katl-vmtest` path uses the prepared ARC image's existing Docker and libvirt
daemons without invoking systemd or reconfiguring QEMU. It installs Podman if
absent, because the Kubernetes fixture cache requires multi-image archives.
Both paths check active libvirt networking and storage, firmware, KVM VM
creation, vhost-vsock access, and AF_VSOCK binding. The producer then proves
actual guest-agent communication. An unprivileged runner pod cannot meet this
contract.

## Enable the ARC pool

The runner label and image contract are defined by
[home-ops #3132](https://github.com/Zariel/home-ops/pull/3132). Each privileged
ephemeral pod owns its Docker daemon, libvirt network, storage, and test disks.
The node supplies KVM, vhost-vsock, and TUN. The image starts its daemons before
accepting work; workflow preparation does not own their lifecycle.

1. Deploy the runner image and scale set using the home-ops rollout procedure.
2. Dispatch `Hosted VM scenarios` on `main`, setting `runner` to `katl-vmtest`.
   Require the producer agent probe, all eight scenario groups, and final
   `VM validation` to pass. Inspect uploaded cgroup memory/CPU diagnostics and
   failure evidence. A build or runner-image check alone does not qualify the
   multi-VM suite.
3. Verify the exclusive device allocation: each eligible node advertises one
   `kernel.org/vmtest` device and each runner requests one. Overlapping runner
   jobs must occupy different nodes or queue. Guest vsock CIDs share a
   host-kernel allocation domain, so revisit ownership before allowing multiple
   runners or other vsock VM consumers on one node.
4. Set `KATL_VMTEST_MAIN_RUNNER` to `katl-vmtest`. Subsequent main builds use the
   pool; delete that variable to return main to the hosted default.

[Katl #370](https://github.com/katl-dev/katl/pull/370) provides the dedicated
pool's dispatch entry point. When combining it with this matrix, use one main
trigger: the release-artifact workflow already calls this suite. Keep the
dedicated entry point manual to avoid running a second full suite on every
main push. Do not route pull-request revisions to the privileged pool.

## Gate selection

`cmd/katl-vmtest-plan` owns the scenario catalog and conservative pre-merge
selection. Documentation, maintainer release tooling, and ordinary unit-test
changes need no VM build. Changes to the CLI's configuration compatibility
and inspection files select configuration, management, and
two-node bootstrap. Credential-rotation changes select management. Shared CLI
and product changes, VM infrastructure changes, and
unrecognized paths select all groups. Main and candidate builds always select
all groups. This is a coarse policy, not a claim to infer all Go dependencies.

The PR diff includes both sides of renames. Empty selections skip the producer
and VM jobs explicitly. The stable `VM validation` job checks the plan and all
selected job results, including failure and cancellation; a skipped selected
gate cannot satisfy it. Keep the ordinary unit and generated-asset checks too.

## Execution model

One producer builds the standard runtime with `KATL_VMTEST_IMAGE_SUPPORT=1`,
then runs `TestDirectRuntimeVMTestAgentSmoke` on that host. It builds the
installer ISO, install and upgrade images, and Kubernetes sysext once. These
instrumented images are test inputs and must never be promoted into release
assets.

The producer uses mkosi's native `Mirror=` setting for Fedora's canonical
HTTPS repository, keeping repository metadata and package downloads on the
same source. The generated setting is included in the evidence. Package
integrity checks remain enabled; this does not provide immutable package
inputs or production-artifact qualification.

Independent scenario jobs download the producer's immutable files, verify the
source commit, builder version, and SHA256 checksums, then invoke
`scripts/vmtest-run --artifact-set=install --no-rebuild`. The existing harness
records its resource manifest, creates fresh VM worlds, and owns libvirt
lifecycle. Boot-only jobs use `--artifact-set=none` with their verified
current-source media. The producer splits boot media, installed-node inputs,
and upgrade inputs into separate bundles. Each consumer downloads only the
bundles it needs. It derives the proof directory and loose runtime components
from the self-contained install image, then checks the original component
checksums. Builder caches and installed guest fixtures are excluded from the
transfer. Archives preserve file modes and sparse files.

The test groups cover these contracts:

| Group | Journey |
| --- | --- |
| Producer | Direct runtime boot and guest-agent health over vsock. |
| Boot | Installer ISO and PXE boot, including network readiness. |
| Install | First install, installed boot health, and local configuration handoff. |
| Storage | Refuse destructive data-disk changes, preserve the existing filesystem, then accept corrected install configuration. |
| Config | Configuration apply modes, including online and next-boot behavior. |
| Management | Management credential rotation, repeat operation, and reboot. |
| Update | Host upgrade, health promotion, rollback, repeated upgrade, and preserved state. |
| Two-node | Operation-backed Kubernetes bootstrap and dependent configuration. |
| Three-node install | Independent generation-zero installation and identity across three nodes. |

Every selected top-level VM test must report a Go JSON pass event. A skipped
selected test fails the workflow. Existing guest assertions and timeouts remain in
effect; the workflow does not disable guest durability or security settings.

First-install readiness requires active boot-complete, SSH, and management
services, successful boot health, the persisted bootstrap handoff, and a healthy
promoted generation zero. The runtime test agent supplies the observation
transport. Interleaved console messages do not decide readiness. Fixture
production waits for the same contract before making a disk reusable.

## Evidence

Download the `hosted-vmtest-*-evidence-<run-id>` artifacts for command output,
exit codes, stage timing, source identity, input checksums, and libvirt logs.
Failed worlds also retain their small log, JSON, and XML files. Guest disks are
not uploaded. Successful worlds use the harness's delete-on-success policy.
Diagnostic artifacts expire after three days; shared image inputs expire after
one day. Record conclusions and run links before they expire.

Each measured command samples whole-host available RAM and root-filesystem
usage every ten seconds. These are sampled observations, not guaranteed peaks
or runner capacity promises. GNU time's maximum resident set is a process
measurement, not the sum of concurrent guests. Actions step durations include
the upload and download costs outside the measured test commands.

## Measured sizing

The October 21, 2026 [GitHub baseline](https://github.com/katl-dev/katl/actions/runs/39645570687)
and [four-core Blacksmith comparison](https://github.com/katl-dev/katl/actions/runs/39646270394)
produced these successful command durations, rounded to the nearest second:

| Command | GitHub Ubuntu 24.04 | Blacksmith 4 vCPU |
| --- | --- | --- |
| Runtime build | 4m10s | 4m22s |
| Installer and install image build | 3m05s | 2m09s |
| Direct agent gate | 35s | 32s |
| ISO and PXE boot group | 1m20s | 1m03s |
| Configuration group | 4m55s | 4m24s |
| Management group | 1m46s | 1m51s |
| Update and rollback group | 5m39s | 4m03s |
| Three-node install group | 5m13s | 5m06s |

These samples use independent cache scopes and runner allocations. They do not
establish a general speed multiplier or warm-main build cost. Both runs failed
the two-node group before Kubernetes bootstrap completed. The Blacksmith
comparison also exposed an install test that depended on interleaved serial
text.

The subsequent [complete Blacksmith run](https://github.com/katl-dev/katl/actions/runs/39653082651)
passed all eight groups with the corrected harness. It finished in 21 minutes
including planning, provisioning, image builds, transfer, and final validation.
The producer job took 8m13s. Consumer command durations were:

| Group | Duration |
| --- | --- |
| Boot | 1m22s |
| Install | 1m25s |
| Storage | 50s |
| Configuration | 4m13s |
| Management | 1m45s |
| Update and rollback | 3m57s |
| Two-node bootstrap | 9m36s |
| Three-node install | 4m20s |

The full run's sampled available RAM reached a minimum of 9.59 GiB during
two-node bootstrap; minimum root-filesystem space was 58.92 GiB during
three-node installation. These observations support the four-core runner for
this suite and identify bootstrap as the longest command.

A [two-core, 8 GB run](https://github.com/katl-dev/katl/actions/runs/39657973632)
also passed every group in 24m25s. Two-node bootstrap took 8m36s and sampled
available host RAM reached 1.93 GiB; three-node installation took 5m06s.
Allocated VM capacity was about 115 vCPU-minutes versus 214 for the first
four-core pass. These independent samples suggest a smaller hosted lane can
reduce allocated capacity, but do not establish billing savings or ARC cgroup
limits. Start ARC with its configured resources and measure actual pod peaks
and concurrent jobs before reducing them.

The split inputs measured 848,896,000 bytes for base, 825,958,400 for media, and
430,284,800 for upgrade. Across the original seven consumer groups this reduced
transfer from 20.27 GB to 7.18 GB, about 65%. Adding the storage group makes
the corresponding totals 23.17 GB and 8.85 GB, about 62%. These totals count
consumer payload downloads, excluding producer uploads, diagnostics, cache
traffic, and archive overhead from subsequent source changes.

## Qualification boundary

These tests qualify instrumented standard images built from the selected
source. Production release artifacts, the LTS flavor, Kubernetes version
upgrades, multi-control-plane recovery, and repeated reliability across runner
allocations need their own evidence. Hosted automation does not replace the
required public `katlctl` journey in a persistent `katldev` VM.

The release workflow requires this source-regression suite before its
publication-approval step. Automatic publication remains conditional on
qualifying the exact production bytes and supported journeys; instrumented
source tests alone do not establish that contract. Do not remove the approval
step solely because a subset of these scenarios passes.
