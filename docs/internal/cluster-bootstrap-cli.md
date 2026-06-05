# Cluster Bootstrap CLI Contract

Status: current decision.

This document defines the operator-run command that turns already installed,
kubeadm-ready Katl nodes into a Kubernetes cluster. It does not change the Katl
runtime boundary: Katl prepares nodes for kubeadm, then kubeadm and
user-managed GitOps/operators own the cluster.

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
  stable endpoint handoff is declared

--kubeconfig-out <path>
  write the operator kubeconfig here

--overwrite-kubeconfig
  replace an existing kubeconfig instead of requiring an exact idempotent match

--dry-run
  validate inventory, access, readiness, and phase plan without running kubeadm

--bootstrap-manifest <path>
  ordered Kubernetes manifest file or bundle to apply after API readiness;
  repeat the flag for multiple files

--bootstrap-wait <wait>
  bounded post-bootstrap wait; supported forms are api-ready, nodes-ready,
  resource-exists[:namespace]:kind/name, and
  condition[:namespace]:kind/name:Condition

--bootstrap-stable-endpoint <host:port>
  stable API endpoint to verify after user bootstrap before writing kubeconfig
```

The command is a bounded coordinator. It runs phases, writes outputs, reports
status, and exits. It is not a daemon, reconciler, add-on manager, CNI manager,
Flux manager, BIRD manager, or cluster lifecycle controller.

## Input Model

Bootstrap input describes installed nodes, not install-time desired state. Each
node entry includes:

```text
name
address
systemRole
access method and credentials reference, not inline secret values
kubeadm configRef or rendered config path
selected Kubernetes payload version
```

The cluster input may also reference a bounded, user-owned bootstrap handoff:

```text
bootstrap.manifests[].path
  ordered manifest file or bundle path

bootstrap.waits[]
  bounded waits using api-ready, nodes-ready, resource-exists with kind/name,
  or condition with kind/name plus condition

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

Before running kubeadm phases, the command verifies every target node is
kubeadm-ready.

Minimum readiness checks:

```text
Katl runtime reached katl-kubeadm-ready.target
selected Kubernetes sysext is active
rendered kubeadm config exists under /etc/katl/kubeadm/<name>/config.yaml
containerd is active and CRI socket responds
kubelet is installed and can be started by kubeadm
/etc/kubernetes is writable projected state, not generated confext
node selected Kubernetes payload version matches the cluster plan
node systemRole and selected KubeadmConfig intent are consistent
```

Initial access may be SSH. VM tests may use vsock or harness agents where
available, but the command contract should not depend on a QEMU-only transport.
All transports must return structured command results with stdout/stderr
redaction. Kubernetes API access starts only after `kubeadm init` has produced a
usable kubeconfig; the API is not a pre-bootstrap coordination channel.

If any node is not ready, the command fails before running kubeadm anywhere.

## Bootstrap Flow

The bootstrap command runs phases in this order:

```text
1. load and validate inventory or compiled plan
2. apply CLI overrides and record them
3. select the init control-plane node
4. verify access and kubeadm-ready state on every node
5. run kubeadm init only on the selected init node
6. collect or create join material
7. join remaining worker nodes
8. join additional control-plane nodes later, when that path is implemented
9. wait for API readiness using the init or declared endpoint
10. optionally run light user bootstrap handoff after API readiness
11. write operator kubeconfig, using a declared stable endpoint only after the
    endpoint handoff wait succeeds
12. print next steps and exit
```

Worker joins must not start until init succeeds and join material exists.
Additional control-plane joins require certificate-key handling and may be
implemented after worker joins. The durable contract is that every non-init
control-plane node is classified for a later control-plane join from
`systemRole`; until that implementation exists, multi-control-plane plans fail
with a clear unsupported message after init-node selection validation.

## kubeadm Material

The command runs kubeadm against rendered Katl input:

```text
kubeadm init --config /etc/katl/kubeadm/<name>/config.yaml
kubeadm join --config /etc/katl/kubeadm/<name>/config.yaml
```

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

stable endpoint handoff
  use a user-declared VIP, DNS name, BIRD-routed endpoint, or load balancer
  after user bootstrap resources make it reachable
```

Katl does not own BIRD, VIP, kube-vip, ingress, load balancer, or DNS lifecycle
as part of this command. The command may wait for a user-declared endpoint after
API readiness and after optional user bootstrap resources run.

Do not add kubePrism as an initial requirement.

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
  a declared handoff wait succeeds
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

The run record should store enough phase state for diagnostics, but the source
of truth remains kubeadm/Kubernetes state on the nodes and API server.

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
They must not print tokens, certificate keys, kubeconfig private data, or full
secret manifests in normal output.

## Optional User Bootstrap Handoff

After API readiness, the command may apply ordered user-supplied bootstrap
resources. This is a handoff, not lifecycle ownership.

Allowed first shape:

```text
ordered manifest files or bundles
server-side apply or create, implementation-defined
waits for API readiness, resource existence/conditions, optional node readiness,
  and optional stable endpoint reachability
```

This can install CNI, CoreDNS, CRDs, Flux, BIRD-related resources, or other
user-owned bootstrap pieces. After the command exits, the user manages those
resources with kubectl, GitOps, or operators.

## Non-Goals

The command does not:

```text
install or select a production CNI
own CoreDNS or kube-proxy lifecycle
own Flux or GitOps lifecycle
own BIRD/VIP/load-balancer lifecycle
continuously reconcile nodes
perform hidden kubeadm upgrades during config activation
run kubeadm from install manifests or generated confext activation
```

## Follow-Up Beads

Existing Beads cover the implementation path:

```text
katl-dty.11.9
  bootstrap node inventory and readiness checks

katl-dty.11.11
  node install-to-bootstrap state machine feeding kubeadm-ready checks

katl-dty.11.10
  operator kubeconfig materialization

katl-dty.11.3
  cluster bootstrap CLI command

katl-dty.11.5
  light user bootstrap handoff after API readiness

katl-dty.11.4
  two-node kubeadm join VM scenario

katl-dty.11.6
  usable multi-node cluster smoke

katl-dty.11.7
  additional control-plane join smoke

katl-dty.11.8 and children
  config-change handling after bootstrap
```

These are sufficient follow-ups for this decision.
