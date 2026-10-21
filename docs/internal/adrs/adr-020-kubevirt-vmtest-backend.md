# ADR-020: KubeVirt backend for VM qualification

Status: proposed; implementation and cluster qualification are outstanding.

Date: 2026-10-21.

## Decision

Extend the existing Go vmtest world with interchangeable libvirt and KubeVirt
backends. Keep one scenario catalog, fixture policy, guest protocol, result
format, and qualification policy. Select infrastructure at world creation;
scenario code must not select a hypervisor or invoke its control tools.

Run KubeVirt guests directly on cluster workers. Initially place every VM and
the test process in a world on one worker, using a private layer-2 network.
Schedule independent worlds across workers. This removes nested virtualization
from tests while preserving guest-to-guest and test-to-guest connectivity.
Cross-worker placement *within* a world is a separate network capability, not a
prerequisite for farming the existing suites across the cluster.

Retain libvirt for local development, hosted fallback, and comparison. Do not
switch required CI checks to KubeVirt until it passes the entire existing
vmtest contract. A missing capability fails a selected CI gate; it must not
silently skip a test or fall back to another backend.

Keep source regression qualification and exact-production-artifact
qualification separate. Both are required before automatic release promotion.
Moving tests to KubeVirt does not close the existing production-byte
qualification gap.

This ADR designs test infrastructure, not a Katl Kubernetes distribution. Cluster
operators own KubeVirt, storage, networking, runner registration, and admission
policy through their infrastructure repository. All interfaces and commands
introduced below are proposals, not implemented behavior.

## Requirements and boundaries

The backend must preserve:

- ISO, UKI/ESP, direct kernel/initrd, installed-disk, and direct-runtime boot.
- Installation handoff, blank and extra disks, stable disk identities, independent
  fixture clones, offline inspection, and installer-to-installed boot transitions.
- Persistent identity, disks, and firmware variables across the reboot and
  stop/start boundaries a scenario requests.
- Concurrent multi-node Kubernetes and DRBD scenarios, stable addresses, direct
  public `katlctl`, SSH, HTTP and Kubernetes API access, and disconnected NIC tests.
- Agent access independent of guest IP configuration, serial signals and hooks,
  bounded execution, diagnostics, failure retention, and safe cleanup.
- Existing image integrity/provenance checks and exclusion of test support from
  production artifacts.

Performance and cost motivate the backend but are not established benefits.
Measure queueing, build/import time, storage amplification, and complete gate
latency before changing the default. Do not reduce durability, assertions, or
scenario coverage to obtain a better benchmark.

Live migration, arbitrary hypervisor XML, cloning running machine memory, and
cross-architecture emulation are not requirements of the audited vmtest suite.
Keep libvirt-specific `katldev` behavior working while extracting shared code;
this ADR does not migrate the persistent developer environment to KubeVirt.

## Evidence and existing coupling

The audit uses Katl parent revision `1ab155f4` and home-ops revision
`2e9d976a8a3c`. Read-only cluster observations on the ADR date found three amd64
workers, each reporting 20 allocatable CPUs and approximately 59.4 GiB allocatable
memory. These are capacities, not available headroom. No KubeVirt or CDI CRDs or
advertised KubeVirt KVM resource were observed. This does not establish whether
host KVM is usable before installing its device plugin.

The home-ops configuration provides:

| Component | Evidence | Design implication |
| --- | --- | --- |
| Cilium | `k8s/apps/kube-system/cilium/app/helmrelease.yaml` | Use the primary pod network for the Kubernetes API and controlled external traffic. Verify actual policy enforcement. |
| Multus | `k8s/apps/kube-system/multus/` | Existing attachment support can be reused; inspect installed CNI binaries and compatibility. |
| Trusted LAN attachment | `multus/networks/trusted-lan.yaml`, macvlan on `bond0.20` | Do not attach CI guests to it. Namespaces do not isolate that physical LAN. |
| Ceph RBD | `k8s/apps/rook-ceph/rook-ceph/cluster/helmrelease.yaml` | `ceph-block` and `csi-ceph-blockpool` exist in the live cluster; prototype disk import, snapshots, and clones here. |
| Local storage | Live `openebs-hostpath` StorageClass | Do not make node-local storage the initial portable fixture store. |
| Snapshot controller | `k8s/apps/volsync-system/snapshot-controller/` | The snapshot APIs exist; successful VM fixture cloning remains unproven. |

The backend boundary extends beyond `VMExecutor.Run`:

