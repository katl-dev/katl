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
`katl-boot-complete.target` boot health. The generation is not known-good until a
later boot reaches `katl-boot-complete.target`.

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

## Promotion Rules

`good` means the node booted with that generation selected and reached
`katl-boot-complete.target`. `healthy` in generation status is set only by the
boot health path or explicit repair tooling that records why boot health was
accepted.

A generation is not known-good when only live apply checks passed. A live-applied
generation selected for a later boot must still pass the normal boot health gate
before it becomes a rollback target.

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

## Failed-trial recovery

Katl automatically reboots once after a supported trial fails. Before allowing
that reboot, `katl-boot-health` must validate that the previous generation is
still `good` and `healthy`, restore its boot entry as the default, mark the
trial `failed` and `unhealthy`, and persist `selection.json`. The
`katl-boot-recovery.service` unit then uses systemd's normal reboot transaction.
The boot-health deadline uses the same path when the candidate does not reach
the health target within 10 minutes. Generation activation creates the
ephemeral `/run/katl/boot-health/pending` authorization only when the running
generation is the pending trial and its distinct fallback is still `good` and
`healthy`. The fallback and ordinary boots do not arm recovery. A successful
boot removes the authorization. The timer may remain scheduled, but its service
is condition-skipped without that authorization. Failure handling consumes the
authorization before systemd requests the one reboot, so restarting a service
cannot request another automatic reboot.

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

These cases set or retain `recoveryRequired` when durable state is available.
The fallback boot never recreates the trial authorization; if it fails, Katl
preserves the failed state and stops for console or out-of-band recovery. Katl
does not use `FailureAction=reboot`, an unbounded service restart, or a hardware
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
generation must not become `good` or `healthy`.

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
