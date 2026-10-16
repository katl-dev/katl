# ADR-019: Kubernetes configuration component health

Status: accepted.

Date: 2026-10-17.

## Ownership

Control-plane reconciliation checks the selected native manifests, local API
readiness, running control-plane containers, and direct etcd endpoint health.
These checks do not require kubelet's exec server or cached mirror-pod readiness.
They do not independently establish scheduler or controller-manager readiness
while kubelet is unavailable. Kubelet reconciliation owns kubelet startup and
Node readiness. An unchanged control-plane configuration must not require a
healthy kubelet before its configuration can be repaired.

Host configuration staging does not restart kubelet to consume a replacement
configuration that has not been written yet. Component reconciliation owns any
required restart, including rebinding the static-pod watcher after a generation
transition. A semantically changed control-plane manifest must converge to a
mirror pod with `ContainersReady=True` and a different native configuration
hash. Old healthy pods
do not establish that a new manifest was consumed. Formatting-only changes do
not require a new hash.

Write the final manifests before a required watcher restart. Native startup
notification precedes the first static-pod synchronization. Starting against
superseded manifests can leave that synchronization waiting for volumes that
the replacement configuration has already removed from its desired state.

## Pre-CNI configuration

Kubeadm bootstrap can complete before the operator installs a CNI. An active
kubelet, healthy runtime, and healthy local control plane can coexist with a
Node whose sole Ready failure reports uninitialized CNI. Kubelet configuration
can preserve that condition. A node observed Ready must regain Ready.

Admit the exception only from a successful, structured Node observation and
corroborating live CRI conditions. Match the complete native uninitialized-CNI
diagnostic. A substring match could conceal another error in Kubernetes'
aggregated Ready message. Missing, malformed, or unavailable observations
cannot grant the exception. Unrecognized diagnostics retain strict readiness.

Derive the readiness requirement from the native Node and CRI observations,
independently of control-plane convergence. Pending control-plane containers
must not turn a valid pre-CNI baseline into a Ready requirement. A fresh Ready
Node report tightens the requirement even if CRI or control-plane health has
not converged, provided the same kubelet invocation remains active throughout
the observation. Completion still requires healthy CRI, control-plane health,
and the active configuration context under that invocation.

For configuration checks, use the native static pods' `ContainersReady`
condition alongside local API health. The node controller can set `PodReady`
to false because the Node is NotReady, independently of container probe
results. Requiring `PodReady` would reintroduce the CNI dependency through
component health. Read the structured native conditions once and let the
operation's bounded health loop own convergence.

Give each native observation its own command deadline within the operation's
overall deadline. Earlier commands must not consume a later probe's budget;
timeouts remain unavailable observations rather than negative health results.

Keep public node status and host and Kubernetes upgrade readiness strict.
Kube-proxy configuration retains its native DaemonSet rollout check.
Katl does not install, manage, or repair the CNI or customer workloads.

## Startup evidence

A `Type=simple` service can appear active before kubelet validates its
configuration. Kubelet's native systemd notification supplies a stronger
synchronous startup boundary, independent of the optional healthz endpoint.
However, runtime-dependent initialization continues asynchronously after that
notification. Healthz and configz can also become available before that work
finishes.

Use one service-owned startup witness for boot, explicit restart, and automatic
restart. Bind its boot identity and precision-aligned heartbeat boundary to the
systemd invocation. Require an acceptable Node report from that invocation,
corroborate live runtime health, and recheck that the same invocation is active.
Kubelet's runtime errors prevent a qualifying Node condition before its runtime
dependent managers initialize successfully.

This relies on kubelet initializing its runtime-dependent modules before
recording a successful base runtime sync. The supported Kubernetes build's
initial Node status includes a runtime error until that sync succeeds. The VM
journey exercises both a rejected CPU-manager policy and an asynchronous
checkpoint-policy conflict; notification success alone must not complete the
latter apply. Retain these checks when changing the Kubernetes version.

The native heartbeat has second precision. Establish the boundary after the
old invocation stops and start the new invocation at or after the next whole
second. This allows the first report in the startup second to qualify. A fence
based on synchronous restart completion can reject that report and wait for
the operator's much longer reporting interval.

This boundary assumes ordinary realtime ordering between the stopped and new
invocations. The startup hook compares realtime and monotonic elapsed time
while aligning the boundary and rejects a visible clock step. Future-dated
heartbeats, mismatched boot or invocation identities, and malformed startup
records cannot establish health. This is not a proof against an arbitrary
clock rollback across invocations; it does not enforce a lifetime clock-offset
policy that would reject ordinary NTP adjustment.

## Recovery

Persist the kubelet readiness requirement in the existing configuration operation
before component mutation. An interrupted restart can leave kubelet inactive
and the Node condition Unknown; recomputing the exception from that state
would either prevent repair or authorize unrelated failures.

A repair can inherit the requirement of the incomplete kubelet mutation it is
reconciling. A fresh Ready observation tightens that requirement. Earlier
successful operations confer no exception, and older records without a
requirement require Ready. Once recorded, Ready cannot be relaxed by recapture
or a stale caller. This state belongs to the operation transaction;
it is not a global CNI mode or an operator option.

Positively observed unhealthy states can still be repaired by configuration,
but must reach Ready unless the recorded uninitialized-CNI exception applies.
No-op completion must check health. A matching configuration file does not
establish that an interrupted restart loaded it.

## Alternatives

Requiring Ready before every configuration change rejects the supported
pre-CNI journey. Accepting arbitrary NotReady states hides component failures.
Using only systemd activity, startup notification, or an early HTTP endpoint
can accept a kubelet that will fail during initialization. A mandatory local
healthz address restricts valid native configuration that changes or disables
that endpoint.

A stability delay does not establish startup completion. Per-operation
stop/start machinery would duplicate lifecycle responsibility and omit boot
and automatic restart. Native Node conditions plus a service-owned invocation
boundary retain one startup contract without managing customer workloads.
