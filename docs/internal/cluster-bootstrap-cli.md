# Cluster Bootstrap CLI Contract

Status: current decision.

This document defines the operator-run command that turns already installed
generation 0 Katl nodes into a Kubernetes cluster. It does not change the Katl
runtime boundary: the command asks `katlc` to create and activate the first
Kubernetes-capable generation from stored intent, coordinates kubeadm init/join,
and users/operators own the cluster contents after bootstrap.

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
  write the operator kubeconfig here

--overwrite-kubeconfig
  replace an existing kubeconfig instead of requiring an exact idempotent match

--dry-run
  validate inventory, access, readiness, and phase plan without running kubeadm

--bootstrap-wait <wait>
  bounded kubeadm-scoped wait; supported forms are api-ready,
  joined-nodes-observed, and control-plane-healthy

--bootstrap-stable-endpoint <host:port>
  stable API endpoint to verify before writing kubeconfig that uses it
```

The command is a bounded coordinator. It runs phases, writes outputs, reports
status, and exits. It is not a daemon, reconciler, add-on manager, CNI manager,
Flux manager, BIRD manager, or cluster lifecycle controller.

## Operation And Run Record Boundary

`katlctl cluster bootstrap` is a bounded coordinator for explicit
`BootstrapCluster` and `JoinCluster` operation attempts. The command writes one
bootstrap run record for the coordinator invocation. The selected init node gets
a `BootstrapCluster` attempt; each worker or later control-plane join gets a
`JoinCluster` attempt. Each attempt asks node-local `katlc` to validate stored
intent and create the first Kubernetes-capable candidate generation before
kubeadm runs.

The run record stores ordering, phase state, selected inputs, CLI overrides,
redacted diagnostics, retry context, and whether kubeadm has mutated node or
cluster state. It is not desired cluster state. The source of truth after
mutation remains kubeadm output, node-local state, and Kubernetes API state. The
command must not remain resident, watch the cluster, or continuously reconcile
failed or missing joins.

The run record must identify the candidate generation ID, the phase where kubeadm
first mutated state, and which mutation scopes were touched. `kubeadm init` can
mutate `/etc/kubernetes`, `/var/lib/kubelet`, `/var/lib/etcd`, and Kubernetes API
objects. `kubeadm join` can mutate `/etc/kubernetes`, `/var/lib/kubelet`, node
objects, and, for control-plane joins, etcd membership. Host rollback after a
failed bootstrap or join does not clean this partial state; retry must inspect
actual kubeadm and Kubernetes state.

## Input Model

Bootstrap input describes installed nodes, not install-time desired state. Each
node entry includes:

```text
name
address
systemRole
access method and credentials reference, not inline secret values
kubeadm configRef from stored intent
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
they must be recorded in the bootstrap run record so diagnostics show what was
actually used.

`systemRole` is the only source of kubeadm bootstrap role:

```text
control-plane
  eligible for kubeadm init or later control-plane join

worker
  eligible only for kubeadm worker join
```

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

The selected init node is recorded in the plan and in the run record. The
command must never try `kubeadm init` on more than one node in one run.

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
stored systemRole and selected KubeadmConfig intent are consistent
node-local katlc can accept an operation request
```

Initial access may be SSH. VM tests may use vsock or harness agents where
available, but the command contract should not depend on a test-harness-only
transport.
All transports must return structured command results with stdout/stderr
redaction. Kubernetes API access starts only after `kubeadm init` has produced a
usable kubeconfig; the API is not a pre-bootstrap coordination channel.

If any node cannot pass pre-bootstrap checks, the command fails before preparing
or running kubeadm anywhere.

## Bootstrap Flow

The bootstrap command runs phases in this order:

These are coordinator phases. Phases that run `kubeadm init` or `kubeadm join`
must also update the corresponding `BootstrapCluster` or `JoinCluster` attempt
in the bootstrap run record.

```text
1. load and validate inventory or compiled plan
2. apply CLI overrides and record them
3. select the init control-plane node
4. verify access and generation 0 installed-runtime state on every node
5. ask katlc on each target node to validate stored intent and create the first
   Kubernetes-capable candidate generation
6. activate the candidate generation and wait for katl-kubeadm-ready.target on
   nodes before their kubeadm phase
7. run kubeadm init only on the selected init node
8. collect or create join material
9. join remaining worker nodes
10. join additional control-plane nodes later, when that path is implemented
11. wait for API readiness using the init or declared endpoint
12. run post-kubeadm health checks and commit each successful candidate generation
13. optionally verify the declared stable endpoint for operator kubeconfig output
14. write operator kubeconfig, using a declared stable endpoint only after the
    endpoint wait succeeds
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
`docs/internal/stacked-etcd-bootstrap-data-policy.md`.

## kubeadm Material

The command runs kubeadm against rendered Katl input created in the candidate
generation:

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

When join material is needed on a node, the command supplies it through a
temporary restricted join configuration or redacted command arguments. Any
temporary file is mode `0600`, lives outside `/etc/katl`, is deleted on a
best-effort basis after use, and is never copied into the run record.

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
requests that mode; the default run record is redacted.

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
writing kubeconfig that uses it. It does not apply the user resources that might
later advertise or replace that endpoint.

Do not add kubePrism as an initial requirement.

The opt-in host/platform endpoint helper design, including BIRD-mediated routing
and pre-Cilium advertisement, is tracked separately in
`docs/internal/platform-api-endpoint-routing-capability.md`.

The broader user story and operator contract for platform API endpoints is
documented in `docs/internal/platform-api-endpoint-user-story.md`.

## Kubeconfig Output

After kubeadm init succeeds and the API is reachable, the command writes an
operator kubeconfig.

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

The bootstrap run record stores enough coordinator and per-operation-attempt
state for diagnostics and safe retry. It is not authoritative over kubeadm or
Kubernetes state; retry decisions must re-check the selected nodes and API
server.

Retry must not assume host generation rollback cleaned a failed kubeadm phase.
It must inspect `/etc/kubernetes`, `/var/lib/kubelet`, `/var/lib/etcd` where
applicable, and Kubernetes API state before deciding whether to skip, rerun, or
require repair.

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
run record with init node, addresses, roles, phases, and artifact versions
```

Diagnostics should identify what to retry and what must be repaired manually.
They must not print tokens, certificate keys, kubeconfig private data, or secret
material in normal output.

## Post-Bootstrap User Ownership

After cluster bootstrap exits, the user installs and owns any CNI, CoreDNS,
kube-proxy policy, CRDs, Flux, Helm releases, storage, ingress, routing, and
workloads with their chosen cluster tooling. `katlctl cluster bootstrap` does not
apply Kubernetes manifests or manage add-on lifecycle.

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
