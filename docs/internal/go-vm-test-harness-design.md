# Go VM Scenario Test Harness Design

Status: proposed design.

This document defines the Go-authored VM scenario harness for Katl integration
tests. The harness grows the current shell smoke checks into typed scenario
tests without making libvirt, multi-node networking, or Kubernetes cluster
automation a first-boot prerequisite.

## Goals

Katl needs VM tests that can prove installer, runtime, update, rollback, and
Kubernetes behavior against real booted systems. The immediate test loop remains
small:

```text
build Katl artifacts
boot installer-image in QEMU/OVMF
deliver an install manifest through local handoff
install to a target disk fixture
boot the installed runtime
assert deterministic serial, journal, filesystem, and command results
preserve logs and disks on failure
```

The same harness should later run longer scenarios:

```text
first install
extra disk layout
kubeadm-ready runtime
single-node kubeadm init with a responsive API server
KatlOS root updates
rollback after failed boot health
Kubernetes sysext version upgrades
configuration generation upgrades
multi-node Kubernetes bootstrap and join
```

The harness is test infrastructure, not a provisioning product. It must not
make Katl responsible for DHCP, PXE, Kubernetes add-ons, GitOps, or site
cluster lifecycle. It may run test-only Kubernetes setup actions when a scenario
needs to prove Katl's kubeadm handoff boundary.

## Decisions

Use Go for scenario orchestration, assertions, artifact indexing, QEMU process
lifecycle, disk fixture modeling, timeout handling, and log collection. These
are stateful and need unit tests.

Keep `scripts/katl-vm` as the thin direct-QEMU compatibility wrapper while the
Go harness is introduced. The Go runner may call it for the first scenario, but
the durable API should be a Go QEMU runner so tests can inspect process state,
ports, serial streams, artifacts, and failures without parsing a shell wrapper's
human output.

Keep `scripts/check-mkosi-smoke` and `scripts/check-installed-disk-smoke` as
operator-friendly smoke commands. They can migrate to calling the Go harness
after the Go API is stable.

Use direct `qemu-system-x86_64` and OVMF for the required automated path.
`mkosi vm` remains useful for manual inspection. Libvirt may be added later for
persistent multi-node environments, but it is not required for first install,
single-node kubeadm, or rollback tests.

Store all generated state under `build/vmtest/<run-id>/` by default. Tests may
override the root with `KATL_VMTEST_STATE_ROOT` or a Go test flag. No committed
configuration should contain host-specific absolute paths for firmware, Nix
profiles, user homes, or local package stores.

Use scenario-owned disk images and QEMU snapshots to avoid mutating build
artifacts. Target disks are writable fixtures; installer and runtime source
artifacts are read-only inputs.

Preserve enough information after every failed run for offline debugging:
scenario manifest, artifact index, QEMU command, firmware vars copy, serial
logs, journal exports when available, handoff request/response, disk image paths,
and a machine-readable result file.

## Package Shape

The harness should be implemented as an internal Go package with a small CLI on
top:

```text
internal/vmtest
  Scenario, Runner, Result, ArtifactSet, DiskFixture, VM, Assertion

internal/vmtest/qemu
  direct QEMU command construction, OVMF discovery, process lifecycle,
  serial capture, user-mode networking, host port allocation

internal/vmtest/artifacts
  build artifact discovery, cache keys, artifacts.json parsing, digest checks

internal/vmtest/disks
  qcow2/raw fixture creation, snapshots, persisted failure copies, optional
  offline inspection helpers

internal/vmtest/kube
  later helpers for kubeadm and kubectl assertions in booted guests

cmd/katl-vm
  optional developer CLI over the same Go runner, once the package is useful
```

Tests should import `internal/vmtest` from Go integration packages. The first
test package can live under `internal/vmtest/scenarios` or `test/integration`
once implementation starts. Unit tests for the harness itself should mock
command execution and serial streams instead of requiring QEMU.

An initial test should look like this at the call site:

```go
func TestFirstInstallBootsRuntime(t *testing.T) {
    vmtest.RequireHost(t, vmtest.HostRequirements{
        QEMU: true,
        OVMF: true,
        KVM:  vmtest.KVMAuto,
    })

    artifacts := vmtest.RequireArtifacts(t, vmtest.ArtifactRequest{
        Build: []string{"installer", "runtime"},
    })

    result := vmtest.Run(t, vmtest.Scenario{
        Name:      "first-install",
        Artifacts: artifacts,
        Disks: []vmtest.DiskFixture{
            vmtest.TargetDisk("root", "qcow2", "20G"),
        },
        Installer: vmtest.InstallerBoot{
            HandoffManifest: "docs/internal/examples/minimal-install-manifest.json",
        },
        Assertions: []vmtest.Assertion{
            vmtest.SerialContains("Katl runtime reached systemd userspace"),
        },
        Timeout: 5 * time.Minute,
    })

    result.RequireSuccess(t)
}
```

The exact API can change during implementation, but the durable shape should
stay declarative: a scenario describes required artifacts, VM boots, disk
fixtures, manifest delivery, actions, assertions, and cleanup policy.

## Scenario Lifecycle

Each scenario run has these phases:

```text
Plan
  Normalize scenario options, allocate run id, reserve ports, select firmware,
  locate tools, compute artifact requirements, and write scenario.json.

BuildOrResolveArtifacts
  Reuse a valid artifact set or run the requested build commands. Record every
  input and output in artifacts.json.

PrepareFixtures
  Create writable target disks, copy OVMF vars, create an EFI boot tree when
  booting a UKI directly, and render install manifests into the run directory.

BootInstaller
  Start QEMU with deterministic serial capture. If local handoff is enabled,
  wait for the serial announcement, parse the token, and POST the manifest over
  host-forwarded user-mode networking.

ObserveInstall
  Wait for an install success signal, reboot signal, QEMU exit, or timeout.
  Preserve installer logs and handoff responses.

BootRuntime
  Start a second VM from the installed disk fixture. Use snapshots by default
  when assertions do not need to persist runtime mutations.

Assert
  Evaluate serial, journal, command, filesystem, HTTP, and Kubernetes assertions.
  Assertions should include enough context in failure messages to diagnose the
  guest state from preserved artifacts.

Collect
  Copy machine-readable results, serial logs, journal exports, QEMU command,
  firmware vars, manifest copies, and selected disk images into the run
  directory.

Cleanup
  Kill QEMU processes, wait for child commands, remove successful throwaway
  fixtures, and keep failed state unless the caller explicitly requested
  deletion.
```

Every phase records status transitions in `result.json` so an interrupted test
can be diagnosed without reading only terminal output.

## Artifact Build And Cache

The first artifact source is the existing mkosi path:

```text
scripts/mkosi build-installer
scripts/mkosi build-runtime
scripts/mkosi build-kubernetes-sysext
scripts/mkosi-artifacts write build/mkosi/artifacts.json
```

The Go harness should read the artifact index instead of scraping filenames.
The index should identify at least:

```text
installer UKI
runtime SquashFS root artifact
runtime metadata
installed ESP artifact tree, when a scenario starts from a prepared disk
Kubernetes sysext image and metadata
generated confext artifacts
manifest templates or rendered manifests
```

Artifact cache keys should include:

```text
git commit or dirty-tree marker
mkosi profile name
profile inputs and package manifests
relevant environment overrides
target architecture
artifact digest and size
```

The first implementation can use the existing `build/mkosi/artifacts.json` as a
single mutable cache. The later harness should allow scenarios to pin an
artifact set by digest so an update test can boot generation A, build generation
B, and prove slot switching without confusing the two sets.

Tests must never rebuild artifacts implicitly after a scenario has started. A
scenario plan resolves all required artifacts before QEMU boots, records their
digests, and passes immutable paths into later phases.

The cross-suite resource preparation contract is defined in
`docs/internal/deterministic-resource-testing.md`. VM scenarios should consume
the resource manifest produced by that layer when they run under the standard
heavy-test command, while direct developer invocations may continue to pass
explicit fixture paths for focused debugging.

The hermetic world execution model is defined in
`docs/internal/hermetic-vmtest-worlds.md`. That document narrows the standard VM
contract further: `scripts/vmtest-run` creates a tmpdir world and injects it
with `go test -exec`; tests allocate their own nodes and guest addresses inside
the world instead of receiving per-scenario fixture paths and IP addresses from
the developer.

Cluster VM scenarios should live in a VM integration package, not under
end-user command packages such as `cmd/katlctl`. A scenario may execute the
built `katlctl` binary as a black-box command when the CLI workflow is the
behavior under test.

## Disk Fixtures

Disk fixtures are typed scenario inputs:

```text
BootImage
  read-only source disk or UKI boot tree for installer-image or runtime images

TargetDisk
  writable disk installed by katlos-install; qcow2 by default, raw when a test
  needs byte-for-byte block inspection

ExtraDisk
  writable non-root data disk selected by manifest for mount and formatting
  tests

SnapshotDisk
  copy-on-write overlay used to run destructive assertions without changing the
  underlying installed disk

CorruptDisk
  derived fixture for rollback and repair tests, such as invalid boot metadata,
  broken candidate root slot, or missing generation state
```

The fixture API should expose stable device identity to manifests. QEMU virtio
disks have predictable order inside a scenario, but destructive install tests
should exercise Katl's stable selectors. The harness should therefore provide
helpers that map a disk fixture to the by-id path expected in the guest, and the
manifest renderer should consume that mapping.

Successful runs may delete throwaway target disks by default. Failed runs keep
all target disks unless the test explicitly marks them disposable. Long-running
scenario suites should support a `-katl.vmtest.keep=never|failed|always` flag.

Offline disk inspection is a later helper. It may use `qemu-nbd`, guestfish, or
loop devices only behind explicit host prerequisite checks because those tools
may need privileges or kernel modules. First implementation assertions should
prefer guest-side commands and serial/journal evidence.

## Manifest Delivery

Local handoff is the first installer manifest delivery mechanism. The harness
must:

```text
boot installer-image without a preseeded manifest
watch serial for the local handoff URL and one-time token
POST the scenario manifest to /v1/install over a host-forwarded port
record the announcement, token redacted where practical, request path, response,
and HTTP status
fail before destructive assertions if handoff is not reached in time
```

The manifest source should be either a committed fixture or a generated manifest
from typed scenario data. Generated manifests are preferable for disk matrix
tests because the target disk selector, extra disks, artifact URLs, and digests
must match the scenario fixtures.

Later scenarios may add preseeded manifest paths and network URL manifests, but
they should reuse the same install manifest schema and `katlos-install` input
contract.

## QEMU And OVMF Execution

The required execution backend is direct QEMU:

```text
qemu-system-x86_64
-machine q35,accel=<kvm|tcg>
OVMF pflash code image
per-run writable OVMF vars copy
virtio block devices
virtio network on QEMU user-mode NAT for single-node tests
virtio-vsock device for post-boot structured guest control
hostfwd ports for installer handoff, SSH, and later kube-apiserver checks
serial written to a stable file and streamed to assertions
display disabled by default
```

KVM policy is:

```text
on
  require /dev/kvm and fail during host prerequisite checks if unavailable

auto
  use KVM when visible, otherwise use TCG and record the slower mode

off
  force TCG for reproducibility or constrained environments
```

OVMF discovery should prefer explicit configuration:

```text
KATL_OVMF_CODE
KATL_OVMF_VARS
test flags or scenario fields
PATH-discovered distro defaults
```

The harness should not commit host-specific firmware paths. It should record the
resolved paths in the run directory for diagnostics.

Each VM process gets a context with a phase timeout. On cancellation the runner
sends a graceful termination signal, waits briefly, then kills the process. The
result should distinguish assertion failure, timeout, QEMU early exit,
prerequisite failure, build failure, and cleanup failure.

Vsock device setup should be explicit in the QEMU command when a scenario uses
guest control operations:

```text
-device vhost-vsock-pci,id=vsock0,guest-cid=<cid>
```

Guest CIDs must be allocated per run and must not be hard-coded into committed
fixtures. The allocator should reserve from a high local range derived from the
run id plus a process-local collision check, and it should record the selected
CID in `scenario.json` and `result.json`. Parallel tests that cannot reserve a
CID must fail during planning before QEMU starts, not after guest boot. The
well-known guest service port is `10240`; tests should treat it as a Katl
vmtest agent port, not as product runtime API.

## Guest Interaction

Serial log matching is the first assertion transport because it works before
networking, SSH, or the Kubernetes API exists.

Serial remains the readiness and failure channel. A VM must first emit the
phase-specific serial signal, such as installer-ready or runtime-ready, before
the harness attempts structured guest control. Vsock is the post-boot control
channel for bounded operations that need request/response semantics after the
guest agent is running. If the agent never starts, the scenario fails with the
serial log, QEMU command, firmware vars copy, and any disk/ESP artifacts already
preserved; it must not silently fall back to SSH or HTTP for the same assertion.

