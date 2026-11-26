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
generation that has not previously passed boot health, Katl reads
systemd-networkd's effective link state and requires:

```text
at least one networkd-managed link with RequiredForOnline enabled
every such required link in networkd's online state
at least one such required link in the routable operational state
```

The generated DHCP fallback marks its physical Ethernet links
`RequiredForOnline=routable`. Native networkd configuration may explicitly
exclude a secondary link with `RequiredForOnline=no`. Unmanaged workload links
and excluded secondary links cannot satisfy the routable-link requirement or
hide an offline required link. This check proves the configured local path is
usable; it does not depend on a particular workstation, gateway, or other
remote client answering a probe.

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

A generation already recorded as `good` and `healthy` does not repeat network
qualification during an ordinary boot. This preserves a known-good local
fallback during a transient network outage; required-service dependencies still
apply on every boot.

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

Boot activation verifies the fallback's known-good health, bound root PARTUUID,
loader entry, and UKI before granting boot-local reboot authorization. The
activation service runs with `PrivateDevices=yes`, so this check uses sysfs
for partition identity and relies on staging for root digest and extension
verification. If verification fails, activation reports the fallback problem
and continues checking the candidate. Failure selection checks known-good
health, the loader entry, and the UKI before setting the fallback as the
default. Slot replacement also checks the running generation's loader entry
and UKI before redirecting EFI and allowing the overwrite.

A trial staged by v2026.11.5 did not receive the full fallback check at
staging. On that upgrade, the boot-safe checks at activation can still arm
recovery if the fallback's root bytes are damaged but its PARTUUID, loader
entry, UKI, and known-good status survive. This matches the released behavior.
Every trial staged by this version receives full fallback verification at
staging.

The existing trial and boot-local authorization rules govern when recovery can
act. The durable boot-move intent and EFI reconciliation are tracked in
[#732](https://github.com/katl-dev/katl/issues/732) and
[#733](https://github.com/katl-dev/katl/issues/733).

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

When boot health fails on a generation that was already known-good, Katl
preserves that status and the EFI default unless the boot is armed for recovery
or the running generation is the pending trial or target. This also applies
when booting a superseded known-good generation from the boot menu or by
manual selection. Katl clears a matching, consumed manual selection, whether
one-shot or persistent. A selection queued for a future boot remains in effect.

The recovery service reports the running generation, the persistent default
and the generation selected for the next boot. It exits unsuccessfully so
systemd leaves the node running. When management is reachable, use
`katlctl node status <node>` to see the failed boot and
`katlctl node generations list <node>` to check the next boot. On the node, use
`systemctl --failed` to investigate. If remote management is unreachable,
use the console or out-of-band access. Fix the cause, for example by applying
corrected configuration with `katlctl cluster apply --config cluster.yaml --node <node>`,
then run `katlctl node reboot <node>`. Use `systemctl reboot` from the console
if management is still unreachable. A repair that stages a new generation
selects it for the next boot.

## Failed-trial recovery

Katl automatically reboots once after a supported trial fails. Before allowing
that reboot, `katl-boot-health` must validate that the previous generation is
still `good` and `healthy` with its loader entry and UKI present, restore its
boot entry as the default, mark the
trial `failed` and `unhealthy`, and persist `selection.json`. The
`katl-boot-recovery.service` unit then uses systemd's normal reboot transaction.
The boot-health deadline uses the same path when the candidate does not reach
the health target within 10 minutes. Generation activation creates the
ephemeral `/run/katl/boot-health/pending` authorization only when the running
generation matches the pending trial and its distinct known-good fallback
passes activation's bootability check. The fallback and ordinary boots do not
arm recovery. A successful boot removes the
authorization. The timer may remain scheduled, but its service is
condition-skipped without that authorization. Failure handling consumes the
authorization before systemd requests the one reboot, so restarting a service
cannot request another automatic reboot.

A pending trial that fails before recovery is armed can still record the trial
failure and restore the fallback as the default after the boot-file checks
pass. If the fallback is unavailable, Katl reports its evidence and keeps the
default. If its evidence is indeterminate, Katl leaves selection unchanged so
the operator can diagnose the read failure. Automatic reboot requires the
boot-local authorization.

This mechanism supports failures after PID 1 has started the boot deadline and
`/var` and `/efi` are available. It includes an unreachable management network,
required-unit or generation activation failure, and a boot that stalls before
the health target. The candidate's journal, immutable generation record, status
transitions, and boot-selection record remain available after fallback.

Katl does not automatically reboot in these cases:

- No validated previous known-good generation exists.
- The fallback generation also fails or times out.
- Firmware, the UKI, the root filesystem, PID 1, `/var`, or `/efi` fails before
  the recovery service can run.
- Boot metadata is missing, corrupt, or does not match the running root.

For a failed trial without a valid fallback, Katl sets `recoveryRequired` when
durable state is available. A fallback boot does not recreate the trial
authorization, so a failure follows the known-good policy above. Katl does
not use `FailureAction=reboot`, an unbounded service restart, or a hardware
watchdog for this policy because those mechanisms cannot prove a remaining
known-good boot source before resetting the node.

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

If first boot fails, Katl records recovery-required state when possible but does
not reboot automatically. Repair tooling or reinstall is required; A/B rollback
only applies after a known-good generation exists.

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
automatic recovery before generation activation arms the trial or required
state mounts are available
```

Those can be layered on later without changing the minimum rule: an update is
not good until a generation-scoped health signal marks the selected generation
healthy.