| Source | Responsibility to separate |
| --- | --- |
| `scripts/vmtest-run`, `scripts/vmtest-exec`, `scripts/vmtest-clean` | Host probes, world setup, resource ownership, test execution, retention and cleanup. |
| `internal/vmtest/world.go`, `world_scenario.go` | Libvirt-only manifest, network configuration, file-locked address allocation. |
| `libvirt_vm.go`, `libvirt_lease.go` | Boot planning, XML, firmware files, domain lifecycle, serial capture, local vsock and DHCP leases. |
| `disk.go`, `world_fixtures.go`, `installed_world.go` | Local paths, qemu-img overlays, publication, fixture provenance and mutable consumers. |
| `first_install.go`, `first_install_world.go` | Local media preparation, handoff, stopped-disk ESP extraction, transition into runtime. |
| `installed_node.go`, `agent_client.go`, scenario callers | Exposed vsock CID and local guest IP assumptions; reconnect behavior. |
| `debug_targets.go`, `cmd/katl-vmtest-debug` | Libvirt commands embedded in debug guidance and retained targets. |
| `cmd/katldev` | Shared libvirt primitives, persistent serial and PCI hotplug capacity; preserve concrete local support. |
| `.github/workflows/hosted-vmtests.yml`, `cmd/katl-vmtest-plan` | Artifact fan-out, selected-test pass accounting, stable required check. |
| `.github/workflows/release-artifacts.yml` | Instrumented source suite precedes publication approval; production images are built separately. |

`TestInstallerPXEBootSmoke` uses direct kernel/initrd boot and tests installer
network readiness. It does not exercise firmware PXE DHCP/TFTP. Preserve that
contract and describe it accurately; a future real PXE test needs a boot server
on the isolated world network. `HostForwards` is rejected by the libvirt planner,
so emulating it is not a parity requirement. `HotplugPCISlots` is used by
`katldev`, not by the audited vmtest scenarios; retain it in the libvirt options
instead of inventing a portable PCI topology API.

## Alternatives and ownership

| Solution family | Benefit | Reason for decision |
| --- | --- | --- |
| Keep hosted libvirt only | Lowest implementation and cluster operations cost. | Retain as fallback; it does not provide cluster scheduling of test guests. |
| Self-host libvirt on a bare-metal runner or nested runner VM | Reuses the harness; avoids this backend work. | Viable baseline for cost comparisons, but privileged host access or nested KVM remains concentrated in the runner. Does not meet the requested native pluggable backend. |
| Native KubeVirt behind the existing world | Cluster schedules guests; native VM/storage APIs own infrastructure. | Selected. Preserve one test implementation and make infrastructure dependencies explicit. |
| A separate Kubernetes test operator and rewritten scenarios | Can queue all work through one service. | Duplicates scenario execution, results and reconciliation before there is a need. Use existing Jobs, Leases, VM controllers and a bounded provisioning process instead. |
| Pod masquerade networking for every guest | Minimal network installation. | Does not directly preserve stable, mutually routable guest addresses, advertised endpoints, arbitrary guest traffic, and a shared test LAN. |
| Cross-worker isolated overlay for every world | Removes the single-worker capacity ceiling. | Requires an additional qualified network implementation. Defer until world size or measurements justify it. Never attach worlds to the trusted LAN as a shortcut. |

The selected placement constraint must be revisited if the largest supported
scenario cannot fit on one worker with production headroom, or queueing removes
the expected benefit. At that point evaluate a standard secondary overlay
network with per-world isolation. Do not grow custom VXLAN reconciliation inside
vmtest to conceal the limitation.

## Backend contract

Keep scenario policy in `internal/vmtest`. Initially keep the interface and both
implementations package-private in that package, split across focused files;
this avoids exporting an entire intermediate model. Constructors return concrete
backend types. Call sites consume only the interface they need.

The following sketch identifies operations, not final Go declarations:

```go
type backend interface {
    Probe(context.Context, requirements) (capabilities, error)
    Prepare(context.Context, worldSpec) (worldRef, error)
    Materialize(context.Context, diskSpec) (diskRef, error)
    Clone(context.Context, diskRef) (diskRef, error)
    InspectStopped(context.Context, diskRef, inspection) (artifactRef, error)
    Start(context.Context, machineSpec) (machineRef, error)
    Observe(context.Context, machineRef) (machineState, error)
    DialAgent(context.Context, machineRef) (io.ReadWriteCloser, error)
    CaptureSerial(context.Context, machineRef, io.Writer) error
    Stop(context.Context, machineRef, stopMode) error
    Collect(context.Context, worldRef, artifactSink) error
    Cleanup(context.Context, worldRef, retention) error
}
```