The next transports should be added in this order:

```text
journal export
  guest units write bounded status to the journal; the harness retrieves it
  through SSH or a guest-side collector when available

SSH command
  installed runtime exposes key-only access for the `katl` user; tests run
  bounded commands such as systemctl, findmnt, kubeadm, and kubectl

HTTP over hostfwd
  installer handoff, kube-apiserver /readyz, and test-only service probes

guest artifact collector
  optional Katl test unit writes selected logs and command results under
  /var/lib/katl/test-artifacts for later retrieval
```

SSH should not be required for installer-image first boot. It becomes the
practical command transport after the installed runtime has the generated `katl`
account and authorized keys.

## Vsock Control Channel

The vmtest guest agent should listen on vsock port `10240` and expose a small
protobuf API with explicit stream framing. The protobuf package should be
`katl.vmtest.v1`, generated into `internal/vmtest/proto`, and kept private while
the harness API is still volatile. The first service surface should cover:

```text
Health
  prove the guest agent is responsive and return agent/runtime metadata

RunCommand
  run a bounded argv with explicit timeout, working directory, environment
  allowlist, exit status, stdout, stderr, and truncation markers

ReadFile
  read bounded files needed by assertions, with size limits and redaction hints

ExportJournal
  return bounded journal text or export records for selected units and boot ids
```

The wire format is length-prefixed protobuf messages over the vsock byte stream:

```text
uint32 big-endian frame length
VmtestRequest protobuf bytes
uint32 big-endian frame length
VmtestResponse protobuf bytes
```

Each request carries a request id, operation timeout, and operation-specific
payload. Each response echoes the request id and returns exactly one of success
payload or structured error. Error responses include a stable code, human
message, retryable flag, and optional operation details. Transport errors,
malformed frames, unknown methods, protobuf decode failures, request timeouts,
and guest command failures must be distinguishable in `result.json`.

The host runner owns scenario and phase timeouts. The guest agent owns
per-operation deadlines after it accepts a request. When either side times out,
the host closes the vsock stream, records the partial artifact if one exists,
and fails the current assertion with enough context to debug from preserved run
artifacts. Large responses must be bounded by request limits; output over the
limit is truncated with explicit byte counts rather than streamed without
limits.

The guest agent is test infrastructure. It may run from a test-only systemd unit
or sysext/confext fixture, but it must not become a required Katl runtime
service. Scenarios that need vsock must declare the agent prerequisite so
ordinary boot smoke tests can keep using serial only.

## Assertions

Assertions are typed and phase-aware. Initial assertion types:

```text
SerialContains(text)
SerialRegex(pattern)
QEMUExited(code)
FileExists(path)
CommandSucceeds(argv, timeout)
SystemdUnitActive(name)
MountActive(path, source)
JournalContains(unit, text)
HTTPReady(url, status)
KubeReadyz(kubeconfig, endpoint)
```

Assertions must avoid hiding product policy in the harness. For example, a
scenario may assert that `/etc/kubernetes` is mounted before kubelet, but the
mount unit and ordering are product artifacts generated by Katl, not test-only
guest mutation.

Kubernetes assertions should start at the handoff boundary:

```text
kubeadm version is available from the selected sysext
containerd is active
/etc/kubernetes is writable and persistent
kubeadm preflight or dry-run succeeds
kubeadm init creates admin.conf and static pod manifests
kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw=/readyz succeeds
```

Applying a CNI for multi-node or workload tests is a harness fixture, not a Katl
runtime feature. Such tests must be named and documented as cluster-level
verification beyond the kubeadm-ready boundary.

## Logs And Artifacts

Every scenario run writes a directory like:

```text
build/vmtest/<run-id>/
  scenario.json
  result.json
  artifacts.json
  qemu/
    installer-command.txt
    runtime-command.txt
    installer-serial.log
    runtime-serial.log
    vsock-transcript.jsonl
    OVMF_VARS.fd
  manifests/
    install-manifest.json
    handoff-response.json
  disks/
    target.qcow2
    extra-0.qcow2
  guest/
    journal.txt
    systemctl.txt
    findmnt.txt
    kubeadm-output.txt
    kubectl-output.txt
```

The exact set depends on scenario phase and available transports. The harness
should also print the run directory on failure, matching the current smoke
script behavior.

