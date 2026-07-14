# ADR-003: Runtime configuration changes use trusted operator input

Status: accepted.

Date: 2026-06-05.

This ADR defines how an installed Katl node accepts changed user-supplied
configuration for generated confext updates. It builds on
`adr-001-generated-confext-configuration.md`,
`adr-002-live-and-next-boot-config-apply-modes.md`, and
`supported-node-config-domains.md`.

## Context

Katl can render Katl-native node configuration into generated confext for first
install. After the node is installed, the same rendering model must support
changed cluster defaults, systemRole-level configuration, and per-node
overrides without turning Katl into a general-purpose configuration manager or
a Kubernetes lifecycle controller.

Runtime configuration input is security-sensitive because it can change
effective `/etc` content, restart host services, and affect kubeadm-ready
state. A node may already be part of a Kubernetes cluster when it receives a
change.

## Decision

Runtime configuration changes use one typed request envelope:

```text
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  desiredVersion: <monotonic operator-chosen version>
  sourceID: <operator/local source identity>
apply:
  mode: auto | live | next-boot
spec:
  clusterDefaults: ...
  systemRoleOverrides: ...
  nodeOverrides: ...
```

The initial implementation accepts this request from `katlc`, running as an
operator command on the node. A local file path is therefore the first concrete
transport. The local file is not the trust root by itself; it is only the
handoff format for an authenticated operator action.

`apply.mode` defaults to `auto` when omitted. The requested mode and accepted
mode are separate audit fields: `auto` may be accepted as `live` or
`next-boot`, strict `live` may not silently fall back to next boot, and strict
`next-boot` may not apply changes online.

The request contains desired Katl configuration, not generated extension
artifacts. After authentication and validation, `katlc` locally renders the
candidate generation: generated confext content, selected sysext activation
metadata, immutable generation spec, generation status, and apply status. User input cannot
directly provide confext images, activation links, or generation paths. `katlc`
rejects unknown domains, unsupported fields, unsupported sysext selection
requests, and unsupported apply modes before render.

`katlc` must merge input in this order:

```text
cluster defaults
per-node overrides
```

The merged result is validated through the supported node configuration
domains. Unknown domains, arbitrary `/etc` paths, kubeadm-owned mutable state,
host account ownership, raw confext artifacts, and user-selected generation
paths are rejected before render.

## Input Options

Katl recognizes four possible input paths, but only the operator-command/local
file path is in scope for the first implementation.

```text
operator command plus local file
  Initial path. The operator authenticates through existing node access, places
  a NodeConfigurationChange document on the node or streams it to the agent, and
  receives a local status/audit record.

pull
  Deferred. A node may later poll an HTTPS source or object store for desired
  config. This requires a pinned trust root, freshness checks, backoff, and
  explicit operator status before it is enabled.

push/API
  Deferred. A remote API may later submit changes to the node agent. This
  requires mTLS or another node-local authentication boundary, authorization,
  rate limiting, and replay protection.

direct local file without operator command
  Not accepted as an autonomous watcher in the first implementation. A file
  appearing on disk must not trigger configuration changes by itself.
```

The local operator-command path keeps early behavior testable in local VM
tests, avoids adding a network control plane, and keeps the trust boundary
aligned with existing operator access.

## Authentication And Trust Roots

For the first implementation, the authenticated actor is the operator who can
reach the node through Katl-supported operator access. The trust roots are:

```text
installed KatlOS runtime, `katlc`, and KatlOS runtime service binaries
existing operator SSH key or vmtest channel used to submit the request
the installed generation spec that records the current root, sysext, and
  confext selection
the request digest and desiredVersion recorded by `katlc`
```

The request body is trusted only after `katlc` has received it through an
authenticated operator path and validated it against Katl domain policy. Katl
must not trust user-provided file ownership, file path, generation ID, render
path, confext activation path, sysext selection, or root selection.

Pull and push/API modes need additional trust roots before they can be enabled:

```text
server identity pinning or mTLS roots
request signer or service account identity
authorization policy for which domains may change
bounded request size and rate limits
replay and stale-version rejection
durable audit records for accepted and rejected requests
```

Generated confext artifacts do not need separate signatures in the first
implementation because they are generated locally from trusted, validated input
and recorded in generation spec by digest. Signing generated confext can be
reconsidered later if Katl starts distributing generated confext artifacts
between machines or needs an offline attestation story.

## Freshness, Replay, And Version Selection

Every runtime change request must include a `metadata.desiredVersion`. The
initial accepted format is an unsigned decimal sequence number encoded as a
string, compared numerically within a `metadata.sourceID`. A later pull or API
transport may introduce a different version scheme only if that scheme defines
a deterministic total ordering before it is accepted by `katlc`.

`katlc` records for each accepted or rejected request:

