# ADR-002: Runtime configuration supports live and next-boot apply modes

Status: accepted.

Date: 2026-06-05.

This ADR defines how later Katl runtime configuration changes choose between
immediate live application and staging for the next boot. It builds on
`adr-001-generated-confext-configuration.md` and
`supported-node-config-domains.md`. The input transport, trust roots, freshness,
and audit policy for those requests are defined in
`adr-003-runtime-config-input-and-trust.md`.

## Context

Katl renders trusted Katl-native node configuration into generated confext.
First install writes one generation and boots it. Later, an installed node also
needs a controlled path for changed Katl configuration without making Katl a
general-purpose configuration manager or a Kubernetes lifecycle controller.
Runtime apply still starts with Katl configuration. The installed node locally
compiles that trusted input into a new generation-scoped confext and selected
sysext activation set instead of accepting user-supplied extension trees.
`katlc` and KatlOS runtime services fail closed: unsupported domains, fields,
apply modes, sysext selections, and raw extension activation paths are rejected
before rendering.

The key distinction is whether a requested change can be made effective on the
currently running node, or whether it must be staged as the next bootable
generation.

## Decision

Runtime configuration change requests use one explicit apply mode:

```text
next-boot
  Render and validate a new generation, set it as the next boot candidate, and
  leave the current boot unchanged.

live
  Render and validate a new generation that reuses the current root and sysext
  selection, activate its generated confext in the current boot, run only the
  domain-specific live apply actions that the planner accepted, and persist the
  generation for later boots only after health checks pass.
```

The apply mode belongs to the runtime configuration change request envelope, not
to individual raw files. Domain renderers and the planner decide whether the
requested domain diffs are valid for that mode. If a request mixes domains and
any diff is not accepted for the requested mode, Katl rejects the request before
rendering or activating partial state.

The render step happens on the node that receives the request. A successful
request creates local generation artifacts from trusted Katl config: generated
confext content, sysext activation metadata, generation spec/status, and apply
status linked to a node-local `OperationRecord`. The request is not a transport
for prebuilt confext images or arbitrary systemd extension activation paths.
Unsupported config never produces partial generation artifacts.

Install-time materialization has no apply mode; it creates the first selected
generation.

## User-Facing Surface

The initial runtime change surface should be shaped like:

```text
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
apply:
  mode: next-boot | live
spec:
  node:
    ...
```

`next-boot` is the default for unattended or ambiguous runtime changes. `live`
is opt-in because it can restart services, change networking, or alter operator
access.

The request still uses supported Katl configuration domains. It is not an
arbitrary `/etc` patch interface, does not accept user-provided confext images,
and does not expose systemd extension activation as a raw user knob.

## Domain Classification

Each supported domain must declare its apply behavior before it can be changed
at runtime.

Online-applicable domains may be used with `live` when their domain-specific
preflight passes:

```text
resolved and host DNS
  Reload or restart systemd-resolved after validating bounded resolver input.

sysctl
  Apply bounded kernel parameters through systemd-sysctl or explicit sysctl
  calls. Unsupported keys remain rejected.

tmpfiles
  Run systemd-tmpfiles only for Katl-managed paths.

networkd
  Conditionally live-applicable. Safe additions or bounded updates may reload
  systemd-networkd/networkctl through tested runtime-agent logic. Changes that
  can drop the management path, rename active interfaces, remove the current
  address, or replace default routing are rejected for `live`.

Bootstrap node metadata
  Live-applicable only for non-secret descriptive fields that do not change
  systemRole, stable node identity, selected bootstrap profile, or selected
  Kubernetes payload.
```

Staged-only domains can be rendered into a candidate generation, but normal
runtime configuration apply does not make them live:

```text
node identity and hostname
  Stable node names and hostnames affect kubelet identity, certificates,
  kubeadm input, and operator reachability.

modules-load
  Boot ordering through systemd-modules-load is the initial supported path.

mount units and extra disks
  Persistent state projections and extra disk topology require boot ordering,
  filesystem checks, and rollback semantics that are not part of first live
  apply.

SSH and operator access
  Operator access changes are staged-only until a lockout-safe validation and
  reload path exists.

Bootstrap profile input
  Katl may render desired native kubeadm input for a future generation, but
  normal config apply does not run kubeadm, kubectl, or mutate live cluster
  objects.
```

Rejected-live changes fail before render or activation:

```text
systemRole changes
selected bootstrap profile or rendered kubeadm input changes
selected Kubernetes sysext payload changes requested for live apply
kubelet node identity changes
user ownership of host account, PAM, sudo, passwd, shadow, or sysusers files
/etc/kubernetes or kubeadm-owned mutable state
unknown domains or arbitrary /etc paths
root, UKI, kernel command line, or raw sysext activation changes
```

The planner may later promote a staged-only domain to online-applicable only
after its live preflight, activation, rollback, and VM tests exist.

## Generated Confext And Generation Metadata

Every accepted runtime change creates a new generation spec and status. A confext-only
change reuses the current root slot, root artifact digest, UKI, kernel command
line, and sysext set, but records a new generated confext path and digest.
Changes that select a different compatible sysext payload are staged as a new
generation that records the selected sysext set with the generated confext.
Raw sysext activation paths and unsupported sysext selections remain rejected
runtime config input.