`machineSpec` describes boot media, CPU/memory, disk roles/serials, MAC addresses,
network attachments, firmware persistence and expected lifetime. It contains no
virsh executable, XML, host OVMF filename, PVC name, or vsock CID. Backend resource
references are opaque to scenarios and identify actual objects by immutable UID
or local ownership identity, not merely by reusable names.

Separate desired specifications, observed state, and resource references.
`machineState` reports power state, instance identity, addresses and faults;
scenario readiness still comes from guest behavior. `Start` returning or a VMI
becoming Running does not establish OS health. `Stop` distinguishes graceful
shutdown from forced power loss; guest reboot normally uses the tested public
interface, with a changed boot ID as evidence. A host stop/start is explicit.

Move generic `VMRunner` orchestration, serial matching, deadlines, guest health,
result writing and retention policy out of the libvirt implementation. Replace
`VSockPlan` exposure in running-node handles with a guest connection method.
Route every agent connection through it, including reconnects and debug tools.
`NewAgentClient` already accepts `io.ReadWriteCloser`; retain its framed protocol.

Replace local-file disk arguments at orchestration boundaries with disk
references. Local fixture generation can still use qemu-img and filesystem tools;
remote inspection runs against an exclusively stopped volume. Do not make each
scenario download a PVC and rediscover its ownership rules.

Add a versioned backend discriminator and provider-specific configuration to
`world.json`, plus observed capabilities, resource identities and profile digest.
Keep local artifact paths distinct from remote resource identifiers. Fail clearly
on an unsupported manifest version. Migrate the scripts and debug readers
together; unshipped internal manifest layouts need no compatibility framework.

Keep `scripts/vmtest-run` as the entry point. Add `--backend=libvirt|kubevirt` and
an infrastructure profile; default to libvirt. Move backend lifecycle policy into
a Go command under `cmd/katl-vmtest`, leaving shell as build/exec orchestration.
Reuse the current `go test -exec`, artifact-set, no-rebuild and result contracts.
The KubeVirt test process runs inside its allocated world runner pod; local
invocation submits that same job and streams its result rather than requiring
routes from a developer laptop into the test LAN.

## KubeVirt implementation

### Machines and boot