```text
sourceID
desiredVersion
request digest
requested apply mode
accepted apply mode, when accepted
planner classification: live, next-boot, operation-only, or rejected
changed domains
previous generation id
candidate generation id, when rendered
decision result
diagnostics
timestamp
```

Replay and staleness behavior:

```text
same sourceID and same desiredVersion with the same request digest
  Idempotent retry. Return the existing decision/status.

same sourceID and same desiredVersion with a different request digest
  Reject. The version is ambiguous.

same sourceID and an older desiredVersion than the latest accepted or rejected
version
  Reject as stale.

newer desiredVersion
  Evaluate normally.
```

The node knows a desired configuration is newer by comparing
`metadata.desiredVersion` against the latest recorded version for the same
`sourceID`. Pull/API transports may later add signed timestamps or server
revision numbers, but the node-local monotonic check remains required.

## Audit And Rejection Records

`katlc` writes an audit record for every request before it mutates runtime
state. The audit record belongs under the Katl writable state tree, for
example:

```text
/var/lib/katl/config-requests/<source-id>/<desired-version>.json
```

Accepted runtime changes also write immutable generation spec/status and a
canonical node-local `OperationRecord` under
`/var/lib/katl/operations/<operation-id>/`, as described by ADR-002. A
generation-local `config-apply-status.json` may remain as a compatibility summary
linked from the operation record, but it is not the authoritative recovery
record.

A rejected request may reuse common decision/status fields, but it does not
become a mutating operation record and must not create partial generation
artifacts. Idempotent retry returns the existing audit decision and, when
accepted, the linked operation record.

Rejected requests must record a redacted diagnostic and must not render,
activate, or partially apply generated confext. Rejection reasons include:

```text
unauthenticated or unsupported input transport
missing sourceID or desiredVersion
stale or replay-conflicting desiredVersion
unknown domains
unsupported fields in known domains
unsafe paths or duplicate rendered outputs
unsupported live domain decisions
operation-only changes submitted as normal config apply
unsupported sysext selection requests
attempted kubeadm/kubectl/CNI/GitOps/package-manager side effects
attempted /etc/kubernetes or host account ownership
root, UKI, or kernel command line changes through normal runtime config
raw sysext activation paths or unsupported sysext selection changes
```

Typed sysext update input may be accepted only by a planner that stages a new
generation and validates compatibility with the selected runtime root and
generated confext.

Diagnostics must redact URLs with credentials, bearer tokens, kubeadm bootstrap
tokens, discovery token hashes, private keys, and kubeconfig-like secrets.

## Kubeadm Boundary

Changing kubeadm desired input is allowed only as Katl configuration rendered
for a candidate generation. Normal runtime configuration apply must not run:

```text
kubeadm init
kubeadm join
kubeadm upgrade
kubectl
CNI installers
cluster add-on installers
GitOps controllers
```

If rendered kubeadm desired input differs from the live cluster state or from
the selected kubeadm metadata, the planner records
`kubeadm.explicitActionRequired=true` in generation spec and the operation
record or status view. Applying that desired input to a live cluster is a
separate kubeadm-aware operator action with its own planner, status, rollback
story, and VM tests.

SystemRole changes, selected bootstrap profile or rendered kubeadm input
changes, stable node identity changes, and `/etc/kubernetes` changes are never
silently applied to a running cluster by normal generated confext activation.

## Testing Contract

Unit tests must cover:

```text
valid NodeConfigurationChange envelope parsing
cluster defaults, systemRole-level overrides, and per-node override merge order
sourceID, desiredVersion, and request digest recording
idempotent retry for same version and digest
rejection for stale versions and same-version digest conflicts
rejection for unknown domains, unsupported fields, and arbitrary /etc paths
rejection for unsupported sysext selection requests
rejection for /etc/kubernetes, host account policy, kubeadm/kubectl/CNI/GitOps,
  package-manager, root, UKI, kernel command line, and raw or unsupported sysext
  selection changes
kubeadm desired input producing explicit-action-required status instead of
  live cluster mutation
redaction in audit/status diagnostics
```

VM tests must cover:

```text
changed networkd input that is accepted for live apply only after preflight
changed kubeadm desired input that is staged or action-required and does not
  run kubeadm or kubectl during normal apply
changed SSH/operator access input rejected or staged until lockout-safe live
  validation exists
systemRole/common defaults and per-node overlay merge behavior
rejected unsafe paths producing an audit/status record without partial
  activation
```

## Consequences

The first runtime configuration implementation should expose an operator-command
path and local request file shape before adding pull or push/API transports.

`katlc` must persist enough request metadata to make retries idempotent and
stale changes visible to operators.

The generated confext renderer remains the only path that creates runtime
configuration artifacts. User-provided confext images and arbitrary `/etc`
patches remain out of scope.
