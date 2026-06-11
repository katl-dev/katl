# Hermetic VM Test Worlds

Status: current implementation plan.

Katl nspawn, VM, and cluster tests should create a temporary world, then run
scenarios inside it. The world is the host-side substrate for tests: scratch
storage, host capabilities, optional network substrate, artifact locations,
logging, and result collection. Each Go test still owns the scenario it is
proving, including whether it starts nspawn containers, VMs, or multi-node
clusters inside the world.

This keeps the test contract conventional:

```text
scripts/vmtest-run
scripts/vmtest-run ./internal/vmtest -run Nspawn
scripts/vmtest-run ./internal/vmtest/scenarios -run '^TestTwoNodeKubeadmJoinSmoke$'
```

Developers should not pass installed disks, ESP trees, node metadata, fixture
manifests, or per-node IP addresses for normal test runs. After the hermetic
runner lands, the older fixture resolver and environment-variable paths should
be removed or folded into world-internal helpers rather than kept as supported
entrypoints.

After a developer installs the required host capabilities, `scripts/vmtest-run`
should do everything else needed for an enabled nspawn or VM test: create the
world, prepare shared artifacts, set up configured host-side world resources,
export the world environment, and execute Go tests through `go test -exec`. The
resulting Go test process owns output and exit status.

This is the canonical nspawn and VM test direction. The broader resource-test
entrypoint may run suites through `scripts/vmtest-run`, but it should not define
a separate fixture contract. Normal developer, agent, and CI runs should enter
through the hermetic world runner, and Katl should not maintain parallel VM test
execution systems once scenarios have been converted.

## Design Goals

The hermetic runner should:

```text
create all mutable output under TMPDIR
print the run directory and link the latest run from build/vmtest/
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
  nspawn/
  packages/
  scenarios/
```

Repo-local `build/` should not be the primary scratch root for hermetic runs.
It may contain only durable pointers or small summaries produced by separate
aggregation tooling, for example:

```text
build/vmtest/latest -> ${TMPDIR:-/tmp}/katl-vmtest/<run-id>
build/vmtest/latest-summary.json
```

The runner should print the absolute run directory before handing off to
`go test`, and `scripts/vmtest-exec` should print it again when a package test
binary exits nonzero. Cleanup and summary generation belong to separate tooling
when the project needs those workflows.

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
    "vsock": "passed",
    "systemd-nspawn": "passed"
  }
}
```

The world provides the selected libvirt network, CIDR, gateway, and DHCP lease
artifact. It should not encode that a two-node test needs `cp-1` at one address
and `worker-1` at another. That allocation belongs to the scenario.

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

## Nspawn Model

The same world model applies to nspawn-backed checks. The difference is that an
nspawn-only world usually needs a prepared userspace root or image, bind mount
workspaces, cgroup and mount privileges, and result directories; it does not need
libvirt, image tooling, OVMF, KVM, or vsock unless the selected test also starts
VMs.

The direct developer entrypoint remains the same thin wrapper over `go test`:

```sh
scripts/vmtest-run ./internal/vmtest -run Nspawn
```

The runner should prepare or resolve the runtime userspace fixture under the
tmpdir world and record its digest in `world.json` or a scenario manifest. The
Go test should decide which generated unit trees, config trees, or request
helpers to mount into that userspace and which `systemd-nspawn` assertions to
run.

Missing `systemd-nspawn`, cgroup support, or required mount privileges are host
capability gaps. Missing runtime artifacts, stale generated userspace fixtures,
or absent bind inputs are setup failures in enabled hermetic runs.

Nspawn tests are a fast userspace contract, not a replacement for VM tests. They can
prove generated systemd/config syntax, runtime helper behavior, and selected
filesystem projections before boot, while VM tests remain responsible for
firmware, disk layout, boot selection, networking, kubelet startup, kubeadm, and
rollback behavior.

## go test -exec

`scripts/vmtest-run` is a thin world setup wrapper over `go test`. Its arguments
are Go package patterns and `go test` flags:

```sh
scripts/vmtest-run ./internal/vmtest -run Nspawn
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run '^TestTwoNodeKubeadmJoinSmoke$' -count=1
```

The wrapper injects the VM world by preparing the world environment and ending
with the equivalent `go test -exec` invocation:

```sh
go test -exec scripts/vmtest-exec ./internal/vmtest/scenarios \
  -run '^TestTwoNodeKubeadmJoinSmoke$' -count=1
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
print the run directory on failure
```

`go test -exec` wraps one package test binary at a time. `scripts/vmtest-run`
creates the world once, exports the world variables, and then tail-calls
`go test -exec scripts/vmtest-exec` with the caller's arguments. The runner
should not classify package patterns or flags, choose a default package set,
inspect `-run` expressions, pipe Go test output, or write a post-test summary
after `go test` exits. Callers can pass the normal `go test` `-json` flag when
JSON event output is needed.

`scripts/vmtest-exec` is an implementation detail of `scripts/vmtest-run`, not
a second developer entrypoint. It should fail when invoked without a world
created by the runner.

Package selection, `-run`, `-count`, `-timeout`, and other ordinary test controls
remain `go test` flags with Go's usual meaning. Harness controls should use
environment variables or a separate setup command so the runner does not need to
parse the Go test argument stream.

Nspawn and VM suites should use `-count=1`; callers or higher-level check
commands should pass that flag explicitly because `scripts/vmtest-run` forwards
ordinary Go test controls with Go's usual meaning.

## Resource Generation

The world runner should prepare shared resources and expose fixture factories
from repo-controlled conventions:

```text
mkosi artifacts and artifact indexes
runtime roots and KatlOS install images
prepared nspawn userspace roots or images
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

