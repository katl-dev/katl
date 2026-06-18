# Cluster Bootstrap CLI Contract

Status: current decision.

This document defines the operator-run command that turns already installed
generation 0 Katl nodes into a Kubernetes cluster. It does not change the Katl
runtime boundary: the command runs from the operator workstation as a control
client, asks node-local `katlc` on each KatlOS host to create and activate the
first Kubernetes-capable generation from stored intent, run the appropriate
kubeadm init/join workflow through the agent executor, and report node-local
operation status. Users/operators own the cluster contents after bootstrap.

## Command Shape

Initial command:

```text
katlctl cluster bootstrap --inventory <cluster.yaml> [options]
```

Equivalent compiled-plan input may be added when `katlc` exists:

```text
katlctl cluster bootstrap --plan <compiled-cluster-plan.json> [options]
```

Important options:

```text
--init-node <node-name>
  explicit first control-plane node for kubeadm init

--node-address <node-name>=<address>
  operator override for a node address from config

--control-plane-endpoint <host:port>
  kubeadm controlPlaneEndpoint and kubeconfig server endpoint, unless a later
  stable endpoint is declared and verified

--kubeconfig-out <path>
  export operator kubeconfig output here

--overwrite-kubeconfig
  replace an existing kubeconfig instead of requiring an exact idempotent match

--dry-run
  validate inventory, access, readiness, and phase plan without running kubeadm

--bootstrap-pre-wait <wait>
  kubectl wait to run after kubeadm joins finish and before bootstrap manifests
  are applied

--bootstrap-wait <wait>
  kubectl wait to run after bootstrap manifests are applied; supported forms
  include api-ready, nodes-ready, resource-exists, condition, rollout-status,
  pods-ready, and stable-endpoint

--bootstrap-stable-endpoint <host:port>
  stable API endpoint to verify before exporting kubeconfig output that uses it
```

The command is a bounded off-node control client. It runs on the operator
workstation, connects to each KatlOS host's `katlc` management endpoint,
sequences explicit node-local operation requests, relays operator-requested
outputs, reports status, and exits. Its only persistent state is `katlctl`
client configuration for communication profiles and known nodes. It is not a
daemon, reconciler, add-on manager, CNI manager, Flux manager, BIRD manager, or
cluster lifecycle controller.

The local workstation config path and environment overrides are defined in
`docs/internal/katlctl-workstation-config.md`.

Authority rule: `katlctl` may load operator input, transport requests, sequence
bounded multi-node workflows, wait on returned operation IDs, and summarize
results. Node-local `katlc` must revalidate every accepted request and is the
only writer of generation specs, generation status, boot selection, operation
records, and durable node lifecycle state. Any `katlctl` plan, log, summary, or
kubeconfig output is disposable client output unless `katlc` has persisted the
corresponding state on the node.

## Operation Record Boundary

`katlctl cluster bootstrap` submits explicit bootstrap operation requests to
node-local `katlc`. The selected init node gets a `bootstrap-init` operation;
each worker join gets a `bootstrap-join-worker` operation. Additional
control-plane joins are a later operation-backed path and are rejected as
unsupported until their design and VM proof exist. Each accepted day-one attempt
asks node-local `katlc` to
validate stored intent, create the first Kubernetes-capable candidate generation,
activate it for kubeadm readiness, run kubeadm, and write a canonical
`OperationRecord` under `/var/lib/katl/operations/<operation-id>/`.

`katlctl` may display a non-authoritative invocation summary that links returned
node operation IDs, selected inputs, CLI overrides, redacted diagnostics, and
rollout ordering. That summary is a client view only. It must not be used as
node crash recovery state, and it must not become desired cluster state. The
source of truth after mutation remains kubeadm output, node-local Katl state, and
Kubernetes API state. The command must not remain resident, watch the cluster, or
continuously reconcile failed or missing joins.

