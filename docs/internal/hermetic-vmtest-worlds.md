# Hermetic VM Test Worlds

Status: current implementation plan.

Katl VM and cluster tests create a temporary world, then run scenarios inside
it. The world is the host-side substrate for tests: scratch storage, host
capabilities, libvirt network selection, artifact locations, logging, and result
collection. Each Go test still owns the scenario it is proving, including
whether it starts one VM or a multi-node cluster inside the world.

This keeps the test contract conventional:

```text
scripts/vmtest-run
scripts/vmtest-run ./internal/vmtest/scenarios -run '^TestTwoNodeKubeadmJoinSmoke$'
```

Developers should not pass installed disks, ESP trees, node metadata, fixture
manifests, or per-node IP addresses for normal test runs. After a developer
installs the required host capabilities, `scripts/vmtest-run` should do
everything else needed for an enabled VM test: create the world, prepare shared
artifacts, set up configured host-side world resources, export the world
environment, and execute Go tests through `go test -exec`. The resulting Go test
process owns output and exit status.

## Design Goals

The hermetic runner should:

```text
create all mutable output under TMPDIR
print the run directory and keep durable VM test caches under _build/vmtest/
probe configured host capabilities before handing off to go test
verify libvirt VM networking before handing off to tests
record the selected libvirt network without inferring it from Go test arguments
provide a CIDR and gateway to tests, not per-scenario node addresses
build or resolve Katl artifacts before scenario assertions start
run Go test binaries inside the world with go test -exec
let tests write logs, manifests, and result files under the world directory
leave enabled-test skip classification to tests or later aggregation
record host capability gaps without interpreting Go test arguments
```

The world is test infrastructure, not a Katl product feature. It must not make
Katl responsible for site DHCP, PXE, cluster networking, Kubernetes add-ons, or
long-lived host network configuration.

## Output Layout

The standard runner creates a run directory below the process temporary
directory:

```text
${TMPDIR:-/tmp}/katl-vmtest/<run-id>/
  world.json
  host-capabilities.json
  artifacts/
  network/
  packages/
  scenarios/
```

Repo-local `build/` should not be the primary scratch root for hermetic runs.
Repo-local `_build/vmtest/` is the durable VM test cache root shared across
world runs. It may contain installed-runtime fixtures published from successful
first-install runs, and it may also contain durable pointers or small summaries
produced by separate aggregation tooling, for example:

```text
_build/vmtest/published-first-install-runtime/
_build/vmtest/fixtures/
_build/vmtest/latest -> ${TMPDIR:-/tmp}/katl-vmtest/<run-id>
_build/vmtest/latest-summary.json
```

The runner should print the absolute run directory before handing off to
`go test`, and `scripts/vmtest-exec` should print it again when a package test
binary exits nonzero. Cleanup and summary generation belong to separate tooling
when the project needs those workflows.

## World Manifest

The runner writes one `world.json` and exports its path to the test binary:

```text
KATL_VMTEST_WORLD_MANIFEST=${TMPDIR:-/tmp}/katl-vmtest/<run-id>/world.json
```

The manifest is the only required ambient input for hermetic VM tests:

```json
{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "VMTestWorld",
  "runID": "20260606T120000Z-abc123",
  "runDir": "/tmp/katl-vmtest/20260606T120000Z-abc123",
  "cacheDir": "/repo/_build/vmtest",
  "artifactDir": "/tmp/katl-vmtest/20260606T120000Z-abc123/artifacts",
  "scenarioDir": "/tmp/katl-vmtest/20260606T120000Z-abc123/scenarios",
  "libvirt": {
    "uri": "qemu:///system",
    "network": "default",
    "storagePool": "default",
    "storagePath": "/var/lib/libvirt/images",
    "domainPrefix": "katl-20260606T120000Z-abc123"
  },
  "network": {
    "backend": "libvirt",
    "name": "default",
    "cidr": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "leaseFile": "/tmp/katl-vmtest/20260606T120000Z-abc123/network/leases.json"
  },
  "capabilities": {
    "image-tool": "passed",
    "libvirt": "passed",
    "libvirt-network": "passed",
    "libvirt-storage-pool": "passed",
    "ovmf": "passed",
    "kvm": "passed",
    "vsock": "passed"
  }
}
```