Serial logs should be streamed to disk while QEMU runs. Tests may tee selected
lines to `testing.T.Log`, but complete logs belong in the run directory.

Journal collection should prefer structured export when available. Plain text
is acceptable for the first implementation if it is bounded and clearly tied to
the failing phase.

Vsock requests and responses should be recorded as JSONL summaries in the run
directory. Summaries should include request ids, method names, timing, byte
counts, exit status, and error codes. They must not record secrets by default:
authorization tokens, kubeconfigs, private keys, and command output marked
sensitive are redacted in transcripts while full sensitive payloads are omitted
unless a scenario explicitly opts into preserving them for a local debug run.

## Host Prerequisites

The harness should check prerequisites before building or booting:

```text
go test binary has permission to create files under the state root
qemu-system-x86_64 is in PATH
qemu-img is in PATH when creating disk fixtures
OVMF code and vars images are readable
/dev/kvm exists and is accessible when KVM is required
host kernel supports AF_VSOCK and QEMU exposes vhost-vsock when vsock is used
curl or Go HTTP client can reach host-forwarded handoff endpoints
mkosi and supporting build tools exist when the scenario requests builds
SSH client support exists when the scenario uses SSH assertions
kubectl exists in the guest or selected artifact when Kubernetes assertions use it
guest image includes and enables the vmtest agent when vsock assertions are used
```

Prerequisite failures should skip tests only when the test is explicitly marked
as host-dependent and the developer requested skip-on-missing behavior. In CI or
required validation, missing prerequisites should fail loudly.

Useful test flags:

```text
-katl.vmtest.run
  opt in to VM scenarios from `go test`

-katl.vmtest.state-root
  override build/vmtest

-katl.vmtest.keep
  never, failed, or always

-katl.vmtest.kvm
  auto, on, or off

-katl.vmtest.build
  never, missing, or always

-katl.vmtest.timeout-scale
  multiplier for slow TCG hosts
```

By default, ordinary `go test ./...` should run unit tests and skip VM scenarios
unless `-katl.vmtest.run` or a focused integration test command enables them.

## Nspawn Userspace Checks

Some generated filesystem and systemd checks can run through `systemd-nspawn`
before a full QEMU boot. These checks use `internal/nspawntest` and require an
explicit prepared Katl or Fedora userspace root or image via `KATL_NSPAWN_ROOT`,
`KATL_NSPAWN_IMAGE`, `-katl.nspawn.root`, or `-katl.nspawn.image`.

Generated unit trees should be mounted read-only into the container and verified
with the userspace root's `systemd-analyze`, for example by enabling
`KATL_NSPAWN_RUN=1` for the generated state unit smoke. Missing nspawn,
privileges, or prepared userspace roots should be reported as explicit skips for
developer preflights.

Host `systemd-analyze --root` remains a useful fallback for narrow local unit
syntax checks when `KATL_VERIFY_SYSTEMD_UNITS=1` is set, but nspawn is preferred
when available because it uses the same systemd userspace selected for runtime
checks. Neither path replaces QEMU tests for boot selection, live service restart
behavior, network continuity, kubelet startup, reboot rollback, disk layout, or
kubeadm cluster behavior.

Runtime configuration apply checks can also run through nspawn by binding a
prepared runtime fixture and a test-only local request helper into the userspace
root. This verifies request decoding, fail-closed audit persistence, absence of
partial generation output on rejected input, generation-scoped confext rendering
on accepted input, and preservation of selected Kubernetes sysext metadata. QEMU
still owns assertions that require a real booted node: boot selection, service
restart effects, network continuity during live apply, kubelet startup, reboot
rollback, and kubeadm cluster mutation boundaries.

## Timeouts And Failure Behavior

Timeouts are explicit at three levels:

```text
scenario timeout
  maximum wall time for the complete test

phase timeout
  build, installer boot, handoff, install, runtime boot, assertion, cleanup

operation timeout
  individual HTTP requests, SSH commands, serial waits, and journal collection
```

Timeouts should produce structured failure records with:

```text
phase
operation
duration
expected signal or command
QEMU process state
last serial lines
artifact directory
cleanup result
```

The runner must not leave QEMU processes running after a test returns. Cleanup
failures should be reported even when the main assertion already failed, but
they should not delete preserved artifacts.

When a scenario modifies a disk and then fails, the default is to keep the disk.
When a scenario succeeds, the default is to delete throwaway disks and keep only
the result summary and selected logs. Developers can override this with the keep
flag.

