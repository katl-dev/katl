# VM Test Harness Design

Status: current implementation direction.

Katl VM tests run through `scripts/vmtest-run` and the Go helpers in
`internal/vmtest`. The supported VM execution backend is libvirt using the
system connection, normally `qemu:///system`. Tests should not invoke hypervisor
binaries directly, depend on ad hoc host networking setup, or ask developers to
assemble per-scenario disks and addresses by hand.

## Contract

The developer entrypoint is conventional Go test syntax wrapped by the world
runner:

```sh
scripts/vmtest-run --artifact-set=runtime ./internal/vmtest -run '^TestDirectRuntimeVMTestAgentSmoke$' -count=1
scripts/vmtest-run ./internal/vmtest -run '^TestFirstInstallTargetDiskBootSmoke$' -count=1
scripts/vmtest-run ./internal/vmtest/scenarios -run 'TwoNode|ThreeControlPlane' -count=1
```

`scripts/vmtest-run` creates one temporary world, records host capabilities,
exports `KATL_VMTEST_WORLD_MANIFEST`, and executes the requested package tests
through `go test -exec scripts/vmtest-exec`. `go test` owns test selection,
output, timeout flags, and exit status.

Tests that do not need installer side effects should not first manufacture an
installed disk. They should direct-boot the runtime root squashfs with
libvirt's kernel/initrd boot path, attach the squashfs as `/dev/vda`, and use
volatile state plus systemd masks for installed-state units. That fast tier is
the default smoke for runtime reachability and `katl-vmtest-agent` health.

Tests that need generation state, installed ESP contents, kubeadm host paths,
or first-install handoff semantics still use installer-produced installed
runtime fixtures. Those fixtures are cached under the world cache directory so
one successful first-install run can feed later kubeadm and bootstrap tests.
The target fast path for those tests is the installed KatlOS VM test framework
defined in `docs/internal/installed-katlos-vmtest-framework.md`: first install
publishes a content-keyed generation-0 fixture once, and later scenarios boot
private overlays of that installed baseline.

The harness records enough state for debugging:

```text
world.json
host-capabilities.json
scenario.json
result.json
domain.xml
launch-command.txt
installer-serial.log
runtime-serial.log
libvirt-lease.json
```

Serial output is written to the scenario artifacts and streamed live for the
default executor so a hung boot has visible progress in the Go test output.
Timeout failures include the tail of the serial log.

## Host Model

A capable host provides:

```text
virsh in PATH
qemu-img or KATL_VMTEST_IMAGE_TOOL for disk image creation
readable OVMF code and vars images
/dev/kvm when KATL_VMTEST_KVM=on
/dev/vhost-vsock when a scenario uses vsock
access to qemu:///system through group membership, polkit, or an explicit
privileged session
an active libvirt network and storage pool
```

The default libvirt settings are:

```text
KATL_VMTEST_LIBVIRT_URI=qemu:///system
KATL_VMTEST_LIBVIRT_NETWORK=default
KATL_VMTEST_LIBVIRT_STORAGE_POOL=default
```

Developers can override those names for local debugging, but the normal mental
model should be: install and enable libvirt, grant the user access, make sure a
network and storage pool are active, then run `scripts/vmtest-run`.

## Planning

Scenario planning is typed and must fail before VM launch when required inputs
are missing. Plans resolve:

```text
installer or runtime boot artifacts
target disks and backing images
preseed media
OVMF vars copy
libvirt domain name and XML
serial log paths
vsock device settings
expected serial signals and timeouts
```

`scripts/vmtest-run` has an artifact-set selector for the build phase:

```text
--artifact-set=runtime
  build or reuse only the runtime root squashfs for direct runtime tests

--artifact-set=default
  build or reuse the installer and KatlOS install image for installer-backed
  tests

--no-rebuild
  rely entirely on already-provided artifacts and indexes
```

The runtime-only gate should be the first VM feedback path for changes that can
be proven without installer, disk layout, ESP, or generation-state behavior.
For changes that need installed state but not installer behavior, the preferred
gate is an installed KatlOS fixture overlay rather than a fresh first-install
boot loop.

The runner writes libvirt domain XML with Go's XML encoder. Tests should assert
semantic XML and lifecycle behavior rather than hard-coding direct process argv.

