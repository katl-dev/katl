# GitHub-hosted VM-test feasibility

Status: investigation, October 2026. This document does not establish a required
CI gate or replace capable-host release qualification.

## Assessment

Standard GitHub-hosted Linux x64 runners can run Katl's installer ISO boot smoke
through the existing VM-test harness. KVM, vhost-vsock, libvirt, and OVMF can be
provisioned on them. Broader lifecycle suites remain unqualified. GitHub
explicitly describes nested virtualization as technically possible but
[unsupported](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners).
Treat usable hardware and a reliable release gate as separate questions.

Use the existing `scripts/vmtest-run` world and libvirt backend. Software
emulation, a second VM runner, or replacing vsock are not prerequisites for
exploring hosted execution.

## Runner choices

The [standard runner reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
specifies four CPUs, 16 GB RAM, and 14 GB storage for public Linux x64 jobs.
These standard jobs are free for public repositories. Pin `ubuntu-24.04` while
qualifying the host setup rather than following `ubuntu-latest` across OS
changes.

| Runner | Assessment |
| --- | --- |
| Standard Ubuntu x64 | Start with single-node boot and agent smokes, then measure install and two-node scenarios. Budget disk use explicitly. |
| Larger Ubuntu x64, 4 vCPU / 16 GB / 150 GB | More predictable disk capacity for image assembly and installed-node fixtures. Same CPU and RAM limits as standard public jobs. |
| Larger Ubuntu x64, 8 vCPU / 32 GB / 300 GB | More headroom for concurrent guests, Kubernetes, image layers, and fixture copies. Evaluate after measuring small suites. |
| Ubuntu ARM or macOS | Does not match the existing x86_64 Linux/libvirt test and artifact contract. |
| `ubuntu-slim` | A container on a shared VM; do not select it for the initial nested-VM implementation. |

[Larger runner specifications](https://docs.github.com/en/actions/reference/runners/larger-runners)
provide the listed storage and memory sizes. At the published
[x64 Linux rates](https://docs.github.com/en/billing/reference/actions-runner-pricing),
four cores cost $0.012/minute ($0.72/hour), and eight cores cost $0.022/minute
($1.32/hour). These runners are billed for public repositories too and require
an eligible organization plan. Larger SKUs have not been tested in this study;
extra capacity does not establish a nested-virtualization support guarantee.

## Evidence and limits

The isolated [probe workflow](https://github.com/katl-dev/katl/blob/980e746b0ede/.github/workflows/hosted-vmtest-probe.yml)
on `investigate-hosted-vmtests` provisions Ubuntu packages and uses read-only
repository permissions. Its workflow has a 20-minute timeout and
does not change required checks or publication. It checks device access, then
runs the existing `TestInstallerISOBootSmoke` using the published
`v2026.10.1` ISO through `scripts/vmtest-run --artifact-set=none --no-rebuild`,
with KVM forced on. This tests hosted execution of released media; it is not
qualification of an image built from the investigation commit.

The host probes established KVM API version 12 and successful VM creation from
an unprivileged process, read/write access to `/dev/vhost-vsock`, and binding an
AF_VSOCK socket. The existing ISO boot smoke then passed in a
[successful hosted run](https://github.com/katl-dev/katl/actions/runs/39638042560).
The QEMU command independently recorded `-accel kvm`. The test observed the
installer's waiting-for-configuration serial signal; it did not install a node.
One measured VM boot took approximately 37 seconds; its whole test step,
including compilation, took approximately 64 seconds. This is a boot-smoke
measurement, not an estimate for installation or Kubernetes journeys.

An observed machine had four CPUs, approximately 16 GiB RAM, and approximately
84 GiB free on a 145 GiB root filesystem. The extra disk space is an observation,
not a replacement for the documented storage allocation. Keep detailed run
diagnostics and provisioning-failure history in the investigation Bead.

Guest-to-host vsock traffic, instrumented-agent health, installation/reboot,
Kubernetes bootstrap, upgrades, rollback, and sustained reliability across runner
allocations remain separate proof obligations.

## Integration model to evaluate

Two integration options preserve the existing runner:

- Run a smoke on its image producer's host. This avoids a new artifact-import
  contract for the first current-source test, but image builds and VM fixtures
  share that host's disk budget.
- Use separate producer and VM jobs. This lets multiple journeys reuse one
  image set and gives VM execution its own capacity, but requires an explicit
  contract for importing the producer's immutable inputs and identities.

Prefer separate jobs as coverage grows, after proving that import contract.
Build each image set once rather than rebuilding the same runtime in every
scenario job. Keep VM-test agents and fixtures in test-only images; production
release images must remain free of them.

The existing runner distinguishes current-source installed-runtime artifacts
from arbitrary external media. Hosted install and lifecycle jobs need to carry
the producer's source identity, metadata, and required inputs through that
contract. The external-media ISO boot probe does not resolve this integration.

Provision system libvirt, NAT/DHCP (including `dnsmasq-base` when disabling
recommended packages), its storage pool, readable OVMF firmware,
KVM/vsock access, and the existing fixture tools on each ephemeral host. Run
Go tests as the runner user with appropriate groups rather than making the
whole build root-owned. Group changes need to take effect in the test process;
the probe uses `sg kvm` for that purpose. One-time device ACLs did not persist
reliably across steps. Verify actual network activity and device access in the
execution context that runs the tests.

Align the QEMU process identity with generated fixture ownership on the
single-user ephemeral host. The harness writes NVRAM with mode `0600`; Ubuntu's
default QEMU identity could not open it in the probe. Running QEMU as the
workflow user with group `kvm` resolved that failure while retaining AppArmor.
Do not infer that a reachable libvirt socket proves its QEMU process can access
all generated VM files.

Start with manually invoked, non-required jobs. Use the existing delete-on-success
policy, retain small failure logs and manifests as Actions artifacts, and avoid
uploading all guest disks. An ephemeral hosted runner cannot provide the retained
live VM debugging experience of a local capable host.

Before making a hosted check required, measure these stages without adding
retries or weakening assertions:

1. Repeat the ISO smoke across fresh runner allocations.
2. Run `TestDirectRuntimeVMTestAgentSmoke` using a test-only runtime to prove
   real guest-to-host vsock communication and agent health.
3. Run first install and reboot, then two-node bootstrap and configuration apply.
4. Run upgrade and recovery scenarios, measuring peak RAM, allocated disk space,
   wall time, and artifact transfer size for each job.

A required gate must fail when its capabilities or intended scenarios are
unavailable. Preserve a small summary of scenario outcomes and producer artifact
identities even when successful VM directories are deleted. A zero `go test`
exit status with skipped VM scenarios is not qualification.

Use larger runners if measured resource demand warrants them. Split longer
suites by meaningful journeys; GitHub-hosted jobs have a
[six-hour execution limit](https://docs.github.com/en/actions/reference/limits).
Retain capable-host qualification until hosted reliability and the artifact
contract have been proved. Automated hosted tests also do not replace the
required hands-on public `katlctl` journey in a persistent `katldev` VM.
