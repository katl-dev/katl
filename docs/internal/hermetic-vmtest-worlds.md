# Hermetic VM Test Worlds

Status: current implementation plan.

Katl VM and cluster tests should create a temporary world, then run scenarios
inside it. The world is the host-side substrate for tests: scratch storage, host
capabilities, network substrate, artifact locations, logging, and result
collection. Each Go test still owns the scenario it is proving, including how
many VMs it starts and which guest addresses it allocates inside the world.

This keeps the test contract conventional:

```text
scripts/vmtest-run
scripts/vmtest-run ./internal/vmtest/scenarios -run '^TestTwoNodeKubeadmJoinSmoke$'
```

Developers should not pass installed disks, ESP trees, node metadata, fixture
manifests, or per-node IP addresses for normal test runs. Focused resolver
scripts may remain for debugging already-produced fixtures, but they are not the
primary test entrypoint.

After a developer installs the required host capabilities, `scripts/vmtest-run`
should do everything else needed for an enabled VM test: create the world,
prepare shared artifacts, set up host-side networking, invoke Go tests through
`go test -exec`, collect results, and print the preserved run directory when a
failure needs inspection.

This is the canonical VM test direction. The broader resource-test entrypoint
may run VM suites through `scripts/vmtest-run`, but it should not define a
separate VM fixture contract. Older resolver scripts and direct environment
variables remain useful debug surfaces for already-produced artifacts; normal
developer, agent, and CI runs should enter through the hermetic world runner.

## Design Goals

The hermetic runner should:

```text
create all mutable output under TMPDIR
print the run directory and link the latest run from build/vmtest/
probe host capabilities before running scenarios
set up a host-side VM test world with a bridge or other selected network backend
provide a CIDR and gateway to tests, not per-scenario node addresses
build or resolve Katl artifacts before scenario assertions start
run Go test binaries inside the world with go test -exec
collect logs, manifests, result files, and summaries from every enabled scenario
fail when enabled tests skip for missing repo-owned setup
classify only declared host capability gaps as host-skipped
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
  go-test.json
  summary.json
```

Repo-local `build/` should not be the primary scratch root for hermetic runs.
It may contain only durable pointers and small summaries, for example:

```text
build/vmtest/latest -> ${TMPDIR:-/tmp}/katl-vmtest/<run-id>
build/vmtest/latest-summary.json
```

On failure the runner prints the absolute run directory and leaves it in place.
On success it may delete large throwaway state according to a keep policy, but
the summary should still identify the run and the artifacts that were tested.