Each node-local `OperationRecord` must identify the candidate generation ID, the
phase where kubeadm first mutated state, and which mutation scopes were touched.
`kubeadm init` can mutate `/etc/kubernetes`, `/var/lib/kubelet`,
`/var/lib/etcd`, and Kubernetes API objects. `kubeadm join` can mutate
`/etc/kubernetes`, `/var/lib/kubelet`, node objects, and, for control-plane
joins, etcd membership. Host rollback after a failed bootstrap or join does not
clean this partial state; retry must inspect actual kubeadm and Kubernetes state.

`katlc` must create and fsync the `OperationRecord` before it creates the
candidate generation, activates kubeadm-ready state, materializes join material,
or invokes kubeadm. Candidate generation commit must set
`committedByOperationID` to the bootstrap or join record. A rerun or retry
creates a new linked operation record or an explicit `retry-operation` child
record. Terminal records and `katlctl` invocation summaries are never rewritten
into authoritative bootstrap state.

Each node-local attempt must also record:

```text
candidateGenerationID
activationMode: live
activationState: pending | activating | active-live | failed | rolled-back
generationCommitState: candidate | committed | abandoned
postKubeadmHealthState: not-run | running | passed | failed
bootHealthPending: true | false
preExecMutationMarkers[]
agentStartID
executorAttemptID
childProcess, when present
pid and exitStatus, when present
resourceLocks[] including kubeadm-state.lock
```

When kubeadm and post-kubeadm operation checks succeed, `katlc` commits the
generation by setting `commitState: committed`. That accepts desired host state,
but leaves persistent default selection and boot health pending until a later
boot reaches `katl-boot-complete.target`.

## Bootstrap State Ownership

Bootstrap state is split by ownership layer:

| State | Owner | Recovery role |
| --- | --- | --- |
| Stored install cluster intent | `katlos-install` and `katlc` | Bootstrap input and provenance only; not live desired cluster state |
| Generation spec/status | `katlc` | Desired host state and boot/commit health only |
| Bootstrap or join attempt state | `katlc` | Canonical `OperationRecord` under `/var/lib/katl/operations` |
| Rendered `/etc/katl/kubeadm` input | `katlc` | Desired kubeadm input selected by the generation; not live cluster state |
| `/etc/kubernetes`, `/var/lib/kubelet`, `/var/lib/etcd`, and API objects | kubeadm, kubelet, Kubernetes, and etcd | Live node or cluster state; inspected by retry and repair operations |
| `katlctl` invocation summary and kubeconfig output | `katlctl` client output | Operator convenience only; not authoritative recovery state |
| CNI, DNS, GitOps, add-ons, and workloads | User-managed cluster tooling | Outside bootstrap operation ownership |

The focused cluster-global artifact boundary is defined in
`docs/internal/cluster-bootstrap-state-model.md`.

Bootstrap and join records use the shared operation evidence model. Required
`bootstrap-init` evidence includes:

```text
nodeIdentity:
  inventoryNodeName
  hostStaticHostname
  kubeadmNodeRegistrationName
  observedAPINodeName
  observedAPINodeUID
kubeadmEvidence:
  subcommand: init
  currentPhase
  completedPhases[]
  firstMutationPhase
  rendered InitConfiguration/ClusterConfiguration digest
mutationScopes:
  etc-kubernetes
  kubelet-state
  etcd-state
  cluster-objects
apiEvidence:
  before init
  after init
  after stable endpoint verification, when requested
staticPodManifestEvidence:
  kube-apiserver
  kube-controller-manager
  kube-scheduler
  etcd
etcdMemberEvidence:
  local member ID after init
  member list after init
joinMaterialEvidence:
  bootstrap token present, expiry, usage, redacted fingerprint only
  certificate key present, expiry, upload-certs phase observed
```

Required `bootstrap-join-worker` evidence includes:

```text
joinRole: worker
nodeIdentity:
  inventory node name, host name, kubeadm registration name, and API node
  name/UID when observed
kubeadmEvidence:
  subcommand: join
  currentPhase
  completedPhases[]
  firstMutationPhase
apiEvidence:
  endpoint used before join and after join
  node object before and after join when API is reachable
staticPodManifestEvidence:
  not-applicable; any manifest is a diagnostic anomaly
etcdMemberEvidence:
  not-applicable
joinMaterialEvidence:
  bootstrap/discovery tokens redacted
```

## Input Model

Bootstrap input describes installed nodes, not install-time desired state. Each
node entry includes:

```text
name
address
systemRole
access method and credentials reference, not inline secret values
cluster intent reference for the node's bootstrap profile
requested Kubernetes payload version
```

The cluster input may also reference bounded kubeadm and output policy:

```text
bootstrap.waits[]
  bounded kubeadm-scoped waits using api-ready, joined-nodes-observed, or
  control-plane-healthy

bootstrap.stableEndpoint
  optional API endpoint to verify before kubeconfig output uses it
```

Addresses may come from the cluster plan, inventory, or `--node-address`
overrides. CLI address overrides are operator conveniences for lab and VM tests;
they must be included in the submitted operation request and recorded in the
relevant node-local operation records, with any invocation summary linking the same
values, so diagnostics show what was actually used.

`systemRole` is the only source of desired cluster node role:

```text
control-plane
  intended to become a Kubernetes control-plane node

worker
  intended to become a Kubernetes worker node
```

The submitted operation request may name the rendered kubeadm input that `katlc`
selected from stored intent, but user-facing intent must not encode kubeadm
verbs such as "run join --control-plane". Katl intent says what role the node
should have; the operation implementation decides which kubeadm command and
phase sequence satisfies that role for the current backend.

Capability overlays are a day-2 design item and are not part of the first
bootstrap inventory contract.

## Init Node Selection

Safe selection rules:

```text
if --init-node is set
  it must name a control-plane node and becomes the only init node

if exactly one control-plane node exists
  that node is the default init node

if more than one control-plane node exists
  --init-node is required for the first implementation

if no control-plane node exists
  fail before contacting nodes
```

The selected init node is recorded in the plan, the selected node's
`bootstrap-init` operation request, and any invocation summary. The command must
never ask more than one node to run `kubeadm init` in one invocation.

## Readiness And Access

Before running kubeadm phases, the command verifies every target node is an
installed generation 0 KatlOS node with enough stored intent to prepare a
Kubernetes-capable generation.

Minimum pre-bootstrap checks:

```text
Katl generation 0 reached installed-runtime health
stored install manifest and cluster intent are present
requested Kubernetes payload version matches the cluster plan
the requested bundled Kubernetes sysext is available and verified
stored systemRole and selected bootstrap profile intent are consistent
node-local katlc can accept an operation request
```

Initial access is the `katlc` management API over TCP gRPC. `katlctl` must not
SSH to nodes, run remote shell commands, or depend on a test-harness-only
transport for the supported bootstrap path. VM tests should exercise the same
remote API shape where practical. Kubernetes API access starts only after
`kubeadm init` has produced a usable kubeconfig; the API is not a pre-bootstrap
coordination channel.

If any node cannot pass pre-bootstrap checks, the command fails before preparing
or running kubeadm anywhere.

## Bootstrap Flow

The bootstrap command runs phases in this order:

These are control-client phases. Phases that run `kubeadm init` or
`kubeadm join` are executed by node-local `katlc` and must update the
corresponding `bootstrap-init` or `bootstrap-join-worker` `OperationRecord`.
Additional control-plane joins are not part of the day-one operation set.

