# Boot Attempt and Health Semantics

This decision defines the first health contract for deciding whether a Katl
generation booted successfully.

## Decision

Katl treats a generation as successful only after the runtime reaches a Katl
health target that is ordered after the node's required local services and
before update state is marked good.

Initial success signal:

```text
katl-boot-complete.target reached
```

The target is generation-scoped. The required local services depend on the
selected generation's capability profile.

Management-network readiness is also local and generation-scoped. For a
generation that has yet to pass boot health, Katl reads systemd-networkd's
effective link state. Every managed link with `RequiredForOnline` enabled must
have carrier and report `OnlineState=online`, which reflects the link's
operational range and address-family policy. At least one required link must
be operationally `routable`.

| Minimum | Address state required in addition to carrier |
| --- | --- |
| `carrier` or lower, or `enslaved` (the default for bond ports) | Any |
| `degraded` | `degraded` or `routable` |
| `routable` | `routable` |

When networkd reports an explicit `RequiredOperationalStateForOnline` range,
Katl also checks the minimum against `AddressState`. If the range is omitted,
`OnlineState` reflects the device-specific default. The generated DHCP fallback sets
`RequiredForOnline=routable` for physical Ethernet. In native networkd
configuration, set `RequiredForOnline=routable` on the interface carrying the
management address if it must have a routable address. Bond and bridge ports
can qualify with carrier alone. Set `RequiredForOnline=no` for optional links.
Katl counts only required, managed links toward readiness.

Trial boot retries unreadable networkctl state within the same 90-second
deadline it uses for links that have yet to qualify.

### Required link without carrier in networkd