The world provides the durable cache directory, selected libvirt network, CIDR,
gateway, and DHCP lease artifact. It should not encode that a two-node test
needs `cp-1` at one address and `worker-1` at another. That allocation belongs
to the scenario.

## Scenario Ownership

Inside a world, a Go test creates its own scenario. Scenario tests should live
in a VM integration package such as `internal/vmtest/scenarios` while the
harness API is internal and unstable. They should not live in end-user command
packages such as `cmd/katlctl`.

The scenario layer allocates addresses from the world CIDR, renders node
metadata, creates install manifests, creates or reuses installed-runtime
fixtures, starts VMs, and records scenario-local contracts. Parallel scenarios
coordinate through the world lease file instead of hard-coded addresses.

This boundary keeps the harness generic. The harness sets up the world; tests
decide what lives in it.

## Network Model

The VM network backend is libvirt. Kubeadm join needs guest-to-guest
reachability, so the runner verifies that the selected libvirt network exists,
is active, and has an address configuration it can record:

```text
connect to the configured libvirt URI
verify the selected libvirt network is active
discover the network CIDR and gateway
write the network, CIDR, gateway, and lease file to world.json
record DHCP leases for scenario inspection
```

Developers may provide a libvirt network override for local debugging, but
normal runs use the default libvirt network. Tests should request leases from
the world rather than receiving addresses through environment variables.

If the host cannot provide the selected libvirt URI, network, or storage pool,
the world setup result is a host capability gap. A required CI suite should
fail; an optional local suite may report `host-skipped`.

## go test -exec

`scripts/vmtest-run` is a thin world setup wrapper over `go test`. Its arguments
are Go package patterns and `go test` flags:

```sh
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run '^TestTwoNodeKubeadmJoinSmoke$' -count=1
```

The wrapper injects the VM world by preparing the world environment and ending
with the equivalent `go test -exec` invocation:

```sh
go test -exec scripts/vmtest-exec ./internal/vmtest/scenarios \
  -run '^TestTwoNodeKubeadmJoinSmoke$' -count=1
```

`scripts/vmtest-exec` is an implementation detail of `scripts/vmtest-run`, not
a second developer entrypoint. It should fail when invoked without a world
created by the runner.

Package selection, `-run`, `-count`, `-timeout`, and other ordinary test controls
remain `go test` flags with Go's usual meaning. Harness controls should use
environment variables or a separate setup command so the runner does not need to
parse the Go test argument stream.

VM suites should use `-count=1`; callers or higher-level check commands should
pass that flag explicitly because `scripts/vmtest-run` forwards ordinary Go test
controls with Go's usual meaning.

The two-node kubeadm smoke stages source-controlled test-owned bootstrap
fixtures from `internal/vmtest/scenarios/testdata/bootstrap`. The harness
installs a test-owned CNI, imports scratch workload images built from local
test binaries, waits for nodes to become Ready before applying workloads, and
then proves cross-node Service traffic with a small client/server workload. This
is VM test scaffolding only; it does not make Katl select a production CNI, DNS,
GitOps, or workload distribution.

The wrapper also accepts a small set of runner controls before Go test
arguments:

```text
--artifact-set=runtime
  build only the runtime root artifact before running tests

--artifact-set=default
  build the installer and install image needed by installer-backed tests

--no-rebuild
  skip wrapper-managed artifact builds
```

Use the runtime artifact set for tests that direct-boot the runtime squashfs and
do not need installer output.

## Resource Generation

The world runner should prepare shared resources and expose fixture factories
from repo-controlled conventions:

```text
mkosi artifacts and artifact indexes
runtime roots and KatlOS install images
direct runtime squashfs boot inputs
node metadata templates
install manifest templates
tmpdir workspace for first-install target disks
durable cache workspace for published installed-runtime fixtures
scenario manifests and result paths
```

Scenario code uses those factories after it declares the topology under test.
A two-node kubeadm test asks for `cp-1` and `worker-1`; a stacked-etcd test asks
for `cp-1`, `cp-2`, and `cp-3`. The world does not need to know those shapes in
advance. It provides the artifact set, scratch roots, network CIDR, and locking
needed to make the requests deterministic and isolated.

Scenarios that only need to prove the runtime image boots should attach the
runtime root squashfs directly and avoid first-install fixture production.
Scenarios that need generation 0 state, ESP contents, kubeadm paths, or
bootstrap node state should use cached installed-runtime fixtures published
under `cacheDir`. The desired form of those fixtures is the content-keyed
installed KatlOS baseline described in
`docs/internal/installed-katlos-vmtest-framework.md`; scenarios should boot
private qcow2 overlays of that baseline instead of rerunning install.

## Failure Semantics

Enabled hermetic tests should never silently pass by skipping missing resources.
The runner and result aggregator should use these statuses:

```text
passed
  setup completed and assertions passed

failed
  setup completed and a Katl behavior assertion failed

setup-failed
  repo-owned setup failed, generated resources were absent or stale, an artifact
  digest did not match, or an enabled test skipped without a host capability
  reason

host-skipped
  the host lacks a declared optional capability such as libvirt, image tooling,
  OVMF, KVM, vsock, or the selected VM network

disabled
  the scenario was outside the selected suite
```

Only `host-capability` gaps may become `host-skipped`. Missing fixture paths,
missing node metadata, missing per-node addresses, and missing generated
manifests are `setup-failed` in enabled hermetic suites.

## Relationship To Direct go test

Plain unit test runs keep their current role:

```text
go test ./...
```

They run unit, parser, planner, golden, and helper tests. Enabled VM scenarios
remain disabled unless explicitly enabled.

When a VM test is enabled and no world manifest is available, it should fail
with a setup error that names `scripts/vmtest-run`. It should not ask the
developer to export a list of fixture paths and addresses.

## Implementation Sequence

1. Define `VMTestWorld` and scenario manifest schemas in Go.
2. Add `scripts/vmtest-exec` that joins the runner-created tmpdir world and
   exports `KATL_VMTEST_WORLD_MANIFEST`.
3. Add `scripts/vmtest-run` as the conventional entrypoint. It should accept
   ordinary `go test` package patterns and flags, prepare the world, and end by
   executing `go test` with `-exec scripts/vmtest-exec`.
4. Move enabled VM tests from skip-on-missing to strict setup failure unless the
   missing prerequisite is a declared host capability.
5. Add world-backed address leasing and scenario directories to `internal/vmtest`.
6. Convert the two-node and three-control-plane tests to consume the world
   manifest, move them out of `cmd/katlctl`, and allocate their own node
   topology.
7. Add direct runtime squashfs scenarios for VM tests that do not need installer
   state.
8. Add world-backed fixture factories so scenarios can generate and cache
   first-install and installed-runtime fixtures from their declared node specs.
9. Remove legacy resolver, wrapper, and environment-variable VM entrypoints, or
   move any still-needed policy into typed helpers behind `scripts/vmtest-run`.
10. Update developer and CI documentation so `scripts/vmtest-run` is the only
   supported VM suite entrypoint.

## Open Questions

1. Should successful tmpdir worlds be deleted automatically?

   Initial recommendation: keep failed worlds, delete successful worlds by
   default after writing a small summary and updating `_build/vmtest/latest` only
   when the run is preserved.

2. Should mkosi build output move fully under the tmpdir world?

   Initial recommendation: the hermetic runner should set build and state roots
   under the world for standard runs. Existing `_build/mkosi` outputs can remain
   a developer cache or debug input until the resource graph is stable.
