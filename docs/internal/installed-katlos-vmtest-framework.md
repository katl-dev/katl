# Installed KatlOS VM Test Framework

Status: proposed technical design.

This document defines the fast VM test path for Katl behavior that needs a real
installed KatlOS node but does not need to test the installer itself.

## Problem

The full installer boot loop is too expensive as the default feedback path for
runtime, `katlc`, bootstrap, kubeadm, and multi-node VM tests. Direct runtime
tests avoid that cost by booting the mkosi runtime squashfs directly, but they
do not represent an installed node: they use volatile state, override the boot
path, and mask installed-state units.

The desired default for non-installer VM tests is a fast boot into an installed
KatlOS node. The first install should be paid once for a content-identical set
of inputs, then later tests should boot isolated snapshots of that installed
baseline.

## Decision

Use an installed KatlOS generation-0 fixture as the primary VM test substrate.

The VM test stack has three tiers:

```text
direct-runtime-volatile
  Boot the real runtime squashfs directly with volatile state. This is only an
  image reachability and vmtest-agent smoke tier.

installed-katlos
  Boot a per-test qcow2 overlay whose backing image is an immutable, cached,
  first-install-produced KatlOS generation-0 fixture. This is the default for
  runtime, katlc, operation, bootstrap, kubeadm-ready, and multi-node tests.

first-install
  Run the installer and first runtime handoff. This is required only for tests
  that prove installer behavior, disk layout, ESP contents, bootloader entries,
  boot selection, and the production of installed KatlOS fixtures.
```

Direct runtime tests remain useful, but they must not assert generation state,
installed boot health, `/var/lib/katl` layout, rollback, kubeadm state, or
bootstrap behavior. Those assertions belong to `installed-katlos` or
`first-install`.

## Existing Baseline

The current code already has the start of this path:

```text
ProduceFirstInstallRuntimeFixture
  runs first install and packages an installed runtime disk plus ESP artifacts

FindPublishedFirstInstallRuntimeFixtureInBuildRoots
  discovers published fixtures under the VM test cache

RunInstalledRuntime
  boots an installed disk with ImageSnapshot=true so each run is isolated from
  the source fixture
```

The missing pieces are cache identity, fixture readiness, cheap cloning, and a
guest-side inspection contract rich enough for tests to avoid serial scraping.

## Goals

The framework should:

```text
boot real installed KatlOS state for most VM tests
avoid rerunning first install unless fixture inputs changed
keep cached base fixtures immutable
give each scenario a private writable overlay
make stale or missing fixtures setup failures, not silent skips
record enough provenance to explain why a fixture was reused
support multi-node tests by cloning one base fixture per node role/spec
use vmtest-agent or katlc-facing APIs for guest assertions instead of SSH
```

## Non-Goals

This design does not:

```text
replace first-install tests
turn direct runtime tests into installed-state tests
introduce synthetic generation state as the main path
make katlc a Talos-style reconcile controller
require SSH from tests or from katlctl
solve package repository locking beyond recording the relevant artifact inputs
```

## Fixture Identity

Published installed KatlOS fixtures must be selected by content identity, not by
"newest fixture for node name and role".

The fixture key should include:

```text
node name and role
runtime root squashfs digest
KatlOS install image digest
installer boot artifact digest
ESP artifact tree digest when externally supplied
install manifest digest
node metadata digest
mkosi artifact index digest
generation metadata schema version
install-state schema version
disk layout schema version
vmtest fixture schema version
relevant feature flags such as UseInstalledESP
```

The resource manifest's build-input and package inventory identity should be
retained with the fixture for diagnosis. Fixture reuse is keyed by produced
artifact digests and relevant public schema versions, not by a committed
transitive package closure. The key also marks whether the source tree was
dirty.

Fixture lookup must fail with `setup-failed` when no fixture matches the
requested key. It may then run the fixture producer if the selected suite allows
fixture production.

## Published Manifest

The durable cache should store fixtures under `_build/vmtest` by fixture key:

```text
_build/vmtest/installed-katlos/<key>/
  installed-katlos-fixture.json
  base.qcow2
  esp/
  node.json
  readiness.json
```

The manifest should bind all mutable inputs and readiness evidence:

```json
{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "InstalledKatlOSVMTestFixture",
  "key": "sha256:<fixture-key>",
  "nodeName": "cp-1",
  "systemRole": "control-plane",
  "disk": {
    "path": "base.qcow2",
    "format": "qcow2",
    "sha256": "<base-disk-sha256>",
    "virtualSizeBytes": 34359738368
  },
  "espArtifacts": {
    "path": "esp",
    "treeSHA256": "<esp-tree-sha256>"
  },
  "nodeMetadata": {
    "path": "node.json",
    "sha256": "<node-metadata-sha256>"
  },
  "inputs": {
    "runtimeRootSHA256": "<sha256>",
    "installManifestSHA256": "<sha256>",
    "mkosiArtifactIndexSHA256": "<sha256>",
    "generationSchemaVersion": "v1alpha1",
    "installStateSchemaVersion": "v1alpha1",
    "diskLayoutSchemaVersion": "v1alpha1"
  },
  "readiness": {
    "path": "readiness.json",
    "sha256": "<readiness-sha256>"
  }
}
```

The exact schema should live in Go first and may later be mirrored under
`docs/internal/schemas` when external tooling needs it.

## Readiness Contract

A cached installed KatlOS fixture is reusable only after it proves the clean
generation-0 handoff contract:

```text
installed runtime booted from the installed disk path
state partition mounted at /var
/var/lib/katl install records exist and validate
generation 0 spec and status exist and validate
generation 0 reached katl-boot-complete.target
machine identity matches boot metadata and /var/lib/katl identity state
katlc binary is installed
katlc-agent.service startup audit completed
node is waiting for cluster bootstrap
no Kubernetes sysext is selected for generation 0
kubelet is disabled, inactive, or absent
kubeadm-owned /etc/kubernetes, kubelet, and etcd state is absent
```

The fixture producer should capture this as `readiness.json` before publishing
the base fixture. Consumers should validate that the readiness record exists and
matches the manifest before booting overlays.

## Scenario Execution

An `installed-katlos` scenario should run as:

```text
compute requested fixture key from the world and node spec
find or produce the matching installed KatlOS base fixture
copy ESP artifacts into the scenario directory
inject vmtest-only boot options into the copied ESP
create a scenario-local qcow2 overlay backed by base.qcow2
boot the VM from the overlay and copied ESP
wait for vmtest-agent health and the scenario-specific readiness signal
run assertions through the guest test API
destroy the VM and delete or preserve the overlay according to Keep policy
```

The cached `base.qcow2` must never be opened writable by a scenario. Tests that
mutate node state mutate only their overlay. Promoting a scenario overlay into a
new cache fixture is forbidden; only the first-install fixture producer writes
published installed KatlOS fixtures.

## Guest Test API

`katl-vmtest-agent` currently proves health. The installed KatlOS framework
needs a bounded inspection API so tests can make assertions without SSH or
serial scraping.

Minimum useful methods:

```text
Health
  report that the agent is running

WaitSystemdUnit
  wait for a unit or target to become active, failed, inactive, or timed out

SystemdUnitStatus
  return active state, sub state, result, and selected timestamps

ReadFile
  read allowlisted files under /etc, /run, /var/lib/katl, and kubeadm state

StatPath
  report existence, type, mode, and size for allowlisted paths

Journal
  return bounded journal excerpts for selected units

KatlStatus
  return parsed generation, boot, operation, and install summaries
```

Arbitrary shell execution should not be the normal assertion interface. If a
debug command method is added, it should be explicitly marked as diagnostic and
allowlisted so tests do not quietly become SSH-like orchestration.

## Test Ownership

Use `installed-katlos` for:

```text
katlc API availability and operation acceptance
generation activation behavior after install
operation record startup audit
bootstrap preflight and kubeadm input rendering
kubeadm-ready local prerequisites
single-node bootstrap smoke
multi-node bootstrap and stacked etcd flows
Kubernetes sysext activation
config apply modes that need installed state
```