## Incremental Scenario Matrix

Build the matrix in stages so each step proves one new contract.

Stage 1: preserve existing smoke coverage.

```text
installer UKI boots and prints the installer-ready signal
runtime artifact is present in the mkosi artifact index
prepared installed disk boots and prints the runtime-ready signal
```

Stage 2: first install to target disk.

```text
installer boots through QEMU/OVMF
local handoff accepts a generated manifest
target disk is partitioned and receives root-a, ESP, state, and boot metadata
runtime boots from the installed disk
serial proves runtime systemd userspace
```

Stage 3: writable state and kubeadm-ready runtime.

```text
/var is mounted from the state partition
/etc/kubernetes is projected from /var/lib/katl/kubernetes/etc-kubernetes
containerd is active
Kubernetes sysext is active for the selected generation
katl-kubeadm-ready.target is reached
kubeadm preflight or dry-run evidence is captured
```

Stage 4: single-node kubeadm init API smoke.

```text
kubeadm init runs with Katl-rendered input under /etc/katl
/etc/kubernetes/admin.conf persists
kube-apiserver static pod starts
kubectl get --raw=/readyz reaches the API server
no CNI or workload scheduling success is required for this smoke
```

Stage 5: disk and install variants.

```text
target disk selector mismatch fails before mutation
extra data disk is formatted and mounted at an allowed path
reserved extra disk mount paths are rejected
optional dedicated /var/lib/etcd partition mounts before kubeadm automation
partial install rerun either resumes, repairs, or refuses deterministically
```

Stage 6: KatlOS update and rollback.

```text
generation A installs to root-a and becomes known good
generation B writes root-b and boots as a trial
successful boot health promotes generation B
failed boot health rolls back to generation A
root, sysext, and confext selection move as one generation
```

Stage 7: Kubernetes and config upgrades.

```text
Kubernetes sysext upgrades while KatlOS root remains compatible
KatlOS root upgrades while Kubernetes sysext remains compatible
generated confext changes apply as a new configuration generation
incompatible artifact pairs fail validation before becoming bootable
```

Stage 8: multi-node Kubernetes tests.

```text
one control-plane VM runs kubeadm init
worker VM installs and joins with kubeadm join
test-only network topology supports node-to-node traffic
test-only CNI fixture is applied when the scenario needs pod networking
kubectl sees expected nodes ready or bounded known states
```

Multi-node networking can start with direct QEMU networking that is sufficient
for kubeadm join. Libvirt networks may be added when direct QEMU setup becomes
too brittle, but that is a later backend decision, not the first harness
contract.

## Relationship To Existing Scripts

`scripts/katl-vm` already proves the direct-QEMU shape: OVMF pflash, serial log,
KVM auto detection, target disk creation, host port forwarding, local handoff,
timeout handling, and artifact paths. The Go harness should port these behaviors
instead of changing the boot model.

`scripts/check-mkosi-smoke` already records a run directory and artifact index
for build plus installer boot. The Go harness should keep the same diagnostic
principle while replacing shell status with `result.json`.

`scripts/check-installed-disk-smoke` already treats ESP files, loader entries,
serial logs, and installed disks as artifacts. The Go harness should reuse that
contract for runtime boot scenarios and then add guest assertions.

## Open Questions

1. Should the first Go scenario package live under `internal/vmtest/scenarios`
   or `test/integration`?

   Initial recommendation: start under `internal/vmtest/scenarios` while the API
   is private and volatile. Move to `test/integration` only when the package
   becomes a stable developer-facing test surface.

2. Should the Go runner call `scripts/katl-vm` first or immediately construct
   QEMU commands itself?

   Initial recommendation: call the script only for a quick compatibility
   bridge if it reduces risk. The durable implementation should construct QEMU
   commands in Go because process control, port allocation, timeouts, and
   assertions are core harness logic.

3. How should journal artifacts be collected before SSH is reliable?

   Initial recommendation: use serial and deterministic systemd status signals
   first. Add a guest-side artifact collector or SSH collection once the
   installed runtime account and network path are stable.

4. What is the first multi-node network backend?

   Initial recommendation: defer until single-node kubeadm init is reliable.
   Start with direct QEMU networking if it can support kubeadm join without host
   network privileges; add libvirt only when the direct backend becomes the
   limiting factor.
