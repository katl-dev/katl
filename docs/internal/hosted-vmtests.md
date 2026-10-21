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

## Gate selection

`cmd/katl-vmtest-plan` owns the scenario catalog and conservative pre-merge
selection. Documentation, maintainer release tooling, and ordinary unit-test
changes need no VM build. `katlctl` changes select configuration, management,
and two-node bootstrap. Shared product changes, VM infrastructure changes, and
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
text. Consult the PR's final gate results for the corrected harness.

During the successful Blacksmith groups, sampled available RAM stayed above
12 GiB and root-filesystem space stayed above 58 GiB. Start with four-core
runners; measure simultaneous multi-control-plane and Kubernetes-upgrade
journeys before assigning them the same capacity.

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

## Self-hosting alternatives

Self-hosting does not inherently require a different VM-test backend. A
disposable capable runner VM on the Katl cluster could run the existing
libvirt world. KubeVirt supports a
[host-passthrough CPU model](https://kubevirt.io/user-guide/compute/virtual_hardware/);
actual nested KVM, vsock, TUN, firmware, storage, and networking capabilities
must be demonstrated inside that runner. This option keeps VM-test policy and
fixtures portable between hosted and self-hosted execution.

A native KubeVirt backend would instead own remote VM lifecycle, image import,
disk snapshots, firmware state, network allocation, diagnostics, and cleanup.
KubeVirt has a [vsock facility](https://kubevirt.io/user-guide/compute/vsock/), but
that does not make the harness's local CID connections portable across cluster
nodes. Prefer the runner-level option first unless distributed guest placement
or a measured nesting constraint justifies the additional backend. Keep a
recovery path outside the cluster hosting its own qualification jobs.
