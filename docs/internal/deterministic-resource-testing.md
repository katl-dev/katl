# Deterministic Resource Testing

Status: proposed design.

Katl has unit tests and libvirt VM scenarios that need different setup. The
heavy tests should be agent-runnable without a developer hand-exporting fixture
paths. When an enabled scenario reaches its assertions, a failure should point at
the Katl behavior under test. Resource preparation, stale artifacts, package
drift, and host capability gaps should be classified before that point.

## Target Outcome

The standard heavy-test entrypoint should:

```text
check host capabilities
build or reuse locked mkosi artifacts
generate deterministic install and node fixtures
run first-install VM setup to publish installed-runtime fixtures
run installed-runtime, config-apply, kubeadm, and multi-node scenarios
summarize passed, failed, host-skipped, and setup-failed scenarios
```

The command should emit machine-readable artifacts under
`_build/resource-tests/` and exit nonzero when an enabled scenario was skipped
because a repo-owned resource was missing or stale.

## Determinism Boundary

This design uses deterministic to mean that every enabled scenario runs from
declared inputs whose identity is recorded before assertions start. It does not
require byte-for-byte reproducible release images in the first implementation,
though the same records are useful for later reproducibility work.

Inputs to record:

```text
git revision and dirty-tree marker
flake.lock revision for host tools
mkosi version and profile names
distribution release and enabled package repositories
resolved package NEVRAs and package checksums
Go module graph and generated binary digests
manifest templates and rendered manifest digests
installer, runtime, sysext, confext, disk, and ESP artifact digests
host capability probe results
```

Scenario execution should consume immutable paths from this resource manifest.
Tests must not rebuild or rediscover artifacts after a VM scenario starts.

## Resource Graph

The resource graph should be typed and content-addressed enough that downstream
scenarios can reuse outputs safely:

```text
toolchain
  Go, mkosi, libvirt, systemd-analyze, image tooling, OVMF

mkosi artifacts
  installer UKI or kernel/initrd pair, runtime root, KatlOS install image,
  Kubernetes sysext image, and generated artifact index

node inputs
  cp-1, worker-1, cp-2, cp-3 metadata, install manifests, target disk selectors,
  deterministic addresses, and optional network topology

first-install fixtures
  target disks produced by booting the installer and applying generated install
  manifests through the same handoff path used by the VM tests

installed-runtime fixtures
  packaged disks, ESP artifact trees, node metadata, and checksum-bound fixture
  manifests published from first-install runs
```

Manual fixture environment variables are transitional scaffolding for the
pre-hermetic VM path. Once `scripts/vmtest-run` provides world fixture
factories, VM-backed suites should not keep manually supplied fixture
environment as a supported path. Any still-needed cache selection should be
expressed through the world manifest and recorded with artifact digests.

## Failure Semantics

The result schema should distinguish scenario assertion failures from setup and
capability outcomes:

```text
passed
  setup completed and assertions passed

failed
  setup completed and an assertion about Katl behavior failed

setup-failed
  repo-owned resource generation failed, an artifact was stale, a digest did not
  match, a package lock check failed, or a required generated fixture was absent

host-skipped
  the host lacks a declared optional capability such as libvirt, image tooling,
  OVMF, KVM, or vhost-vsock

disabled
  the scenario was outside the selected suite
```

Enabled suites should treat generic Go test skips as failures unless the skip is
mapped to `host-skipped` by the resource-test summary. This prevents
`KATL_VMTEST_RUN=1` from returning a green result while all resource-backed
tests skipped because fixtures were never prepared.

## Standard Commands

The repo should keep conventional entrypoints:

```text
scripts/check-resource-tests
scripts/vmtest-run
```

For VM-backed suites, the stronger execution contract is the hermetic world
model in `docs/internal/hermetic-vmtest-worlds.md`: `scripts/vmtest-run`
creates a tmpdir world, exports the world environment, then executes `go test`
through `go test -exec`. Each test allocates its own VMs and guest addresses
inside that world. Resource checking remains an internal setup concern, not a
developer-facing requirement to pass fixture paths and IP addresses.

Responsibilities:

```text
preflight host capabilities and write host-capabilities.json
run mkosi builds through scripts/mkosi
write and verify the mkosi artifact index
generate node manifests and metadata under _build/resource-tests/<run-id>/
run first-install VM setup and publish installed-runtime fixtures
exec go test with the caller's arguments and resource-test strict mode
```

## Test Layout

The normal test commands should keep distinct purposes:

```text
go test ./...
  pure unit, parser, planner, golden, and helper tests; no privileged resources

scripts/vmtest-run ./internal/vmtest -count=1
  libvirt first-install and installed-runtime checks

scripts/vmtest-run ./internal/vmtest/scenarios -run 'TwoNode|ThreeControlPlane' -count=1
  multi-node kubeadm and stacked-etcd checks
```

Each suite can still be focused during development, but the command owns setup
and summary rules for the enabled suite.

## Bazel Decision

Use the mkosi plus resource-graph path first. The useful properties are an
explicit action graph, stable inputs, content digests, and cacheable outputs; the
project can implement those directly around the systemd-native build and test
tools already in use.

Bazel would add value after the graph stabilizes if Katl needs remote caching,
cross-repository build graph integration, or stronger sandboxing for pure
actions. It would still need local execution escapes for mkosi, libvirt, KVM,
OVMF, and vhost-vsock. Fedora and Kubernetes package repository contents still
need an explicit lock. Package locks, fixture generation, and strict skip
classification are the immediate sources of determinism.

## Implementation Sequence

1. Define the resource-test manifest schema and status taxonomy.
2. Add strict result aggregation that fails on unexpected enabled-suite skips.
3. Add the first-install installed-runtime fixture factory as the primary VM
   setup path.
4. Add package-set recording and strict lock verification around mkosi builds.
5. Generate deterministic multi-node fixture inputs from source-controlled
   templates.
6. Wire CI to call the same command, with VM suites on runners that provide the
   declared host capabilities.

## Open Questions

1. Should host capability gaps fail in CI instead of being reported as
   `host-skipped`?

   Initial recommendation: CI jobs that advertise a resource suite should fail
   when the required capability is absent. Local development can still report
   `host-skipped` for optional suites.

2. Should the package lock live next to mkosi profiles or under
   `docs/internal/schemas`?

   Initial recommendation: keep it near the mkosi profiles because it is an
   executable build input.