`qemu-img` remains an image manipulation tool. References to it should be named
as image-tool requirements in Katl code and docs, not as the VM execution
backend.

## Lifecycle

The default executor:

1. Defines the libvirt domain from the generated XML.
2. Starts the domain.
3. Captures serial output with a direct `virsh console` process using a Go-owned PTY. The harness appends output to the serial artifact and tails it to stderr for live progress.
4. Waits for the expected serial signal, timeout, idle timeout, or early exit.
5. Runs optional serial hooks and guest-agent checks.
6. Destroys and undefines the domain with NVRAM cleanup.

Cleanup must run even when assertions fail or contexts are cancelled. Tests must
not leave domains running after the test returns.

## Ownership

`scripts/vmtest-run` holds an exclusive flock on `<run dir>/lease` for the
whole run and records the path in the world manifest as `lease`. The kernel
releases it however the runner ends. Libvirt domains and networks that the run
creates carry the run ID and the lease path in their `katl/vmtest` metadata, as
`run` and `lease` attributes.

The first scenario of each test process removes the vmtest domains and networks
whose lease nobody holds: they belong to runs that have ended, including domains
preserved for debugging. It never touches objects with other metadata, such as
katldev worlds, or vmtest objects that name no lease and so have no owner to
ask.

## Admission

Runs share the host without a host-wide lock. A scenario in a leased libvirt
world reserves vCPUs and memory for the nodes it declares (`vmtest.Nodes`,
one by default, each at the default guest size) when it is planned, and
`WorldScenario.Release` gives them back after its network is removed. Adding
more nodes than the scenario declared fails.

The ledger is a directory of reservation files shared by the user's runs
(`KATL_VMTEST_ADMISSION_DIR`, `/tmp/katl-vmtest-admission-<uid>` by default). A
reservation counts while a process holds a flock on its file; the kernel drops
that lock when the holder dies, so a killed run frees its capacity and the next
admission deletes the leftover file. Counting and creating a reservation happen
under one ledger lock.

The host's CPU count sets the vCPU budget for concurrent runs. A run can exceed
that budget when it holds the only reservations, including when it adds nodes
through a nested scenario. While multiple runs hold reservations, their
combined demand must fit within the vCPU budget.

The memory budget defaults to the host's total memory minus the larger of 2 GiB
and one eighth of total memory. If a run's combined memory demand exceeds that
budget, admission fails immediately. A scenario waits if the other runs'
reservations leave too little capacity or the host has too little memory
available beyond its reserve. While a scenario waits, it reports its demand
and what is holding capacity. `KATL_VMTEST_ADMISSION_VCPUS` and
`KATL_VMTEST_ADMISSION_MEMORY_MIB` override the respective budgets.

vsock guest CIDs are allocated by libvirt (`cid auto='yes'`) and read back from
the running domain.

## Networking

Each scenario of a leased world creates its own libvirt NAT network, named
`katl-<run ID>-<scenario ID>`, on a /24 that no libvirt network, host interface,
or route uses. Choosing the subnet and starting the network happen under a host
lock, and a subnet that libvirt refuses is skipped. The scenario's copy of the
world network describes that network, and the network is removed when the
scenario ends unless a preserved domain still uses it.

The network splits its addresses into a reservable range and a short dynamic
DHCP tail. Every planned node gets a DHCP reservation for its MAC at its planned
address, so the planned address is the address the guest receives, and an
unreserved guest only ever receives a tail address. dnsmasq logs DHCP
transactions (`log-dhcp`). VMs that belong to no scenario node use the world's
network, `default` unless `KATL_VMTEST_LIBVIRT_NETWORK` names another.

Katl does not support ad hoc host networking as a current path.

## Failure Semantics

Enabled VM tests should not silently skip because fixtures were absent. Missing
repo-owned artifacts, stale generated inputs, and invalid scenario manifests are
setup failures. Missing host capabilities are recorded in
`host-capabilities.json` and may be reported as host capability gaps by the
runner or summary tooling.

Plain `go test ./...` keeps enabled VM scenarios disabled. When a VM scenario is
explicitly enabled without a world manifest, it should fail with a setup message
that names `scripts/vmtest-run`.
