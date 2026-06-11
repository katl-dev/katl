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
scripts/vmtest-run ./internal/vmtest -run '^TestFirstInstallTargetDiskSerialSmoke$' -count=1
scripts/vmtest-run ./internal/vmtest/scenarios -run 'TwoNode|ThreeControlPlane' -count=1
```

`scripts/vmtest-run` creates one temporary world, records host capabilities,
exports `KATL_VMTEST_WORLD_MANIFEST`, and executes the requested package tests
through `go test -exec scripts/vmtest-exec`. `go test` owns test selection,
output, timeout flags, and exit status.

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
virsh and script in PATH
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

The runner writes libvirt domain XML with Go's XML encoder. Tests should assert
semantic XML and lifecycle behavior rather than hard-coding direct process argv.

`qemu-img` remains an image manipulation tool. References to it should be named
as image-tool requirements in Katl code and docs, not as the VM execution
backend.

## Lifecycle

The default executor:

1. Defines the libvirt domain from the generated XML.
2. Starts live serial capture through `virsh console`.
3. Starts the domain.
4. Waits for the expected serial signal, timeout, idle timeout, or early exit.
5. Runs optional serial hooks and guest-agent checks.
6. Destroys and undefines the domain with NVRAM cleanup.

Cleanup must run even when assertions fail or contexts are cancelled. Tests must
not leave domains running after the test returns.

## Networking

VM tests use libvirt networks. The world manifest records the libvirt network
name, CIDR, gateway, and lease artifact path. Scenarios allocate node identities
inside the world and discover guest addresses from libvirt DHCP leases.

Katl does not support ad hoc host networking as a current path.

## Relationship To Nspawn

Nspawn checks share `scripts/vmtest-run` and world setup where useful, but they
remain userspace checks. They can prove generated systemd/config syntax and
runtime helper behavior before boot. VM tests remain responsible for firmware,
disk layout, boot selection, kubelet startup, kubeadm flows, update behavior,
and rollback behavior.

## Failure Semantics

Enabled VM tests should not silently skip because fixtures were absent. Missing
repo-owned artifacts, stale generated inputs, and invalid scenario manifests are
setup failures. Missing host capabilities are recorded in
`host-capabilities.json` and may be reported as host capability gaps by the
runner or summary tooling.

Plain `go test ./...` keeps enabled VM scenarios disabled. When a VM scenario is
explicitly enabled without a world manifest, it should fail with a setup message
that names `scripts/vmtest-run`.
