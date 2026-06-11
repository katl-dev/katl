# Deterministic Resource Testing

Status: proposed design.

Katl has unit tests, nspawn userspace checks, and VM scenarios that all need
different setup. The heavy tests should be agent-runnable without a developer
hand-exporting fixture paths. When an enabled scenario reaches its assertions, a
failure should point at the Katl behavior under test. Resource preparation,
stale artifacts, package drift, and host capability gaps should be classified
before that point.

## Target Outcome

The standard heavy-test entrypoint should:

```text
check host capabilities
build or reuse locked mkosi artifacts
generate deterministic install and node fixtures
prepare nspawn userspace roots or images
run first-install VM setup to publish installed-runtime fixtures
run installed-runtime, config-apply, kubeadm, and multi-node scenarios
summarize passed, failed, host-skipped, and setup-failed scenarios
```

The command should emit machine-readable artifacts under `build/resource-tests/`
and exit nonzero when an enabled scenario was skipped because a repo-owned
resource was missing or stale.

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
Tests must not rebuild or rediscover artifacts after a VM or nspawn scenario
starts.

## Build Inputs

Mkosi remains the image builder. The deterministic resource layer should add a
lock and verification step around mkosi rather than replace the builder.

The first lock can be a JSON document generated from the build result:

```text
build locks
  committed source of expected mkosi profiles, package repositories, package
  names, selected versions, and checksums

build manifests
  per-run records under build/resource-tests/<run-id>/ that name the actual
  mkosi outputs, package set, tool versions, and content digests
```

The heavy-test command should support two modes:

```text
strict
  used by agents and CI; package drift, missing lock records, and stale artifacts
  fail during resource preparation

refresh
  used by maintainers to intentionally update the package lock and review the
  package/artifact diff
```

Fedora and Kubernetes repository updates should therefore show up as an explicit
lock refresh with a reviewable package and artifact diff.

The first package-lock verifier lives in `internal/resourcetest`. It consumes a
`kind: ResourcePackageLock` document and a generated resource manifest. The lock
names mkosi profile paths, profile config digests, package repository identities,
selected package NEVRAs, optional package checksums, and mkosi/tool versions.
Generated package sets in `ResourceTestManifest` carry `lockSHA256`, the digest
of the lock source used for the run.

Strict verification fails before scenario assertions when:

```text
the package lock is missing profile, repository, or package records
a generated mkosi profile points at a different path, config digest, or package set
a generated package set omits the package-lock digest
the package-lock digest does not match the lock used by the run
selected package NEVRAs or package checksums drift from the lock
generated package sets contain packages not present in the lock
```

Refresh mode should regenerate the lock from a reviewed mkosi build and then
rerun strict verification so the resource manifest records the new lock digest.
The initial command surface is:

```text
katl-resource-lock prepare-mkosi --manifest build/resource-tests/<run-id>/manifest.json --mode strict
katl-resource-lock add-artifact --manifest build/resource-tests/<run-id>/manifest.json --name runtime-root --kind squashfs --path build/mkosi/katl-runtime-root.squashfs
katl-resource-lock add-rpm-package-set --manifest build/resource-tests/<run-id>/manifest.json --name runtime --root build/mkosi/katl-runtime-root
katl-resource-lock refresh --manifest build/resource-tests/<run-id>/manifest.json
katl-resource-lock verify --manifest build/resource-tests/<run-id>/manifest.json
```

The lock commands default to `mkosi.profiles/resource-package-lock.json`. The refresh
command writes that lock from the manifest's generated mkosi profile and package
records and prints the lock digest. The resource-test preparation path should
use `prepare-mkosi` for the standard build outputs. It records runtime RPMs from
the runtime root, installer RPMs from the package-set TSV emitted during
installer mkosi builds, and Kubernetes package identities from the sysext
metadata when `katl-kubernetes.raw.json` is present. For the KatlOS install
image, it reads the embedded image index and locks the component artifact
checksums plus component package identities. It also records the mkosi version
and profile config digests for the package-producing profiles so strict mode
catches profile or tool drift before scenario execution. The lower-level
`add-artifact` and `add-rpm-package-set` commands remain available for focused
resource preparation and custom suites.

## Resource Graph

The resource graph should be typed and content-addressed enough that downstream
scenarios can reuse outputs safely:

```text
toolchain
  Go, mkosi, libvirt, systemd-nspawn, systemd-analyze, image tooling, OVMF

mkosi artifacts
  installer UKI or kernel/initrd pair, runtime root, KatlOS install image,
  Kubernetes sysext image, and generated artifact index

node inputs
  cp-1, worker-1, cp-2, cp-3 metadata, install manifests, target disk selectors,
  deterministic addresses, and optional network topology

nspawn fixtures
  prepared userspace root or image with the selected systemd userspace and Katl
  runtime/config artifacts mounted or copied according to the scenario contract

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
  the host lacks a declared optional capability such as systemd-nspawn
  privileges, libvirt, image tooling, OVMF, KVM, or vhost-vsock

disabled
  the scenario was outside the selected suite
```