A required link can be left with systemd-networkd reporting no carrier while
sysfs reports carrier up (`carrier=1`, `operstate` up), and the link then gets
no DHCP lease ([#529](https://github.com/katl-dev/katl/issues/529)). That
state can begin at any time after the link comes up, on a trial boot or a
known-good one, so the response belongs to `katl-link-recovery.service`
(`katlc agent recover-links`, implemented in `internal/linkrecovery`), which
runs for the whole of every boot whatever the generation's boot-health state
([#811](https://github.com/katl-dev/katl/issues/811)). It polls networkd with
the backoff that `internal/managementnetwork` gives every watcher of the
management links, including the trial-boot wait, and the readiness snapshot
there reports each unready required link in that state. Required links are the
same set readiness counts: links networkd manages with `RequiredForOnline`
enabled. A link outside that set is never touched.

When the service first sees a link in that state, it starts the link's
20-second `katl-deadline-management-carrier@.timer` grace period and logs a
line naming the link and the grace length. If the state persists until the
grace period elapses, the service runs `networkctl reconfigure <link>` once.
Each link has its own grace period. If networkd reports carrier before the
grace period elapses, the grace period ends, and a new one starts only if the
state returns. When sysfs also reports no carrier, the link is not
reconfigured, and a link whose grace period cannot be started is not
reconfigured in that run of the service.

The record under `/run/katl/link-recovery/<link>` is the only account of an
attempt. The service claims it by exclusive creation before running
networkctl, then stores the outcome: reconfigured, or failed with networkctl's
error. A claim whose outcome was never stored reads as attempted with no
recorded result. The record limits each link to one attempt per boot,
including across restarts of the service, whose runtime directory is
preserved. Every report derives from the record's description, a sentence
that names the link once and ends with the next step: none while the node is
healthy after a reconfiguration, and otherwise inspecting the service's
journal. If the link stays offline, the description directs you to run
`systemctl reboot` from the node's console. The remote
`katlctl node reboot <node>` command requires a working management link.

- The service writes the description to its journal and console.
- Live boot health appends it to the boot-health diagnostic, so
  `katlctl node status` and the node console show a reconfiguration, whether
  it succeeded or failed, even when the node is healthy.
- If the link is still not online when the trial-boot management-link
  deadline expires, the trial fails as for any offline link, and the failure
  reason includes the description.

Neither boot-health path reconfigures links itself.

The VM cannot reproduce on demand the state in which systemd-networkd reports
no carrier while sysfs reports carrier up. The grace period is a deadline
timer, so a VM test can disable it with a drop-in and start the expiry unit
after the service logs that the grace period started. To present the state on
a healthy node, the VM test installs a drop-in that puts a test-owned
`networkctl` first in the service's `PATH`. It logs the calling process and
arguments of every call, prints a recorded link state for `networkctl list`,
and runs the real `/usr/bin/networkctl` for everything else, so the
reconfiguration runs unchanged under the shipped sandbox. The test asserts
that only the first process of the service reconfigured the link, once, and
that the process had no effective capabilities. The service has no test-only
input; the drop-in and the wrapper exist only under `/run` and
`/var/lib/katl/test-artifacts` while the test runs.

Systemd owns required-service qualification. The boot-health unit has explicit
`Requires=` and `After=` dependencies for Katl's baseline management services,
and selected system extensions add generation-scoped requirements through
`RequiredBy=katl-boot-health.service`. A required dependency failure prevents
the boot-health command from running successfully. An unrelated failed unit is
reported through ordinary systemd and Katl status but does not invalidate the
generation.

Volume activation is the exception. `katl-volumes-activate.service` refuses to
mount a volume whose recorded binding no longer matches the attached disks, as
described in `docs/internal/storage-identity.md`. That refusal reflects what is
attached, not the generation: failing boot health for it would mark a sound
generation failed, could move the boot default to an older generation that
cannot mount the volume either, and would block the configuration apply that
rebinds a replaced volume. Boot health therefore orders after the unit with
`Wants=` instead of requiring it. Node status reports boot health `failed`
while the unit is failed in the running boot, and the volume status names each
unmounted volume, the cause and the remedy.

Required-service dependencies apply on every boot. During an ordinary boot of a
known-good generation, the boot service can complete while a management link is
down, preserving the generation as a local fallback. After boot health settles,
`katlctl node status` and the operator console read current-boot health using
the same networkd criteria as trial boot health. Both report unhealthy boot
health, name the offline required link and show repair guidance. After the link
recovers, both show the restored boot health if the other checks pass. These
observations leave the boot selection and generation records intact, so
operators can still apply a repair or request a reboot.

For generation 0, "machine identity available" means PID 1 received the
install-generated value through `systemd.machine_id=`, `/etc/machine-id`
resolves to that same value during runtime, and
`/var/lib/katl/identity/machine-id` persists the same 32-character lowercase hex
ID. A mismatch is a boot-health failure.

Generation 0 installed-runtime profile:

```text
state partition mounted at /var
selected baseline sysext/confext activation completed
machine identity available
network configuration loaded
sshd started when enabled
katlc-agent.service running
katlc agent startup audit of operation state completed successfully
```

Kubeadm-ready profile, after the bootstrap or join operation asks `katlc` to
create and activate the Kubernetes-capable candidate generation:

```text
selected Kubernetes sysext active
kubeadm input rendered under /etc/katl/kubeadm
/etc/kubernetes projected from writable state
containerd active and CRI socket available
kubelet installed and ordered for kubeadm use
katlc agent startup audit of operation state completed successfully
```

Kubernetes-upgrade profile, after an explicit upgrade operation reaches the
planned target-kubelet activation phase:

```text
target Kubernetes sysext active
kubeadm upgrade phase complete for the node role
kubelet activation gate released
kubelet running from the target payload
local role checks passed
katlc agent startup audit of operation state completed successfully
```

The first implementation can keep the target conservative and local. It does
not need to prove full Kubernetes control-plane convergence before marking the
OS generation good.

For the first Kubernetes-capable generation, local kubeadm-ready health is not
enough to commit the generation. The bootstrap or join operation commits the
candidate only after kubeadm succeeds and post-kubeadm health checks pass.
Those post-kubeadm checks are operation health checks, not
`katl-boot-complete.target` boot health. When they pass, the operation promotes
the live-activated generation as good and healthy and makes it the persistent
EFI default without a trial reboot.

Kubelet is only started before boot health when the selected generation
explicitly enables that policy.

## State Storage

Generation status is stored separately from immutable generation selection:

```text
/var/lib/katl/generations/<generation-id>/status.json
```

`status.json` is bound to immutable
`/var/lib/katl/generations/<generation-id>/spec.json` by `specDigest`. Boot
health updates only `bootState` and `healthState`; it does not change generation
selection fields.

Commit values:

```text
candidate
  validated generation spec exists, but it is not accepted as persistent desired
  host state

committed
  accepted desired host state; persistent default boot selection is tracked
  separately in /var/lib/katl/boot/selection.json

superseded
  previously committed generation replaced by a newer committed generation, but
  still eligible as a rollback target if boot health remains good

abandoned
  candidate rejected or failed before it became committed
```

State values:

```text
pending
  created but not selected for boot yet

trying
  selected for the next boot and not yet marked healthy

good
  reached katl-boot-complete.target

failed
  exhausted attempts or explicit health failure

```

Health values:

```text
unknown
  no health verdict yet

healthy
  local boot-complete target reached

unhealthy
  local boot-complete target failed or timed out

deferred
  health policy deliberately skipped for a debug or recovery boot
```

## Current Boot Identity

`/var/lib/katl/boot/selection.json` records `bootedGenerationID` and
`activeGenerationID`. Management reports `activeGenerationID` as the node's
current generation and checks `expectedCurrentGenerationID` against it, and the
agent starts before boot health runs. The first `katl-generation-activate` run
in each boot therefore records the kernel-selected generation as both booted
and active. Activation runs again whenever `systemd-sysext.service` restarts, so
the first run writes `/run/katl/boot-health/current`, and later runs in the same
boot keep the active generation, which a live promotion may have replaced.
Because the agent also starts after a failed activation, `bootedGenerationID`
alone can describe an earlier boot. At startup the agent resumes a live
promotion interrupted in an earlier boot only when this boot's
`/run/katl/boot-health/current` record names the candidate. The
record relies on `/run` being cleared by every boot; KatlOS does not support
systemd soft-reboot, which would keep it.

## Management During Boot Health

Boot health resets the default to the booted generation and rejects a boot whose
pending trial changed underneath it, so the agent holds mutating requests
(`SubmitOperation` other than dry runs, `SelectGeneration`, `RemoveGeneration`)
until boot health settles for the current boot. `katl-boot-health-settled.target`
wants and orders after `katl-boot-health.service` and `katl-boot-recovery.service`,
so it is reached when boot health succeeds, fails, or cannot run because a unit
it requires failed to start. Its `JobTimeoutSec=10min` bounds a hung
dependency. The agent cannot order itself after the target, because boot health
requires the agent, so it reads the target's start job from systemd at request
time. systemd state is scoped to the running boot and covers the whole
boot-health service, including configuration-apply bookkeeping after the
selection update.

A held request returns `Unavailable` after five minutes, or two seconds before
the caller's deadline, so clients see the retry guidance within their own
timeouts. Node status reports `pending` while the target has a start job or
boot health is still running after the target gave up, and `failed` when boot
health failed or did not run. A boot whose health cannot run still settles, so
an operator can push a repair or select another generation. Node status names
required units that systemd still reports as failed alongside any volume or
generation diagnostic. If no required unit still shows as failed, status directs
the operator to `journalctl -b -u katl-boot-health`. Reads, reboot, and shutdown
are never held.

## Promotion Rules

`good` means the node booted with that generation selected and reached
`katl-boot-complete.target`, or that live promotion accepted it. `healthy` in
generation status is set only by the boot health path, live promotion, or
explicit repair tooling that records why boot health was accepted.

Live promotion applies to a generation whose complete runtime was activated in
the current boot and passed its operation's live health checks: live
configuration apply, Kubernetes bootstrap and join, and Kubernetes-only
upgrades. It marks the generation good and healthy and makes it the persistent
EFI default immediately. Only next-boot apply and host upgrade arm a one-shot
trial that must pass the boot health gate.

## Generation bootability

`internal/generation` owns bootability evidence. It matches the spec's PARTUUID
to a partition on the running system disk through sysfs. A partition enumeration
or clone scan error makes the match indeterminate. For a full check, the verifier
opens the bound partition's kernel device node, which it resolves by disk
attachment sequence and partition number.

The verifier hashes the padded SquashFS image at the start of the partition.
Katl's runtime builder uses the default 4 KiB image padding, so the verifier
rounds the superblock's `bytes_used` up to the next 4 KiB boundary. An
implausible extent, including a padded image larger than the partition, is
indeterminate. Deriving the extent keeps the partition tail out of the digest
and works with existing generation specs. Install and upgrade write verification
use the build's recorded artifact length to verify the write they just made.

The same check requires the loader entry and UKI on the ESP and each selected
sysext, bundled confext, and generated confext at its recorded path. Activation
still verifies extension content digests when the kernel-selected generation
starts. This gives manual selection three outcomes:

- **Bootable** means the root digest matches and the recorded boot artifacts
  are present. Manual selection also requires known-good health.
- **Unavailable** means the check found evidence such as an absent PARTUUID,
  a mismatched root digest, a missing artifact, or prior slot invalidation.
- **Indeterminate** means Katl could not complete the check, for example because
  reading the root returned an I/O error. Katl reports the cause and protects
  the record from explicit removal and pruning while the check is indeterminate.

`katlctl node generations list` checks recorded state and artifact presence
without reading the root image under the submission and boot-state locks.
Manual selection, removal, and pruning verify the root digest. Manual selection
checks the request deadline before publishing the selection, and each EFI write
and compensation has a bounded timeout independent of the request.

Next-boot configuration and host upgrades verify the known-good fallback
before writing EFI for a trial. Reboot admission rechecks the staged target and
its fallback with the full check. It uses the next entry reported by `bootctl`
and verifies that it agrees with the effective EFI variable, where a one-shot
takes precedence over the persistent default. If both variables are absent on
readable, mounted efivarfs, the default reported by `bootctl` is sufficient.
An unmounted efivarfs or an unreadable variable makes the entry indeterminate.
A `preferred` key in the ESP's `loader/loader.conf` also makes the entry
indeterminate because firmware can apply it ahead of the persistent EFI default
that `bootctl list` reports. KatlOS does not write this key.
A successful staging receipt can still report a bootability problem if the
target changes after staging. The agent refuses to schedule the reboot when it
cannot verify the target.

Boot activation does not verify the fallback. Its sandbox has no access to
the root device and cannot write EFI variables, so it reconciles an outstanding
move only as far as sysfs and the ESP allow, defers the rest, and binds a trial
authority to the boot when the running generation is the trial's candidate. The
fallback's known-good health, bound root PARTUUID, loader entry, and UKI are
verified when the trial is armed and again when a failure selects it. Slot
replacement also checks the running generation's loader entry and UKI before
redirecting EFI and allowing the overwrite.

A host upgrade runs from any supported release, including one that cannot read
boot-move records; the boot-state owner writes the selection in a form every
bootable generation's release decodes, by the rule in
[Boot selection transaction](boot-selection-transaction.md#boot-move-owner).
A trial armed while the running generation
is not the published default is refused unless the default generation has run a
release with the owner, which it records as a marker file under
`/var/lib/katl/boot/owners/` when the owner publishes that default. The default
gets a marker when it is the running generation, or boots the same runtime
artifact as the running generation, as a live promotion does. The fallback is always the
published default, and a fallback that has not run the owner would not complete
the records a failed trial leaves.

The boot selection transaction decision describes the
[intent format, trial authority, and reader-only runtime](boot-selection-transaction.md#durable-boot-move-intent).
EFI reconciliation is tracked in [#733](https://github.com/katl-dev/katl/issues/733).

## Attempt Count

The initial update policy allows one attempted boot for a new generation. The
candidate generation must be tried with a bounded boot mechanism: keep the
previous known-good generation as the default and select the candidate with
systemd-boot one-shot state, or use explicit boot counting when that is wired and
tested.

If the candidate does not reach `katl-boot-complete.target`, the next boot must
return to the previous known-good generation instead of repeatedly booting the
candidate.

Later work may use systemd-boot boot counting for multiple attempts, but the
first policy should keep rollback behavior easy to validate in VM tests.
Systemd executes the bounded attempt; `katlc` records boot-selection state,
validates the boot result, and promotes or rolls back through the transaction
model. Rollback selection is defined in
`docs/internal/rollback-selection-rules.md`; transaction details are defined in
`docs/internal/boot-selection-transaction.md`.

## Failure on a known-good generation

Only a trial authority bound to the running boot can change boot state when
boot health fails. Every other failed boot reports health and changes nothing:
the generation status, the persistent default, the next boot, the EFI
variables, and the boot ID stay as they were, and no automatic reboot follows.
This includes a known-good generation booted from the boot menu or by manual
selection, a reboot of the default, and the boot of a manual selection that
fails. Katl clears a matching, consumed manual selection, whether one-shot or
persistent. A selection queued for a future boot remains in effect.

`katl-boot-recovery.service` reports the running generation, the persistent
default and the generation selected for the next boot, and the next action. It
exits unsuccessfully so systemd leaves the node running. `katl-boot-deadman.timer`
is not running on such a boot. When management is reachable, use
`katlctl node status <node>` to see the failed boot and
`katlctl node generations list <node>` to check the next boot. On the node, use
`systemctl --failed` to investigate. If remote management is unreachable,
use the console or out-of-band access. Fix the cause, for example by applying
corrected configuration with `katlctl cluster apply --config cluster.yaml --node <node>`,
then run `katlctl node reboot <node>`. Use `systemctl reboot` from the console
if management is still unreachable. A repair that stages a new generation
selects it for the next boot.

## Failed-trial recovery

Katl automatically reboots once after a supported trial fails. A next-boot
configuration apply or a host upgrade arms the trial: the boot-state owner
records a durable trial authority, keeps the source generation as the
persistent default, and selects the candidate for one boot. Activation binds
the authority to the first boot of the candidate after the arming boot and
then starts `katl-boot-deadman.timer`. The timer is not enabled at install and
runs only while a bound trial is undecided. If activation cannot start it, the
trial fails: activation completes, reports that it could not start the boot
deadline, and exits unsuccessfully so the failed boot health reaches the
recovery service, because a trial must never run without its deadline.

When the bound boot fails, `katl-boot-health` asks the owner to fail the trial.
A required unit that fails, a management link that stays offline for 90
seconds, or a boot that does not reach `katl-boot-complete.target` within 10
minutes all end this way. `katl-boot-recovery.service` handles the first two
and `katl-boot-deadman.service` the deadline. The owner validates a fallback
that is still `good` and `healthy` with its loader entry and UKI present and
that passes the full bootability check. It marks the trial `failed` and
`unhealthy`, publishes the fallback as the default, and verifies the EFI
variables. It then spends the trial's reboot eligibility durably, before it
publishes the selection, and requests the one reboot by exiting successfully;
the unit's `SuccessAction=reboot` performs it. Restarting either service
cannot request another automatic reboot, and a retry after a crash finishes the
same move. After the trial is decided, `katl-boot-health` stops the deadline
timer.

This mechanism supports failures after PID 1 has started the boot deadline and
`/var` and `/efi` are available. It includes an unreachable management network,
required-unit or generation activation failure, and a boot that stalls before
the health target. The candidate's journal, immutable generation record, status
transitions, and boot-selection record remain available after fallback.

Katl does not automatically reboot in these cases:

- No validated previous known-good generation exists. The owner publishes the
  running generation as the default with `recoveryRequired` and leaves the node
  running.
- The reboot was already used for this trial.
- The check of a fallback is indeterminate, for example because reading the
  root returned an I/O error. The trial stays pending and the boot target is
  unchanged.
- The fallback generation also fails or times out. The fallback is not a
  trial, so its failure is report-only.
- Firmware, the UKI, the root filesystem, PID 1, `/var`, or `/efi` fails before
  the recovery service can run.
- Boot metadata is missing, corrupt, or does not match the running root.

`recoveryRequired` is cleared only by a later healthy boot of the published
default, such as after the operator repairs the cause and runs
`katlctl node reboot <node>`, or of a known-good generation chosen from the
boot menu, which becomes the default. Until then node operations such as apply and
upgrade are refused with that step. The operator can instead boot another
generation from the boot menu or reinstall the node. Katl does not use
`FailureAction=reboot`, an unbounded service restart, or a hardware watchdog for
this policy because those mechanisms cannot prove a remaining known-good boot
source before resetting the node.

## First Install

First install has no previous known-good generation. The installer writes the
initial generation as:

```text
commitState: committed
bootState: pending
healthState: unknown
```

The first runtime boot transitions to:

```text
bootState: good
healthState: healthy
```

If first boot fails, generation 0 stays `pending` and `unknown`. Katl does not
set `recoveryRequired` and does not reboot automatically. Node operations such
as apply stay refused until a boot of the generation is healthy. Fix the cause
and run `katlctl node reboot <node>`, or use repair tooling or reinstall; A/B
rollback only applies after a known-good generation exists.

The first runtime boot of generation 0 is evaluated against the
installed-runtime profile. It must not wait for `/etc/kubernetes`, containerd,
kubelet, Kubernetes sysext activation, or `katl-kubeadm-ready.target`.

Activation failure is boot-health failure. If `/var` is unavailable, the
selected generation spec or status is missing or invalid, artifact digest
validation fails, `/run` activation links cannot be created, `systemd-sysext` or
`systemd-confext` fails, boot-time operation reconciliation fails, or
`/etc/kubernetes` cannot be projected for a kubeadm-ready generation, the
generation must not become `good` or `healthy`. katlc-agent is ordered after
activation without requiring it, so it still starts after a failed activation
and reports the failed boot health; its mutation hold ends when boot health
settles, as for any other failed boot.

## Out Of Scope

The first runtime health contract does not include:

```text
full kubeadm init/join success
Kubernetes API availability
cluster workload health
remote attestation
TPM measured boot policy
multi-attempt boot counting
automatic root cause classification
automatic recovery before generation activation binds the trial or required
state mounts are available
```

Those can be layered on later without changing the minimum rule: an update is
not good until a generation-scoped health signal marks the selected generation
healthy.