Use `first-install` for:

```text
target disk partitioning and formatting
systemd-repart behavior
ESP contents and bootloader entries as produced by install
first runtime handoff from installer to generation 0
installed fixture production and readiness proof
repair of partially completed install state
```

Use `direct-runtime-volatile` only for:

```text
runtime squashfs, kernel, and initrd boot compatibility
baseline systemd userspace reachability
vmtest-agent smoke
presence of runtime-contained binaries or static files
```

## Failure Semantics

Failures before a scenario VM starts are setup failures:

```text
missing matching fixture key
stale manifest or digest mismatch
base fixture opened or modified unexpectedly
overlay creation failed
host lacks required libvirt, OVMF, image-tool, or vsock capability
readiness record missing or invalid
```

Failures after the VM reaches the scenario assertion phase are Katl behavior
failures:

```text
generation status invalid
katlc API unavailable
kubeadm-ready target failed
bootstrap operation failed
forbidden generation-0 Kubernetes state appeared
```

Enabled suites must not silently skip because a fixture was absent. Local runs
may classify host capability gaps as `host-skipped`; CI suites that advertise
the installed KatlOS framework should fail when those capabilities are absent.

## Cache Locking And Pruning

Fixture production must lock by fixture key:

```text
_build/vmtest/locks/installed-katlos/<key>.lock
```

Only one producer may build a key at a time. Other scenarios wait for the
published manifest or fail with the producer result.

The cache may keep multiple keys. Pruning should be explicit at first, for
example:

```text
scripts/vmtest-run --prune-cache
```

or a later Go command. Automatic pruning should not delete a fixture that is
currently locked or referenced by an active world.

## Migration Plan

1. Define the installed KatlOS fixture key and manifest types in Go, with golden
   tests for stable key generation.
2. Replace newest-by-node fixture selection with exact key lookup.
3. Publish first-install fixtures under `_build/vmtest/installed-katlos/<key>`.
4. Create per-scenario qcow2 overlays from the cached base fixture without
   copying or rehashing the full disk on every scenario.
5. Add the generation-0 readiness probe and `readiness.json` publication.
6. Extend `katl-vmtest-agent` with the bounded inspection API.
7. Convert installed runtime and bootstrap tests to consume installed KatlOS
   overlays by default.
8. Keep direct runtime tests as a narrow smoke tier and reject installed-state
   assertions from that tier by helper API and review.
9. Make fixture production a separate gate so agents can refresh the installed
   KatlOS cache before running focused scenario tests.

## Validation Gates

Implementation should add or run:

```text
go test ./internal/vmtest
go test ./internal/vmtest/scenarios
go test ./...
scripts/vmtest-run ./internal/vmtest -run 'FirstInstall.*Fixture|InstalledRuntime' -count=1
scripts/vmtest-run ./internal/vmtest/scenarios -run 'TwoNode|ThreeControlPlane' -count=1
```

When the host cannot run VM tests, the handoff must record the exact skipped
`scripts/vmtest-run` commands and the missing host capability.

## Open Questions

1. Should fixture keys include the dirty Git tree marker only as provenance, or
   should dirty trees always produce unique fixture keys?

   Initial recommendation: include the dirty marker in the key for agent and CI
   runs. Developers can opt into reusing dirty-tree fixtures only with an
   explicit override.

2. Should the base fixture disk be stored compressed?

   Initial recommendation: keep the canonical base as qcow2 for cheap backing
   overlays. Add optional export compression later for artifact upload.

3. Should vmtest-agent inspection use the same gRPC transport as katlc?

   Initial recommendation: keep vmtest-agent as test-only guest instrumentation.
   It may share libraries and protobuf conventions, but it should not become a
   supported product API.

4. Should fixture production be automatic for every installed-katlos test?

   Initial recommendation: automatic locally when `scripts/vmtest-run` has the
   default artifact set and host capabilities; explicit in CI so fixture
   production and fixture consumption can be timed and diagnosed separately.