```text
1. load and validate inventory or compiled plan
2. apply CLI overrides and record them
3. select the init control-plane node
4. verify access and generation 0 installed-runtime state on every node
5. ask katlc on each target node to validate stored intent and create the first
   Kubernetes-capable candidate generation
6. ask katlc to activate the candidate generation and wait for
   katl-kubeadm-ready.target on nodes before their kubeadm phase
7. ask katlc to run kubeadm init only on the selected init node
8. collect or create join material
9. ask katlc to join remaining worker nodes
10. join additional control-plane nodes later, when that path is implemented
11. wait for API readiness using the init or declared endpoint
12. katlc runs post-kubeadm health checks and commits each successful candidate
    generation
13. optionally verify the declared stable endpoint for operator kubeconfig output
14. export operator kubeconfig output, using a declared stable endpoint only
    after the endpoint wait succeeds
15. print next steps and exit
```

Worker joins must not start until init succeeds and join material exists.
Additional control-plane joins require certificate-key handling and may be
implemented after worker joins. The durable contract is that every non-init
control-plane node is classified for a later control-plane join from
`systemRole`; until that implementation exists, multi-control-plane plans fail
with a clear unsupported message after init-node selection validation.

The greenfield multi-control-plane target is kubeadm stacked etcd. Its data
ownership, quorum, join ordering, and rollback limits are defined in
`docs/internal/stacked-etcd-bootstrap-data-policy.md`. Cluster-global bootstrap
state ownership is defined in `docs/internal/cluster-bootstrap-state-model.md`.

## kubeadm Material

The node-local `katlc` operation runs kubeadm against rendered Katl input
created in the candidate generation:

```text
kubeadm init --config /etc/katl/kubeadm/<name>/config.yaml
kubeadm join --config /etc/katl/kubeadm/<name>/config.yaml
```

Rendering those paths is part of the bootstrap or join operation. Generation 0
stores kubeadm intent, but it does not render the kubeadm input paths or activate
Kubernetes binaries during normal boot.

Bootstrap tokens, discovery tokens, certificate keys, and uploaded certificate
material are generated or collected during the operator action. They are not
stored in normal Katl node config, generated confext, or committed inventory
fixtures.

When join material is needed on a node, `katlctl` supplies it through the
operation request or transport. Node-local `katlc` materializes any temporary
restricted join configuration needed to run kubeadm. Any temporary file is mode
`0600`, lives outside `/etc/katl`, is deleted on a best-effort basis after use,
and is never copied into the normal `OperationRecord` or invocation summary.

Normal output redacts:

```text
bootstrap tokens
discovery-token-ca-cert-hash values when bundled with tokens
certificate keys
kubeconfig client certificate data
kubeconfig client key data
private keys
bearer tokens
```

Debug bundles may contain sensitive artifacts only when the operator explicitly
requests that mode; the default `OperationRecord` and any invocation summary are
redacted.

## Control-Plane Endpoint

The command supports two endpoint shapes:

```text
initial kubeadm endpoint
  single-node bootstrap may use the selected init node address
  multi-node bootstrap requires an explicit --control-plane-endpoint or an
  equivalent endpoint from the compiled plan

stable endpoint verification
  use a user-declared VIP, DNS name, routed endpoint, or load balancer as the
  stable operator-facing API endpoint
```

Katl does not own BIRD, VIP, kube-vip, ingress, load balancer, or DNS lifecycle
as part of this command. The command may verify a declared stable endpoint before
exporting kubeconfig output that uses it. It does not apply the user resources
that might later advertise or replace that endpoint.

Do not add kubePrism as an initial requirement.

The opt-in host/platform endpoint helper design, including BIRD-mediated routing
and pre-Cilium advertisement, is tracked separately in
`docs/internal/platform-api-endpoint-routing-capability.md`.

The broader user story and operator contract for platform API endpoints is
documented in `docs/internal/platform-api-endpoint-user-story.md`.

## Kubeconfig Output

After kubeadm init succeeds and the API is reachable, the command may export
operator kubeconfig output when explicitly requested. This is client-side output,
not Katl node state, an operation record, or desired cluster state.

Rules:

```text
default path is ./katl-kubeconfig
explicit --kubeconfig-out overrides the default
file mode is 0600
parent directory must already exist or be created with safe permissions
existing file is refused unless --overwrite-kubeconfig is set or content
  exactly matches the intended output
server endpoint is the selected bootstrap endpoint or the stable endpoint after
  a declared endpoint wait succeeds
normal logs never print certificate or key material
```