The immutable generation `spec.json` for runtime configuration changes must
record:

```text
configuration source digest
changed domains
requested apply mode
accepted apply mode
previous generation id
kubeadm explicit-action-required flag, when rendered input differs from live
  cluster state
```

Mutable generation status lives in:

```text
/var/lib/katl/generations/<generation-id>/status.json
```

Mutable apply results live in a canonical node-local operation record:

```text
/var/lib/katl/operations/<operation-id>/
```

That operation record stores phase, live action results, diagnostics, rollback
target, rollback result, timestamps, and redacted failure reasons. This keeps the
current generation spec rule intact: the generation selects root, sysext, and
confext together, and Katl must not mutate an existing generation's immutable
selection fields in place.

A generation-local
`/var/lib/katl/generations/<generation-id>/config-apply-status.json` may remain
as a compatibility summary of the latest apply attempt, but it is not the
authoritative recovery record. Authoritative phase, retry, and rollback state
comes from the `katlc`-owned operation journal described in
`docs/internal/generations-and-operations.md`.

For `next-boot`, Katl renders and validates the candidate, writes
`commitState: candidate`, `bootState: pending`, and `healthState: unknown`,
records `phase: next-boot`, and arms bounded boot selection. The current boot
keeps the previous active generation and no live activation occurs.

For `live`, Katl renders and validates the candidate, writes `pending` and
`unknown` generation status with `commitState: candidate`, exposes only that
selected generated confext in the current boot, runs the accepted live apply
action plan, and records progress in the node-local `OperationRecord`.
`operationPhase: live-active` means the current boot is using the candidate
confext and accepted live actions passed. It does not mean the generation has
passed boot health.

After successful live apply, Katl may select the same generation as a bounded
next-boot candidate. It becomes the persistent default only after a boot reaches
`katl-boot-complete.target`.

This ADR describes normal runtime configuration apply. Kubeadm-aware bootstrap
and join are explicit operations that may live-activate and commit a
Kubernetes-capable generation after kubeadm and operation health checks succeed,
while leaving boot health pending until a later boot.

## Status And Rollback Reporting

Runtime configuration status must be machine-readable and operator-facing. At
minimum it reports:

```text
generation id
previous generation id
requested and accepted apply mode
changed domains
phase: planned, rendered, staged, activating, active, next-boot, rolling-back,
  rolled-back, failed
health state
domain action results
diagnostic artifact paths
rollback target, when present
failure reason with sensitive values redacted
```

Rollback always selects a complete prior generation spec. It must not roll back
only confext while leaving generation spec pointed at another root or
sysext set. For a failed `live` change, rollback means restoring the previous
running confext activation path and rerunning the bounded apply actions needed
to make that prior state effective. If rollback cannot restore the live state,
Katl must leave the previous generation selected for next boot and report that a
reboot is required.

A failed live apply candidate must not become a rollback target. Failed live
apply rollback reactivates the previous generation's confext set and reruns only
the bounded apply actions needed to restore that state. If live rollback fails,
Katl leaves the previous known-good generation selected for boot and reports that
reboot or repair is required.

## Kubeadm And Cluster Boundary

Generated confext activation must not run:

```text
kubeadm init
kubeadm join
kubeadm upgrade
kubectl
CNI installers
cluster add-on installers
GitOps controllers
```

Changing kubeadm desired input is normal Katl configuration, but applying that
input to a live cluster is a separate kubeadm-aware operation with its own
planner, explicit user request, status, rollback story, and tests. A `live`
configuration request that would change systemRole, selected bootstrap profile,
rendered kubeadm input, or node identity is rejected instead of silently
changing cluster state.

Katl-rendered kubeadm/kubelet input is desired state, not ownership of the live
cluster artifacts that kubeadm creates or updates. The live owner remains kubeadm
or kubelet for `/etc/kubernetes`, kube-system kubeadm/kubelet ConfigMaps, and
`/var/lib/kubelet` files.

A normal runtime configuration apply may detect and report desired/live drift,
but it must not close that drift by editing those paths or Kubernetes objects.
Closing the drift requires an explicit kubeadm-aware operation with its own
request, status, rollback limits, and tests.

## Testing Contract

Implementation follow-up work must cover:

```text
planner unit tests for live, next-boot, staged-only, and rejected-live decisions
golden tests for generation spec/status fields and generated confext paths
golden tests for OperationRecord snapshots and any config-apply-status.json
  compatibility summary
negative tests proving mixed live requests fail atomically
negative tests proving kubeadm and /etc/kubernetes changes are never live-applied
status serialization tests with redaction
VM tests for at least one online-applicable domain and one staged-only domain
```

Generated paths and test fixtures must remain repo-relative or under the VM test
artifact tree. Tests must not bake host-specific Nix store, user home, or
machine-local paths into committed configuration.

## Consequences

A follow-up should implement a typed planner before `katlc` executes live
changes. The planner should produce an explicit decision: accepted `live`,
accepted `next-boot`, or rejected with domain diagnostics.

Another follow-up should prove the decision in VM tests before live application
is considered supported.

Future kubeadm-aware actions must build on the desired/live state planner rather
than hiding cluster mutations inside generated confext activation.
