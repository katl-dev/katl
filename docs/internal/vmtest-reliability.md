# VM Test Harness Reliability

Status: decided design; migration in progress.

This document defines how the VM test harness (`scripts/vmtest-run`, `internal/vmtest` and the hosted VM workflows) produces gate results that are trustworthy and attributable to the code under test. That must hold on a developer host shared by several concurrent runs and on contended hosted CI runners.

## Requirement

A VM gate's pass or fail describes the code under test. Infrastructure behaviour must not produce a result that reads as a product failure. Runs wait for each other only when they compete for a genuinely scarce resource.

The harness's recurring failures came from a few design choices rather than isolated bugs:

- **Serial parsing as control flow.** Hooks matched partial serial lines (#447), and a 45-second serial idle timer treated silence as death (#448).
- **One mutable build directory** shared by builds and running gates. Builds ran before the host lock, `scripts/mkosi` deleted and regenerated artifacts in place, and mkosi's caches were shared across every worktree without a lock (#514).
- **One host-wide lock** serialising every VM run, because domains, networks, addresses and vsock CIDs were not owned per run.
- **Fixed wall-clock timeouts** that contended runners tripped (#487).
- **Evidence deleted on success**, so a passing run left no proof of which tests ran.

## Invariants

1. A run consumes only artifact bytes bound to it before its first VM starts.
2. Every libvirt resource belongs to a run. Cleanup touches only resources whose owner is provably dead.
3. The harness reserves each guest's address, and serial output is evidence, never control flow.
4. Liveness is the next event a phase is owed, from a native source. Silence is not a stall.
5. A run waits only for vCPU and memory that are actually exhausted.
6. Every run keeps a compact record that proves which tests ran on which artifacts, and how each ended.
7. The run environment is built from an allowlist, with `HOME` and XDG directories inside the run.
8. The harness never retries. The merge queue is the gate of record for the full regression suite.

## Artifacts

`scripts/mkosi` owns building. It holds one exclusive lock for its whole invocation, covering the checkout's build directory and the mkosi caches shared across worktrees. Because the builder owns the lock, `scripts/vmtest-run` and `katldev` cannot disagree about it. `scripts/mkosi` publishes every artifact by writing a temporary file and renaming it into place, and never writes into an existing artifact. A build that ends with an expected artifact missing fails and names it.

Each run pins its artifacts before its first VM starts. `katl-resource-lock prepare-mkosi`, which already records digests, places them in `<run>/artifacts` and makes them read-only. It tries these methods in order:

1. **Hardlink**, when the run directory is on the same filesystem as `_build/mkosi`. This is safe only because the builder publishes by rename: a later build replaces the directory entry, and the pinned inode keeps the bytes the run bound.
2. **Reflink** (`cp --reflink=always`), on filesystems that support copy-on-write clones.
3. **Full copy** in any other case.

`world.json` names only pinned paths. The run's digests are recorded in `run.json`. The pin lasts exactly as long as the run directory, so it needs no store, garbage collection or reference counting. CI extracts the producer job's artifacts and pins them the same way. Release builds never read pinned run artifacts.

## Addressing

Each scenario creates its own libvirt network, with an IPv4 /24 chosen collision-free by the same subnet chooser katldev uses. The network lives for the duration of the scenario. Every planned node is reserved by MAC address, so the planned address is the address the guest receives. The DHCP dynamic range is a small tail disjoint from the reservations, for guests that are deliberately unreserved. dnsmasq's DHCP logging is enabled through libvirt's dnsmasq options, so the run record shows whether a guest sent a DHCPDISCOVER and whether an offer left.

`OwnedNetwork` is the single owner of networks and reservations for both katldev and vmtest.

| Purpose | Transport |
| --- | --- |
| Harness control and observation (test agent, PID 1 notifications, lifecycle, lease state) | vsock and libvirt, with no IP |
| Guest product networking | IPv4 DHCP on the owned network, reserved by MAC |
| Installer endpoint | IPv4 at the reserved address, through the product handoff API |

Product journeys keep exercising IPv4 DHCP, the common home-lab path, so a gate fails when an operator would be stuck. IPv6 install coverage is a separate product scenario.

## Ownership and admission

`scripts/vmtest-run` holds a lock on `<run>/lease` for the whole run. Domains, networks and volumes carry the run ID and lease path in libvirt metadata, read by one metadata parser. The sweep at run start reaps only resources whose lease is free. vsock CIDs are assigned by libvirt with `cid auto='yes'` and read back after the domain starts.

Admission replaces the host lock. A scenario reserves its declared nodes' total vCPU and memory at once against the host's CPUs and memory, minus a reserve. Each reservation is a file held under a lock, so a killed process frees its reservation and the ledger is derived from the files that remain. A demand the host can never meet ends immediately as inconclusive rather than waiting.

Admission also caps a scenario by the memory the host reports as available, minus the same reserve, so VMs that no admission accounts for, such as persistent `katldev` worlds, are not oversubscribed. Each attempt first sweeps the libvirt objects of finished runs, so capacity freed by a killed run is reused only after its domains are gone. A scenario that cannot be admitted within `KATL_VMTEST_ADMISSION_TIMEOUT` (45 minutes by default, below the `go test` timeout) fails with the list of holders. Package parallelism (`-p` above 1) is refused for leased runs, because admission treats scenarios of one run as nested rather than concurrent.

Libvirt objects created before leases existed carry no lease metadata, and no sweep reaps them. Remove them once by hand: list them with `virsh -c qemu:///system list --all`, `net-list --all` and `vol-list` for the vmtest pool, then `destroy` and `undefine --nvram` each stale `katl-` domain and `net-destroy`/`net-undefine` each stale network. Tracked in [#683](https://github.com/katl-dev/katl/issues/683).

## Progress and timeouts

Each phase waits on one event it is owed, with a deadline:

- a libvirt lifecycle event;
- the DHCP lease on the owned network;
- the installer handoff API answering at the reserved address;
- PID 1's notifications over vsock, through systemd's `vmm.notify_socket` credential;
- the test agent connecting, on vmtest images only.

PID 1 forwards only its own state, not a service's progress, so installer readiness comes from the reserved address and an API probe. Only one console scenario asserts the installer's announcement line, as a literal string, because operators read it.

`go test -timeout` remains a ceiling, counted from admission. Admission waits never count toward a test's deadline.

## Outcomes and evidence

A run ends as **pass**, **fail** or **inconclusive**:

- **Inconclusive** requires positive evidence that the host withheld promised resources: flat vCPU time, steal or pressure above threshold, or admission never satisfied. Absence of evidence is a fail.
- **Harness defects count as fails.** The harness is code under test in this repository. Whether a failure is in the product or the harness is a diagnosis recorded in the evidence; it does not change the outcome.

Host health is recorded first, and runs are classified only once thresholds are calibrated on CI data. At that point a merge group whose result is inconclusive may be rerun once on a fresh runner. That exception needs explicit wording in AGENTS.md.

`scripts/vmtest-run` records `host-health.json` on every run: host load, available memory, CPU steal, and CPU and memory pressure over the run, plus clocksource watchdog and stall messages from guest serial logs. The same file carries a provisional verdict. A run that did not pass is inconclusive only when go test ended in its own timeout panic, no test reported a failure, and the health shows steal or pressure above threshold or a guest clocksource watchdog message. Every other failure is a fail, including assertion failures on a starved host. The thresholds are not yet calibrated, and nothing acts on the verdict. Per-guest vCPU time is not yet recorded.

Every run keeps `_build/vmtest/runs/<run-id>/`, containing:

- `run.json`, with the revision, a digest of any uncommitted changes, the artifact digests, admission wait, outcome and host health;
- `go test -json` output;
- per-scenario results;
- compressed serial logs.

Disks and VM directories are deleted on success, and retention is bounded by age. `scripts/vmtest-run` prints the run ID and each test's outcome. When `-run` names tests as an anchored alternation of plain names, the run fails unless each reported a pass, and any `-run` that selects no test fails. Go exits 0 in both cases. A gate result is cited by run ID.

## Contract tests

Each test names the wrong implementation it catches.

| Test | Catches |
| --- | --- |
| Two worktrees build concurrently with different package sets, and both finish complete | A per-checkout lock that leaves the shared cache unprotected |
| Rewriting `_build/mkosi` mid-run leaves the VM's bytes matching `run.json` | Recording a digest without binding it |
| Killing a build mid-publish leaves each artifact absent or complete | In-place writes |
| A serial signal split at every byte offset fires only after the newline | Substring matching on incomplete text |
| Two concurrent runs, one killed: its resources are reaped and the other's survive | A global or name-prefix sweep |
| Every node's observed lease equals its planned address, and an unreserved guest never receives a planned one | A plan dnsmasq does not know about; overlapping dynamic ranges |
| Budget of 4 vCPU against demands of 2, 2 and 2: the third waits; `kill -9` frees it; a demand of 6 ends inconclusive at once | Counting runs instead of reserving resources; leaked reservations; a disguised serial queue |
| A guest silent on serial for ten minutes while emitting events passes | Serial-based liveness |
| A throttled guest is inconclusive, and a guest with DHCP disabled fails | Wall-clock verdicts; a classifier that excuses everything |
| A passing run's record names its tests and digests, and does not match another run's ID | Delete-on-success; shared logs |
| A poisoned `HOME` and XDG directory is never read | Environment scrub lists that miss new variables |
| Release images contain no test agent | Test support leaking into published artifacts |

## Migration

These steps are required before v2026.11.2 so that its own gates are trustworthy:

1. Deliver only complete lines to serial hooks, and give the announcement parser one owner in `internal/installer/handoff` (#447).
2. Add the `scripts/mkosi` build lock over the shared caches and the build directory, publish by rename, and name any missing artifact (#514).
3. Keep the compact evidence record on every run, and print the run ID and per-test outcomes.
4. Apply any harness-side fix that the #448 investigation establishes.

These steps follow the release, as separate pull requests:

5. Build the environment from an allowlist, and add `go test -json` and `run.json` (done).
6. Pin artifacts per run. Then remove the AGENTS.md rule against building while a gate runs.
7. Add ownership: run leases, per-scenario owned networks with MAC reservations and DHCP logging, run-scoped names, libvirt-assigned CIDs, and an owner-scoped sweep. The host lock stays.
8. Add demand-based admission, then remove the host lock (done; the host lock protected only the capacity that admission now accounts for, since builds run under the `scripts/mkosi` lock and libvirt network names and subnets under the network creation lock).
9. Obtain the installer endpoint at the reserved address through an API probe. Remove serial scraping and the announcement parser, and add the literal console test.
10. Add owed-event deadlines, and remove the serial idle timer and serial control hooks.
11. Record host health, then classify outcomes, then enable the single rerun of inconclusive merge groups.

## Out of scope

- Self-hosted CI runners are a separate concern. The harness must fix its own root causes on any runner.
- The installer's silent 30-second wait for a DHCP address is a product defect, tracked separately (#521).