The command prints a concise next-step line:

```text
kubectl --kubeconfig <path> get nodes
```

## Idempotency And Retry

The command is safe to retry after common failures.

Rules:

```text
if kubeadm init already completed on the recorded init node
  verify the API and continue with join/kubeconfig phases

if a worker already joined
  verify its node object or kubelet state and skip join for that node

if a different node appears to have initialized the cluster
  fail and require explicit operator resolution

if kubeconfig output exists and matches
  treat it as idempotent success

if kubeconfig output exists and differs
  require --overwrite-kubeconfig

if join material expired
  create fresh join material from the control-plane API
```

Node-local `OperationRecord`s store enough request, phase, mutation, and
diagnostic state for safe retry decisions on each node. Any `katlctl` client
view may help the operator resume rollout order, but it is not authoritative over
kubeadm or Kubernetes state. Retry decisions must re-check the selected nodes and
API server.

Retry must not assume host generation rollback cleaned a failed kubeadm phase.
It must inspect `/etc/kubernetes`, `/var/lib/kubelet`, `/var/lib/etcd` where
applicable, and Kubernetes API state before deciding whether to skip, rerun, or
require repair.

Retry is operator-triggered through an explicit `katlctl cluster bootstrap`
rerun or repair command. Katlc agent startup audit only classifies node-local
attempts and records diagnostics. It must not automatically rerun `kubeadm
init`, rerun `kubeadm join`, refresh expired join material, or continue
multi-node rollout ordering after power loss.

### Day-One Failure Contract

Day-one bootstrap supports inspectable failure and operator-mediated
re-attempt. It does not support automated repair, automated rebuild, or hidden
cleanup of kubeadm, kubelet, etcd, or Kubernetes API state.

For `bootstrap-init` and `bootstrap-join-worker`, every accepted operation must
record enough state for an operator to decide whether a new attempt is safe:

```text
operation id and parent operation id, when retrying
operation kind: bootstrap-init or bootstrap-join-worker
request digest and redacted bootstrap request summary
expected current generation and candidate generation, when rendered
phase plan, current phase, completed phases, and terminal result
pre-exec mutation marker before each kubeadm invocation
mutation scopes that may have been touched
whether external mutation started or a mutating tool ran
redacted invocation summaries and diagnostic artifact paths
retryHint and nextAction for operator-visible status
```

Pre-mutation failure means no kubeadm, kubelet, etcd, or Kubernetes API
mutation boundary was crossed. Typical examples are rejected input, missing
cluster intent, unreachable node agent, profile/rendering failure, artifact
validation failure, or candidate generation rendering failure before kubeadm is
started. If the request is rejected before an operation is accepted, no
`OperationRecord` is created. If an accepted operation fails before mutation, it
uses the existing operation result vocabulary such as `timed-out` or
`failed-needs-repair`, and `mutationScopes[]`, `preExecMutationMarkers[]`,
`externalMutationStarted`, and `mutatingToolRan` must prove that no external
bootstrap mutation happened. Generation 0 remains the persistent boot target. A
re-attempt is allowed after operator action fixes the cause, such as updating
config, cluster intent, artifacts, network reachability, bootstrap profile
input, or agent credentials. The re-attempt is a new operation with its own
operation id and request digest unless the exact same request digest is being
retried.

Post-mutation failure means kubeadm, kubelet, etcd, or Kubernetes API state may
have changed. The operation must end `failed-needs-repair` or another terminal
state with `recoveryRequired=true`; it must preserve mutation markers,
diagnostic artifacts, and the touched mutation scopes. Host generation rollback
or reboot may make generation 0 reachable again, but it must not claim to undo
`/etc/kubernetes`, `/var/lib/kubelet`, `/var/lib/etcd`, bootstrap tokens, static
pods, or API objects. A re-attempt is allowed only after the operator inspects
the preserved evidence and changes the relevant input or environment, or
chooses an explicit repair/retry workflow once that workflow exists. Day-one
Katl must refuse automatic replay of post-mutation and ambiguous records.