Use `VirtualMachine` objects with `runStrategy: Manual` and explicit start/stop
subresources. Avoid automatic restart policies that can hide a crash or poweroff.
Observe VM, VMI, launcher pod and guest boot identities separately. Watches must
recover through relist after disconnection/resource-version expiry and retain the
overall deadline. Do not substitute a newly named machine for a failed one.
[KubeVirt run strategies](https://kubevirt.io/user-guide/compute/run_strategies/)
provide the lifecycle primitive; vmtest owns the expected transition.

The feature mapping is:

| Existing contract | KubeVirt implementation and parity evidence |
| --- | --- |
| Direct kernel/initrd and command line | Digest-pinned OCI wrapper with `kernelBoot.container` paths and unchanged boot bytes/arguments. Boot both installer and direct-runtime cases. |
| UKI/EFI tree/image | Generate the existing FAT ESP media, import it, and boot with EFI. Do not treat a UKI as a Linux kernel image. |
| ISO and disk-first install | Read-only CD-ROM media and explicit boot order; writable target first with ISO fallback for a blank disk. Prove same-machine install-to-disk reboot. |
| Direct runtime squashfs | Import as raw block content with the existing root command line; verify no image converter interprets it as a partitioned OS image. |
| Blank, extra and installed disks | PVC-backed volumes with fixed guest serials, sizes and intended buses. Verify `/dev/disk/by-id`, partition geometry and read-only behavior inside the guest. |
| EFI variables | `firmware.bootloader.efi.persistent: true` with qualified backend-state storage; test boot counting, rollback and stop/start. |
| CPU, RAM, KVM, RNG | Explicit resources, x86_64 machine/CPU profile and virtio RNG. Require hardware virtualization; no silent software emulation. |
| Extra disconnected NIC | Additional isolated attachment with link state down; assert installer wait-online and connectivity on the working NIC. |
| Serial signals, failure hooks, persistent output | Capture from the first boot, append with instance identity, and feed the existing matcher. Preserve output before launcher deletion. |
| Root agent without networking | VMI vsock subresource carrying the existing framed connection. |
| SSH, handoff, management API, Kubernetes API | Direct test-LAN access from the runner; no translation of endpoint addresses announced by the guest. |
| Snapshot fixture and ESP inspection | Stopped disk snapshot/clone and restricted helper Job; never inspect a writable live attachment. |
| Retention and debug | UID-bound VM references, serial/log artifacts, agent reconnect, console guidance and explicit cleanup. |

KubeVirt supports [external kernel boot from container images](https://kubevirt.io/user-guide/user_workloads/boot_from_external_source/).
The OCI image is transport packaging and must record the source media checksums.
Pin and record the KubeVirt launcher image, firmware, machine type and CPU
profile in qualification evidence. Arbitrary local OVMF overrides remain
libvirt-specific; the KubeVirt profile supplies a qualified firmware version.
Match the existing Secure Boot setting explicitly rather than inheriting a
different platform default.

### Disk and firmware lifetime

Use CDI DataVolumes for imported disks and independent writable clones, with
Ceph RBD CSI snapshots where supported. The initial implementation keeps caches
within one world and reimports verified inputs for another world. Shared
cross-run PVC caching is an optimization requiring separate authorization and
immutability rules; it must not be a prerequisite for correctness.
[CDI](https://kubevirt.io/user-guide/storage/containerized_data_importer/) supplies
import/clone machinery; Katl retains source identity and fixture validity policy.

Import sources must be content-addressed and immutable. Verify downloaded file
hashes before ingestion and verify imported raw content before exposing a fixture
as ready. If conversion expands qcow2 to raw, record both the original transport
digest and verified raw-content digest. An importer success condition alone is
not integrity evidence. Size limits include virtual capacity and temporary
conversion/snapshot space, not just compressed download size.

Publish an installed fixture only after a successful first install, shutdown,
and confirmation that no writer owns the disk. Create a storage snapshot, wait
for readiness, and clone each consumer independently. Never clone the disk of a
retained failed VM or allow consumers to mount the base writable. Keep provenance
bound to artifact identities, install inputs and fixture contract version.

Use a restricted helper Job to mount an inactive filesystem-mode PVC read-only
and inspect its disk file using userspace tools for partition/ESP extraction.
Do not expose host devices, loop devices, host paths or the worker filesystem to
that Job. A dedicated writer/conversion Job may own the volume only before it is
sealed or attached. Serialize those phases through actual volume/VM state, not
only a local lock file. Start with filesystem-mode RWO volumes; evaluate block
mode only after measuring its benefit and defining an equally safe inspection
path. Do not assume sparse files or discard reduce Ceph usage without measuring.

Persist firmware state on the VM's own backend-state PVC throughout its
lifecycle. Configure the CDI StorageProfile for filesystem RWO on `ceph-block`
and explicitly qualify `vmStateStorageClass` behavior. Do not assume the absence
of CephFS/RWX blocks all persistent state, or that the StorageClass default is
sufficient. KubeVirt documents both access-mode selection and limitations on
[restoring persistent firmware state into a different VM](https://kubevirt.io/user-guide/compute/persistent_tpm_and_uefi_state/).

A cloned installed-disk fixture starts with fresh firmware state; it is a new
machine. A test requiring the previous machine's EFI variables must continue
with that VM and its state PVC. Do not claim whole-machine cloning parity from
a disk-only snapshot. Prove Katl's loader variables across guest reboot and cold
stop/start: KubeVirt can override firmware boot-order settings, so persisted
files alone do not prove the expected boot behavior.

### Guest control and serial evidence

Enable the qualified KubeVirt vsock capability and request
`devices.autoattachVSOCK`. Dial by namespace, VMI name and checked UID through the
API subresource. The CID is assigned by KubeVirt and is diagnostic data, not an
address the runner can dial locally. This keeps agent access working before
network setup and during network failure.

Use the KubeVirt `kubecli` streaming client or its supported successor in the
pinned version, not a generated client method that is only a stub. Adapt the
stream to `io.ReadWriteCloser`, propagate cancellation and bound dial time.
Explicitly request `UseTLS: false` for the existing plaintext framed guest
protocol; keep TLS validation and authentication on the Kubernetes API connection.
This setting controls the guest leg, not API certificate verification. Pin and
review the [VSOCK client implementation](https://github.com/kubevirt/kubevirt/blob/main/staging/src/kubevirt.io/client-go/kubecli/vmi.go)
and [subresource API](https://kubevirt.io/api-reference/main/operations.html)
when choosing dependency versions.

Reconnection must not replay a mutating guest command after an ambiguous response
loss. Return an unknown-outcome error; let the owning scenario observe state and
decide recovery. Health checks may reconnect. Carry the same rule through local
and remote transports, including the existing transport-loss issue recorded in
Beads. Do not let an HTTP/WebSocket retry library replay application requests.

Enable serial logging at launch using the pinned version's supported
`logSerialConsole` facility, collect the `guest-console-log` stream, and persist
it outside the launcher before cleanup. Validate that early boot output is
available even when attachment is delayed. Preserve ordering and avoid replaying
hooks on reconnect; a stream gap is an explicit observation failure, not proof
that an expected marker never occurred. Keep serial-idle timeouts separate from
queue/import deadlines. Interactive console attachment must be marked invasive;
ordinary debug tailing reads captured logs. See the
[KubeVirt API definitions](https://kubevirt.io/api-reference/main/definitions.html)
for the version-sensitive console settings.

### Network and scheduling

Use a small administrator-defined pool of world slots. Each slot binds a worker,
a private bridge name and a capacity budget. A trusted provisioning Job leases a
slot and creates a fresh namespace, admission constraints, quota, runner identity
and allowed NetworkAttachmentDefinition (NAD). Namespace names and the run record
include the workflow run and attempt identity. The runner cannot create arbitrary
NADs or change its placement. Separate allocations never share an active bridge.

The NAD uses the standard bridge CNI with no host IP, no physical uplink and no
host gateway/NAT. All guests and the runner attach to that bridge on its assigned
worker. The primary pod interface remains available to infrastructure pods;
disable automatic attachment of that pod network to guests. A dedicated,
trusted network-service pod provides DHCP, DNS, gateway and bounded egress on the
world LAN. Generate reservations from Go world/node identities and preserve the
existing CIDR/gateway/address allocation contract. Observe DHCP leases and verify
addresses by guest/public access before declaring them usable.

The runner has a static test-LAN address and can invoke unmodified public tools
against guest addresses and advertised Kubernetes endpoints. Guest-to-guest
traffic, service VIPs, guest CNI routes and DRBD stay inside the world. Give the
network-service pod only the capabilities needed inside its own network
namespace, never host networking or a host mount. Firewall ingress to its primary
interface and forwarding from the test LAN separately; default-deny forwarding
must be installed before serving DHCP. Permit required artifact/package endpoints
through a controlled proxy or reviewed egress rules. Deny host, home LAN, cluster
service/pod, metadata and other-world destinations. Account for IPv6 and DNS
rebinding rather than assuming an IPv4 deny list suffices.

Separate the world gateway from the address of runner-hosted fixture services.
For example, `stageOperationBackedKubernetesPayloadBundle` currently advertises
`World.Network.Gateway` for a server listening in the test process. Add a
guest-reachable service address to the world contract and bind those servers to
the runner's test-LAN interface. On libvirt this can equal the gateway; on
KubeVirt it is the runner address. Audit artifact servers, registries and callback
URLs together, and test guest downloads directly. A local listener or successful
test-to-guest connection does not establish guest-to-runner reachability.

The [bridge CNI](https://www.cni.dev/plugins/current/main/bridge/) is host-local;
Multus is attachment plumbing, not cross-node connectivity. Kubernetes primary
NetworkPolicy must not be assumed to protect the secondary bridge. Isolation
comes from separate bridges without uplinks and the trusted gateway rules.
Verify bridge IPv6/link-local host exposure is disabled or blocked through the
worker's managed configuration. Test attempted cross-world and host access.
[KubeVirt network configuration](https://kubevirt.io/user-guide/network/interfaces_and_networks/)
provides secondary interfaces and link-state controls; qualify their exact
behavior with the installed CNI versions.

Reserve each world's peak concurrent guest resources plus runner, gateway,
launcher and storage-helper overhead before admitting it. Configure requests,
quotas, worker selectors and a low-priority test workload class that does not
preempt production. Initial concurrency is one world per eligible worker,
further reduced by measured headroom. Node drain or loss fails and preserves the
attempt; it does not silently migrate or restart it. Release the slot only after
all old attachments and writers are gone. An expired Lease alone cannot authorize
reuse of a bridge that still carries a retained or orphaned world.

## Security and CI ownership

Treat tested source, build hooks, guest images and test binaries as executable
input. A namespace and an ephemeral runner do not by themselves make arbitrary
public pull requests safe on a home cluster. GitHub explicitly describes the
[self-hosted runner risk](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions).

Use the following trust boundary:

| Actor | Authority |
| --- | --- |
| Protected CI provisioning workflow | Allocate a slot, create the namespace and policy from a pinned trusted implementation; issue bounded credentials. Never execute PR code with this identity. |
| Ephemeral test runner | Manage only its world's VMs, DataVolumes/PVCs/snapshots, approved helper Jobs and diagnostics; use only required VM subresources. |
| Guest | No hosting-cluster credentials, service-account disk, registry push token or runner-registration token. Its Kubernetes credentials belong only to the disposable guest cluster. |
| Cleanup job | Enumerate owned allocations, fence expired runners, collect evidence and remove only UID-verified resources. |
| Release publisher | Separate protected job outside the test trust domain; verifies qualification identity before obtaining publication/signing authority. |

Install the runner controller/registration service through home-ops. Prefer
one-job ephemeral runners with a read-only repository token and no Docker socket,
host mounts or host privileges. ARC may manage ordinary runner pods; world
allocation and test Jobs remain explicit infrastructure operations. Do not add
cluster-admin credentials to a checked-out workflow or use `pull_request_target`
to execute arbitrary pull-request content. Unreviewed fork changes run on hosted
infrastructure; self-hosted execution requires approval of the exact commit by
a trusted workflow. A push invalidates that approval. Approval admits code into
the stated trust domain; it is not a security sandbox for hostile code.

RBAC is necessary but insufficient: a VM/Pod creator could select a privileged
service account, mount a secret, reference another PVC, choose a dangerous NAD or
supply a hook container. Admission must constrain service accounts, volumes,
images, attachments, node placement, resources and VM annotations. Deny hostPath,
host namespaces, device passthrough, arbitrary hooks, migration, user-selected
privilege escalation and attachment to other namespaces. Account explicitly for
KubeVirt/CDI controller-generated resources; generic restricted Pod Security
settings may not admit launcher pods. Qualify the narrow platform exemptions and
keep user-controlled workloads outside them. Runner RBAC cannot grant itself
Roles, tokens, admission exemptions or namespace changes.

Generate explicit namespaced RBAC rather than wildcard API access: lifecycle
verbs on VMs and owned storage/Jobs; read/watch on VMIs, pods and events; pod-log
access; and the pinned API's required start, stop, console and vsock subresources.
Keep ordinary pod exec, secret listing, node access and cluster-scoped writes out
of the runner role. The provisioner supplies fixed helper templates and admission
restricts their use. Test denied operations as well as successful VM creation;
subresource permissions and controller behavior must be verified against the
chosen version rather than copied from an administrator role.

Use short-lived namespace-scoped credentials; put no long-lived kubeconfig in
artifacts. Secrets, handoff bundles, kubeconfigs and serial output require
restricted access and redaction. Separate trusted release caches from PR caches.
Do not permit PR jobs to overwrite content-addressed artifacts used for release
qualification. Self-hosted jobs have no release publication credentials.

For initial rollout, keep artifact builds on the existing producer and offload
VM execution. To farm mkosi builds onto the cluster, add an independent disposable
builder VM with a pinned tools image: transfer the selected source, run
`scripts/mkosi` inside that VM, verify/export the artifact manifest, then destroy
its writable disks. Privileged container operations, if needed by mkosi, occur
inside that VM, not on a worker Docker socket or privileged runner pod. The trusted
orchestrator owns the builder's lifecycle; build input receives only per-attempt
artifact write access. KubeVirt itself is not a remote build API. Build offload
must independently prove reproducibility, cache isolation and cleanup before
replacing the producer. It does not need nested KVM to run the build.

## Failure, retention and recovery

Persist a small allocation record before remote creation: run/attempt ID,
namespace UID, slot lease generation, profile/artifact digests and expiry. Use
native owner references for namespaced children and record UIDs of indirectly
created resources. Avoid a custom test CRD/controller initially. One trusted Go
provisioning/cleanup tool runs as Jobs; a scheduled janitor handles abandoned runs.

Create/adopt operations verify ownership and desired-spec digest. A timed-out
create is resolved by observing the same resource identity before retrying.
Conflicting ownership fails closed. Local result files are evidence, not the sole
inventory needed to clean up after the runner disappears.

On success, collect results, stop VMs, verify launchers and volume writers have
terminated, delete world resources, and release the slot. On failure, first
capture VM/VMI status, events, launcher logs, serial, guest diagnostics when
available, storage conditions and allocation identity. Preserve the original
scenario error even if diagnostics fail. Report cleanup failures separately and
leave the allocation discoverable by the janitor.

Default failure retention preserves stopped disks/state and small artifacts for
a bounded period. Explicit live-debug retention also keeps the network and slot
until its expiry; no later test may reuse them. Define the profile's maximum
retention and total storage budget, and print the expiry with debug guidance.
Successful worlds use delete-on-success. Never upload multi-gigabyte guest disks
by default or auto-delete a failed world merely to rerun a test.

Cancellation/SIGKILL/token expiry is handled by the independent janitor. It revokes
or fences the old identity, checks ownership/UID and expiry, stops writers, then
deletes. A namespace deletion request is not proof that cleanup completed:
finalizers, PVCs and attachments can remain. Do not strip storage finalizers or
force-delete a launcher on an unreachable worker merely to reuse a slot; quarantine
it until infrastructure fencing proves the old writer cannot return.

`vmtest-debug` and `vmtest-clean` dispatch from the manifest's backend identity.
They report usable commands/targets for retained KubeVirt objects and preserve
optional fields. Repeated cleanup is safe; cleanup must reject a name reused by
a different UID. Keep recovery access to GitHub, artifacts and the hosting
cluster outside that cluster's own test dependency chain.

## Acceptance and release promotion

Keep the stable `VM validation` check and conservative selection owned by
`katl-vmtest-plan`. The backend is a workflow/profile input, not a second scenario
catalog. Source qualification requires an explicit Go JSON pass for every
selected test and success of setup, producer health and evidence collection;
skipped, canceled, missing or capability-failed selected jobs cannot pass.
Preserve selected-test accounting on both backends during comparison.

The source pipeline remains:

```text
trusted selection -> build instrumented inputs once -> verify identities
  -> backend world jobs -> aggregate selected-test outcomes -> VM validation
```

The release pipeline adds an independent gate:

```text
build production candidate once (standard and LTS; test support disabled)
  -> immutable candidate manifest and digests
  -> install/boot/manage/upgrade/rollback those exact artifacts in fresh worlds
  -> protected verifier matches candidate, suite, attempt and passing evidence
  -> publication approval -> publish those same bytes (no rebuild)
```

The production suite uses public `katlctl`, SSH and OS/Kubernetes observations;
it must not inject the vmtest agent or test sysext into the candidate runtime.
Reuse lifecycle/transport infrastructure but keep agent-dependent assertions in
the instrumented suite. Verify artifact absence of test support independently.
Include ISO installation, supported network-install media, installed access,
configuration/reboot, host upgrade from a supported prior release, health
promotion, rollback and durable workload/identity state. Enumerate the supported
flavor/version paths in a versioned release matrix. A newly unsupported path
requires an explicit release decision, not a backend capability skip.

Qualification evidence binds source SHA, candidate digests, test/harness revision,
profile and KubeVirt versions, required scenario matrix, run attempt and outcomes.
A trusted publisher obtains workflow/job conclusions independently and verifies
that the evidence covers the exact candidate and required matrix; a JSON file
written by a test is not sufficient authorization. A failed attempt remains in
the record; reruns receive fresh worlds and attempt identities. Changing any
candidate bytes invalidates prior qualification. Retain small release evidence
with the release rather than relying on short-lived Actions logs.

Keep publication approval until this production suite exists and is proven.
Also retain the repository's separate public `katlctl` journey in a persistent
`katldev` VM for user-facing changes. A KubeVirt CI pass does not replace it.

## Implementation sequence and exit criteria

1. **Qualify infrastructure assumptions.** In an explicitly provisioned throwaway
   namespace, pin a compatible Kubernetes/KubeVirt/CDI/client/CNI/CSI version set.
   Install through home-ops, then prove hardware KVM, network isolation, direct
   LAN access, serial-from-boot, plaintext guest vsock through the authenticated
   API, Ceph import/clone, and persistent firmware. Record manifests and outcomes
   in the implementation Bead. No default-CI change yet.
2. **Extract the boundary with libvirt only.** Separate machine intent and resource
   references; move lifecycle policy from shell into Go; remove direct backend
   calls from scenarios, node helpers and debug tools. Keep concrete libvirt
   options for `katldev`. Run the full existing libvirt suite before adding the
   remote implementation so refactor failures remain distinguishable.
3. **Implement remote resources and lifecycle.** Add the KubeVirt backend, scoped
   allocation tooling, media import, exclusive inspection, vsock transport, serial
   capture, diagnostics, cleanup and janitor. Test partial creation, cancellation,
   API disconnection, UID replacement, retention and storage detachment before
   enabling installed-world fixtures.
4. **Prove complete scenario parity.** Run the same boot, first-install, config,
   management, update, two-node, three-node install, multi-control-plane recovery,
   Kubernetes upgrade and DRBD tests that are applicable in the full repository.
   The hosted subset alone is insufficient. Inventory tests and requirements in
   one machine-readable catalog; require all applicable tests to pass on both
   backends. Resolve capability gaps rather than keeping backend-specific skips.
5. **Compare CI and cut over.** Run KubeVirt as a non-required shadow check first.
   Prove fork/approval rules, no-secret execution, failed-run evidence, janitor
   recovery and capacity limits. After parity and measurements, select it for
   required checks while retaining a tested libvirt fallback profile. Fallback
   requires a new explicit run and equivalent coverage; it never converts the
   failed KubeVirt attempt into success.
6. **Offload builds and qualify releases.** Implement isolated builder VMs if the
   measured producer cost warrants them. Separately implement the exact-artifact
   suite and protected publisher verification. Only then consider removing manual
   publication approval. Version/profile upgrades rerun backend conformance and
   representative production qualification before becoming defaults.

For changed code, `go test ./...` remains the baseline. Add direct contract tests
that can falsify incorrect ownership and lifecycle behavior, rather than asserting
identical API call sequences. Test both backend implementations against shared
contracts where possible. Useful discriminators include:

- Two clones diverge while their base and a retained failure remain unchanged.
- A guest mutation followed by lost transport is not executed twice.
- A replaced UID or expired allocation cannot be adopted or deleted by an old run.
- A guest reboot preserves required state and changes boot ID; a stop/start
  preserves firmware variables without silently substituting a fresh machine.
- A canceled runner leaves recoverable inventory; repeated cleanup removes only
  its resources, and a failed detach prevents slot reuse.
- A disconnected NIC does not block the working interface; guests cannot reach
  another world, the host, trusted LAN, metadata endpoint or hosting-cluster API.
- An unavailable agent still leaves early serial and platform diagnostics.
- Missing/skipped tests, forged candidate digests and stale attempt evidence
  prevent promotion even when another job reports success.

Run actual workflows through `scripts/vmtest-run ... -count=1`, using the proposed
backend flag for KubeVirt after it exists. Observe disks, EFI state, Linux/systemd
and Kubernetes outcomes independently through supported interfaces. Unit tests
and API-object rendering do not qualify a hypervisor backend. Infrastructure
implementation changes require focused security/storage review and the relevant
VM gates; record exact commands and capability gaps in Beads.

## Measurement and unresolved gates

Benchmark identical artifacts and scenario selections on hosted libvirt, local
libvirt where useful, and KubeVirt. Collect cold and warm runs separately: queue,
producer, upload/download, import, clone, scheduling, boot, test and cleanup time;
p50/p95 whole-gate duration; failure rate; CPU-seconds, peak guest/launcher memory,
Ceph allocated bytes/IO and network transfer. Include idle capacity, replicated
storage, electricity and maintenance in cost per passing gate. Avoid concurrent
benchmark load that makes the environments incomparable.

Before cutover, agree on a gate-latency budget and concurrency policy using those
measurements. No price, speedup or available-capacity claim is made by this ADR.
Retain the hosted option if total cost or reliability is worse.

The following are explicit implementation blockers to qualify, not assumed
platform guarantees:

- The exact version set and feature gates, actual worker KVM support, and capacity
  for the largest world alongside production reservations.
- Early serial recovery, cancellation of API streams, and vsock RBAC/client
  behavior with the existing non-TLS guest agent.
- Persistent EFI variables relevant to Katl boot selection, disk-first ISO
  fallback, raw squashfs import and fresh-firmware fixture boot.
- Ceph StorageProfile access modes, import/clone performance and writer fencing.
- Secondary bridge isolation, IPv6 host exposure, gateway egress enforcement and
  admission rules for controller-generated pods.
- Complete production-artifact qualification without test-agent injection.

If these reveal that the selected network, firmware or storage model cannot
preserve the contract, revisit that ownership decision before layering custom
hypervisor hooks or special-case scenario logic onto the backend.