Legacy resolver scripts were transitional migration tools. Once the world
fixture factories exist, those scripts should either be removed or have their
policy moved into typed Go helpers used behind `scripts/vmtest-run`. They should
not remain supported developer entrypoints for assembling VM suites by hand.

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
  OVMF, KVM, vsock, nspawn privileges, or the selected VM network

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

They run unit, parser, planner, golden, and helper tests. Nspawn and VM
scenarios remain disabled unless explicitly enabled.

When an nspawn or VM test is enabled and no world manifest is available, it
should fail with a setup error that names `scripts/vmtest-run`. It should not
ask the developer to export a list of fixture paths and addresses.

## Implementation Sequence

1. Define `VMTestWorld` and scenario manifest schemas in Go.
2. Add `scripts/vmtest-exec` that joins the runner-created tmpdir world and
   exports `KATL_VMTEST_WORLD_MANIFEST`.
3. Add `scripts/vmtest-run` as the conventional entrypoint. It should accept
   ordinary `go test` package patterns and flags, prepare the world, and end by
   executing `go test` with `-exec scripts/vmtest-exec`.
4. Move enabled nspawn and VM tests from skip-on-missing to strict setup failure
   unless the missing prerequisite is a declared host capability.
5. Add world-backed address leasing and scenario directories to `internal/vmtest`.
6. Convert the two-node and three-control-plane tests to consume the world
   manifest, move them out of `cmd/katlctl`, and allocate their own node
   topology.
7. Add world-backed fixture factories so scenarios can generate first-install
   and installed-runtime fixtures from their declared node specs.
8. Remove legacy resolver, wrapper, and environment-variable VM entrypoints, or
   move any still-needed policy into typed helpers behind `scripts/vmtest-run`.
9. Update developer and CI documentation so `scripts/vmtest-run` is the only
   supported nspawn and VM suite entrypoint.

## Open Questions

1. Should successful tmpdir worlds be deleted automatically?

   Initial recommendation: keep failed worlds, delete successful worlds by
   default after writing a small summary and updating `build/vmtest/latest` only
   when the run is preserved.

2. Should mkosi build output move fully under the tmpdir world?

   Initial recommendation: the hermetic runner should set build and state roots
   under the world for standard runs. Existing `build/mkosi` outputs can remain
   a developer cache or debug input until the resource graph is stable.
