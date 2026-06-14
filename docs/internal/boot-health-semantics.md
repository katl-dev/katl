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

Generation 0 installed-runtime profile:

```text
state partition mounted at /var
selected baseline sysext/confext activation completed
machine identity available
network configuration loaded
sshd started when enabled
katlc and systemd operation wiring available
```

Kubeadm-ready profile, after the bootstrap or join operation asks `katlc` to
create and activate the Kubernetes-capable candidate generation:

```text
selected Kubernetes sysext active
kubeadm input rendered under /etc/katl/kubeadm
/etc/kubernetes projected from writable state
containerd active and CRI socket available
kubelet installed and ordered for kubeadm use
```

The first implementation can keep the target conservative and local. It does
not need to prove full Kubernetes control-plane convergence before marking the
OS generation good.

For the first Kubernetes-capable generation, local kubeadm-ready health is not
enough to commit the generation. The bootstrap or join operation commits the
candidate only after kubeadm succeeds and post-kubeadm health checks pass.

Kubelet is only started before boot health when the selected generation
explicitly enables that policy.

## State Storage

Boot attempt state is stored in the selected generation record:

```text
/var/lib/katl/generations/<generation-id>/metadata.json
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

superseded
  healthy generation no longer selected because a newer healthy generation
  replaced it
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
`katl-boot-complete.target`. `healthy` in generation metadata is set only by the
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
Rollback selection is defined in
`docs/internal/rollback-selection-rules.md`.

## First Install

First install has no previous known-good generation. The installer writes the
initial generation as:

```text
bootState: pending
healthState: unknown
```

The first runtime boot transitions to:

```text
bootState: good
healthState: healthy
```

If first boot fails, repair tooling or reinstall is required; A/B rollback only
applies after a known-good generation exists.

The first runtime boot of generation 0 is evaluated against the
installed-runtime profile. It must not wait for `/etc/kubernetes`, containerd,
kubelet, Kubernetes sysext activation, or `katl-kubeadm-ready.target`.

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
```

Those can be layered on later without changing the minimum rule: an update is
not good until a generation-scoped health signal marks the selected generation
healthy.