The runner should avoid writing host-specific absolute paths into committed
configuration. Absolute paths are acceptable in generated tmpdir manifests and
failure artifacts because they describe one local run.

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
  "artifactDir": "/tmp/katl-vmtest/20260606T120000Z-abc123/artifacts",
  "scenarioDir": "/tmp/katl-vmtest/20260606T120000Z-abc123/scenarios",
  "network": {
    "backend": "bridge",
    "bridge": "katl-vmtest0",
    "cidr": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "leaseFile": "/tmp/katl-vmtest/20260606T120000Z-abc123/network/leases.json"
  },
  "capabilities": {
    "qemu": "passed",
    "qemu-img": "passed",
    "ovmf": "passed",
    "kvm": "passed",
    "bridge": "passed",
    "vsock": "passed"
  }
}
```

The world may provide a CIDR and configured host-side bridge. It should not
encode that a two-node test needs `cp-1` at one address and `worker-1` at
another. That allocation belongs to the scenario.

## Scenario Ownership

Inside a world, a Go test creates its own scenario. Scenario tests should live
in a VM integration package such as `internal/vmtest/scenarios` while the
harness API is internal and unstable. They should not live in end-user command
packages such as `cmd/katlctl`.

`katlctl` is still tested by normal unit and command tests in its own package.
When a VM scenario needs to prove user-facing management behavior, it should
execute the built `katlctl` command as a black-box tool inside the scenario
rather than making `cmd/katlctl` own the VM world.

Example scenario shape:

```go
func TestTwoNodeKubeadmJoinSmoke(t *testing.T) {
    world := vmtest.RequireWorld(t)
    scenario := world.NewScenario(t, "two-node-kubeadm")

    cp := scenario.NewNode(t, vmtest.NodeSpec{
        Name: "cp-1",
        Role: vmtest.ControlPlane,
    })
    worker := scenario.NewNode(t, vmtest.NodeSpec{
        Name: "worker-1",
        Role: vmtest.Worker,
    })

    scenario.BootInstalledRuntime(t, cp)
    scenario.BootInstalledRuntime(t, worker)
    scenario.RunKatlctlBootstrap(t, cp, worker)
}
```

The scenario layer allocates addresses from the world CIDR, renders node
metadata, creates install manifests, creates or reuses installed-runtime
fixtures, starts VMs, and records scenario-local contracts. Parallel scenarios
coordinate through the world lease file instead of hard-coded addresses.

This boundary keeps the harness generic. The harness sets up the world; tests
decide what lives in it.

## Network Model

The first network backend may be a bridge because kubeadm join needs guest to
guest reachability. The hermetic runner should own bridge setup for the run
where host policy allows it:

```text
choose or create a test bridge
assign the bridge gateway address inside the selected CIDR
enable the host-side settings required by the QEMU backend
write the bridge, CIDR, gateway, and lease file to world.json
tear down runner-created network state during cleanup
```

Developers may provide a CIDR or network backend override for local debugging,
but normal runs should have defaults. Tests should request leases from the
world rather than receiving addresses through environment variables.

If the host cannot provide the selected backend, the world setup result is a
host capability gap. A required CI suite should fail; an optional local suite
may report `host-skipped`.

## go test -exec

`scripts/vmtest-run` is a thin wrapper over `go test`. Its normal arguments are
Go package patterns and `go test` flags:

```sh
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run '^TestTwoNodeKubeadmJoinSmoke$'
```

The wrapper injects the VM world by translating that command to the equivalent
`go test -exec` invocation:

```sh
go test -count=1 -json -exec scripts/vmtest-exec ./internal/vmtest/scenarios \
  -run '^TestTwoNodeKubeadmJoinSmoke$'
```

`go test -exec` lets the world wrapper run each compiled test binary inside a
prepared environment. `go test` builds the package test binary, then invokes:

```text
scripts/vmtest-exec <test-binary> <test-args...>
```

The exec wrapper should:

```text
join the tmpdir world created by scripts/vmtest-run
export KATL_VMTEST_WORLD_MANIFEST
force VM tests into strict enabled mode
forward all test arguments unchanged
preserve the child exit code
record go test package metadata and result paths
print the run directory on failure
```

`go test -exec` wraps one package test binary at a time. `scripts/vmtest-run`
should own broad invocations, create the world once, invoke
`go test -exec scripts/vmtest-exec` for the selected packages, collect all
`go test -json` streams, and write the final summary.

For harness debugging, `scripts/vmtest-exec` may create a one-package world when
no manifest is present and an explicit debug flag allows it. The standard path
is still runner-created world first, test binary execution second.

The wrapper should not grow a separate scenario configuration language. Package
selection, `-run`, `-count`, `-timeout`, and other ordinary test controls should
remain `go test` flags. Any harness-only options should be few, clearly named,
and limited to world behavior such as keep policy, CIDR override, network
backend, or host-skip policy.

VM suites should use `-count=1`; they must not depend on Go test caching.

## Resource Generation

The world runner should prepare shared resources and expose fixture factories
from repo-controlled conventions:

```text
mkosi artifacts and artifact indexes
runtime roots and KatlOS install images
node metadata templates
install manifest templates
tmpdir workspace for first-install target disks
tmpdir workspace for published installed-runtime fixtures
scenario manifests and result paths
```

Scenario code uses those factories after it declares the topology under test.
A two-node kubeadm test asks for `cp-1` and `worker-1`; a stacked-etcd test asks
for `cp-1`, `cp-2`, and `cp-3`. The world does not need to know those shapes in
advance. It provides the artifact set, scratch roots, network CIDR, and locking
needed to make the requests deterministic and isolated.

Existing resolver scripts can remain useful lower-level tools:

```text
scripts/resolve-installed-runtime-fixture
scripts/resolve-first-install-katlos-image-fixture
scripts/resolve-two-node-kubeadm-fixtures
scripts/resolve-three-control-plane-kubeadm-fixtures
```

In hermetic runs, those scripts should be called only with scenario-generated
inputs or replaced by equivalent Go helpers. They should not be the interface
that asks a developer to assemble the suite by hand.

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
  the host lacks a declared optional capability such as QEMU, OVMF, KVM, vsock,
  nspawn privileges, or the selected VM network backend

disabled
  the scenario was outside the selected suite
```