Enabled suites should treat generic Go test skips as failures unless the skip is
mapped to `host-skipped` by the resource-test summary. This prevents
`KATL_VMTEST_RUN=1` or `KATL_NSPAWN_RUN=1` from returning a green result while
all resource-backed tests skipped because fixtures were never prepared.

## Strict Summary

The first strict aggregation helper also lives in `internal/resourcetest`. It
consumes the resource manifest, Go test output, and nspawn or VM `result.json`
artifacts. It writes `kind: ResourceTestSummary` with per-scenario status
counts, scenario run directories, failure summaries, and Go test failures.
Callers may request `go test -json` output from `scripts/vmtest-run`, but JSON
event output is opt-in rather than the default runner contract.

Enabled scenarios classify as `setup-failed` when their result artifact is
missing, invalid, still `planned`, from another scenario or run, or when Go
reports a generic skip that is not backed by a declared missing host capability.
A skipped result with a populated `missing` prerequisite list classifies as
`host-skipped`. The summary exits nonzero when any scenario is `failed` or
`setup-failed`, or when the Go test JSON stream contains failures.

## Manifest Schema

The first typed schema lives in `internal/resourcetest`. It uses
`apiVersion: katl.dev/v1alpha1` and `kind: ResourceTestManifest`.

The manifest records:

```text
runID and creation time
git revision and dirty state
tool versions and optional tool digests
mkosi profile paths, config digests, and package-set references
package-set identity, package NEVRAs, and package checksums
host capability probe results
build artifacts with paths, sizes, and SHA-256 digests
fixtures with artifact references and optional fixture manifests
scenario result paths, run directories, fixture references, and required host capabilities
```

Scenario status values are:

```text
passed
failed
setup-failed
host-skipped
disabled
```

Generic Go test skips in an enabled resource suite classify as `setup-failed`
unless the resource-test layer has already mapped them to a declared missing
host capability. This keeps missing generated fixtures separate from real host
capability gaps.

## Standard Commands

The repo should grow conventional entrypoints, initially as thin scripts:

```text
scripts/check-resource-tests
scripts/vmtest-run
```

For nspawn and VM-backed suites, the stronger execution contract is the
hermetic world model in `docs/internal/hermetic-vmtest-worlds.md`:
`scripts/vmtest-run` creates a tmpdir world, exports the world environment,
then executes `go test` through `go test -exec`. Each test allocates its own
containers, VMs, and guest addresses inside that world. Resource checking remains
an internal setup concern, not a developer-facing requirement to pass fixture
paths and IP addresses.

`scripts/check-resource-tests` may delegate to `scripts/vmtest-run` for nspawn
and VM suites, but `scripts/vmtest-run` is the direct developer entrypoint for
those tests.

Responsibilities:

```text
preflight host capabilities and write host-capabilities.json
run mkosi builds through scripts/mkosi
write and verify the mkosi artifact index
generate node manifests and metadata under build/resource-tests/<run-id>/
prepare the nspawn userspace fixture
run first-install VM setup and publish installed-runtime fixtures
exec go test with the caller's arguments and resource-test strict mode
```

The script can call the existing mkosi wrapper and narrow Go helpers such as
`cmd/katl-mkosi-artifacts`. Existing fixture resolvers and publishers are
transitional; once world fixture factories exist, their policy should move
behind `scripts/vmtest-run` or be deleted. Argument interpretation, result
aggregation, lock validation, and scenario status classification belong in
separate Go tooling when they are needed; they should not be embedded in the
world setup wrapper.

## Test Layout

The normal test commands should keep distinct purposes:

```text
go test ./...
  pure unit, parser, planner, golden, and helper tests; no privileged resources

scripts/vmtest-run ./internal/vmtest -run Nspawn -count=1
  nspawn userspace and generated systemd/config checks

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
actions. It would still need local execution escapes for mkosi, nspawn, libvirt,
KVM, OVMF, and vhost-vsock. Fedora and Kubernetes package repository contents
still need an explicit lock. Package locks, fixture generation, and strict skip
classification are the immediate sources of determinism.

Revisit Bazel when these are true:

```text
resource inputs and outputs have stable schemas
mkosi package locks are enforced
resource tests have one strict command with reliable result summaries
CI spends enough time rebuilding unchanged artifacts that remote caching matters
privileged integration tests can be cleanly tagged as local-only actions
```

## Implementation Sequence

1. Define the resource-test manifest schema and status taxonomy.
2. Add strict result aggregation that fails on unexpected enabled-suite skips.
3. Add the self-provisioning nspawn fixture path.
4. Add the first-install installed-runtime fixture factory as the primary VM
   setup path.
5. Add package-set recording and strict lock verification around mkosi builds.
6. Generate deterministic multi-node fixture inputs from source-controlled
   templates.
7. Wire GitHub Actions to call the same command, with VM suites on runners that
   provide the declared host capabilities.

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

3. How much package drift is acceptable for local development?

   Initial recommendation: local refresh mode may build against fresh metadata,
   but strict mode should reject drift until the lock is intentionally updated.