Generation 0 reachability is the day-one escape hatch, not a Kubernetes repair
mechanism:

```text
rollback: select generation 0 as the host boot target when boot selection and
  boot health state allow it
reboot: return to the currently selected healthy generation when no candidate
  boot target was committed
reinstall: wipe/reinstall the node when local Kubernetes state is too uncertain
  for supported day-one retry
```

Operator status must expose enough evidence without requiring shell access:

```text
KatlcAgent.GetNodeStatus over TCP gRPC
KatlcAgent.GetOperation over TCP gRPC
KatlcAgent.WatchOperation over TCP gRPC
katlctl cluster bootstrap status, backed by the agent API
redacted diagnostic artifact names and paths
```

Local `katlc` debug commands, if present later, are break-glass wrappers around
the same agent API. They are not the normal operator UX and must not create a
second supported operation status path.

Normal output and records must show the blocking check and the expected operator
action, but must not include raw bootstrap tokens, discovery tokens,
kubeconfigs, certificate keys, bearer tokens, or client private key material.

Explicitly deferred to day two:

```text
automatic kubeadm reset or cleanup
automatic etcd repair or member removal
automatic control-plane rebuild
ambiguous post-mutation recovery
clearing recoveryRequired without a recorded repair or retry operation
destructive wipe/reinstall as an implicit retry side effect
```

## Failure Diagnostics

On failure, collect redacted diagnostics:

```text
phase name and selected node
node readiness failures
kubeadm command exit status
redacted kubeadm stdout/stderr
containerd and kubelet status snippets
static pod manifest presence
selected endpoint reachability
API readyz/livez errors
node-local operation IDs plus any invocation summary with init node, addresses,
roles, phases, and artifact versions
```

Diagnostics should identify what to retry and what must be repaired manually.
They must not print tokens, certificate keys, kubeconfig private data, or secret
material in normal output.

Redaction applies before data enters normal operation records, including argv,
environment, stdout/stderr, rendered temporary join configs, kubeconfigs, and
diagnostic artifacts. Katl must never store raw bootstrap tokens, discovery
tokens, certificate keys, kubeconfig private keys, bearer tokens, or client
certificate/key data in normal records. Store only presence, expiry/source
metadata, and optional HMAC fingerprints for correlation.

## Post-Bootstrap User Ownership

After cluster bootstrap exits, the user owns any CNI, CoreDNS, kube-proxy
policy, CRDs, Flux, Helm releases, storage, ingress, routing, and workloads with
their chosen cluster tooling. `katlctl cluster bootstrap` may apply explicitly
provided `--bootstrap-manifest` inputs as a bounded handoff step, but it does not
select a production distribution or manage add-on lifecycle.

## Non-Goals

The command does not:

```text
install, apply, select, or manage a production CNI
install, apply, select, or manage CoreDNS or kube-proxy lifecycle
install, apply, select, or manage Flux, GitOps, Helm, CRDs, or workloads
install, apply, select, or manage BIRD/VIP/load-balancer resources
continuously reconcile nodes
perform hidden kubeadm upgrades during config activation
run kubeadm from install manifests or generated confext activation
perform disaster recovery, etcd snapshot restore, CA recovery, automatic
  control-plane replacement, or same-cluster rebuild
```

## Follow-Up Work

Existing follow-up work covers the implementation path:

```text
bootstrap node inventory and readiness checks

node install-to-bootstrap state machine feeding generation 0 health and candidate
generation readiness checks

operator kubeconfig materialization

cluster bootstrap CLI command

two-node kubeadm join VM scenario

usable multi-node cluster smoke

additional control-plane join smoke

config-change handling after bootstrap
```

These are sufficient follow-ups for this decision.