The missing-prerequisite model should record a kind such as:

```text
host-capability
artifact
fixture
tool
config
```

Only `host-capability` gaps may become `host-skipped`. Missing fixture paths,
missing node metadata, missing per-node addresses, and missing generated
manifests are `setup-failed` in enabled hermetic suites.

## Relationship To Direct go test

Plain unit test runs keep their current role:

```text
go test ./...
```

They run unit, parser, planner, golden, and helper tests. VM scenarios remain
disabled unless explicitly enabled.

Focused direct VM runs that bypass `scripts/vmtest-run` are still useful for
harness development, but they are a debug surface:

```text
KATL_VMTEST_WORLD_MANIFEST=/tmp/katl-vmtest/<run-id>/world.json \
  go test ./internal/vmtest/scenarios -run TwoNode -count=1 -katl.vmtest.run
```

When a VM test is enabled and no world manifest is available, it should fail
with a setup error that names `scripts/vmtest-run`. It should not ask the
developer to export a list of fixture paths and addresses.

## Implementation Sequence

1. Define `VMTestWorld` and scenario manifest schemas in Go.
2. Add `scripts/vmtest-exec` that joins the runner-created tmpdir world and
   exports `KATL_VMTEST_WORLD_MANIFEST`.
3. Add `scripts/vmtest-run` as the conventional entrypoint. It should accept
   ordinary `go test` package patterns and flags, then inject
   `-exec scripts/vmtest-exec`.
4. Move enabled VM tests from skip-on-missing to strict setup failure unless the
   missing prerequisite is a declared host capability.
5. Add world-backed address leasing and scenario directories to `internal/vmtest`.
6. Convert the two-node and three-control-plane tests to consume the world
   manifest, move them out of `cmd/katlctl`, and allocate their own node
   topology.
7. Add world-backed fixture factories so scenarios can generate first-install
   and installed-runtime fixtures from their declared node specs.
8. Keep resolver scripts as debug validators over already-produced fixtures, or
   retire their policy into typed Go helpers once the world runner is stable.
9. Update developer and CI documentation so fixture path environment variables
   are presented as debug overrides, not the primary way to run VM suites.

## Open Questions

1. Should the default network backend be a runner-created bridge or direct QEMU
   networking?

   Initial recommendation: use a bridge first if it is the shortest reliable
   path to kubeadm join. Keep the backend behind the world schema so direct QEMU
   or libvirt can replace it without changing scenario tests.

2. Should successful tmpdir worlds be deleted automatically?

   Initial recommendation: keep failed worlds, delete successful worlds by
   default after writing a small summary and updating `build/vmtest/latest` only
   when the run is preserved.

3. Should mkosi build output move fully under the tmpdir world?

   Initial recommendation: the hermetic runner should set build and state roots
   under the world for standard runs. Existing `build/mkosi` outputs can remain
   a developer cache or debug input until the resource graph is stable.
