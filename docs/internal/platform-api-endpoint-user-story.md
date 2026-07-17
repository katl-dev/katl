# Platform API Endpoint User Story

Status: current design.

Katl installs generation 0 nodes and provides a bounded `katlctl` control client
that asks node-local `katlc` to create kubeadm-ready candidate generations and
run explicit kubeadm operations. It does not become a Kubernetes distribution or
an add-on manager. The Kubernetes API endpoint is the main place where those
boundaries meet: the endpoint must exist early enough for kubeadm, `katlctl`,
joining nodes, and operator kubeconfig output. Later user-installed cluster
components may use or replace that endpoint, but the mechanism that advertises
or load-balances it is user infrastructure, a later Katl capability, or a
post-Cilium cluster resource.

This document defines the user story for that platform API endpoint. The
optional dynamic-routing helper is a later capability documented separately in
`docs/internal/platform-api-endpoint-routing-capability.md`.

## User Story

A Katl cluster author wants to:

```text
1. describe installed nodes and kubeadm intent with Katl-native input
2. boot or install nodes until generation 0 installed-runtime health is reached
3. run katlctl cluster bootstrap
4. after katlctl exits, install user-owned CNI, CoreDNS, CRDs, Flux, and workloads
5. keep the API endpoint usable for operators and later joins
```

For that story to work, the author must choose how the Kubernetes API endpoint
is reachable. Katl should make that choice explicit, validate it where it can,
and report when the endpoint plan is circular.

The day-one endpoint story should be simple:

```text
use an endpoint that is independently reachable for kubeadm and joins
```

That endpoint may be an external load balancer, a router-owned VIP, a directly
routable control-plane address, DNS that resolves to one of those paths, or a
future opt-in platform helper. It must not be created by the same Cilium, Flux,
or other user-installed resources that need the API endpoint in order to start.

## Endpoint Roles

Katl treats these as separate roles:

```text
bootstrap API reachability
  the path katlctl can use after kubeadm init and while sequencing explicit join
  operation requests

stable API identity
  the kubeadm controlPlaneEndpoint used for certificate SANs, kubeconfig output,
  Cilium k8sServiceHost/k8sServicePort values, and operator access

external API advertisement
  the network mechanism that makes the stable identity reachable from the user
  fabric
```

One address may satisfy all three roles, but Katl must not assume that it does.
The operator contract must say which role an endpoint is meant to satisfy.

## Katl Responsibilities

Katl owns the platform contract around the endpoint:

```text
render kubeadm controlPlaneEndpoint into native kubeadm input
validate endpoint syntax in katlc and katlctl inputs
record endpoint choice and CLI overrides in bootstrap operation diagnostics
wait for API readiness after kubeadm init
verify join-time endpoint reachability when joining nodes
optionally verify a stable endpoint before exporting kubeconfig output that uses it
export kubeconfig output only after the selected endpoint path is known reachable
redact endpoint wait diagnostics that may contain credentials
```

Katl should eventually model endpoint provenance explicitly:

```text
external
  reachable through user infrastructure before kubeadm and joins need it

platform-host
  reachable through a host or Katl platform helper before kubeadm and joins need it

bootstrap-node
  reachable through the selected init node address; useful for lab or staged
  bootstrap flows

post-cilium
  reachable only after Cilium or user-installed resources advertise it
```

`post-cilium` may be valid for later operator access, but it is invalid as the
only API path for kubeadm bootstrap, joins, or the first kubeconfig output.

## Reachability Vantage Points

Endpoint readiness is not a single boolean. Katl must name the vantage point for
every endpoint wait.

Relevant vantage points:

```text
operator runner
  the host where the katlctl client is invoked; needed for control-client
  reachability and kubeconfig output

init control-plane node
  where kubeadm init creates the first apiserver

joining nodes
  control-plane or worker nodes that must reach the API during kubeadm join

early add-ons
  CNI, CoreDNS, Flux, or other user-installed components that must talk to the API while
  they start
```

A successful operator-runner probe does not prove that joining nodes or early
add-ons can reach the same endpoint. The bootstrap plan should say which waits
are operator-only and which waits prove node reachability; add-on reachability is
user-owned after bootstrap.

Day-one checks may start with bounded operator-runner probes because that is
what `katlctl` can reliably perform. Greenfield readiness should still record
the remaining vantage-point gap when Cilium or additional control-plane joins
need reachability from a different network namespace or node.

## User Responsibilities

The cluster author owns the concrete endpoint implementation unless they opt in
to a Katl-supported helper later.

User-owned choices include:

```text
external load balancer or router VIP
DNS records for the stable endpoint name
router policy and fabric reachability
Cilium chart values and Cilium BGP or LB policy
CoreDNS, Flux, and later GitOps lifecycle
whether post-Cilium API advertisement remains enabled after bootstrap
```

If the user says early Cilium resources should talk to the stable endpoint, the
user must also make that endpoint reachable independently of those Cilium
resources. Katl can probe and reject obvious contradictions, but Katl cannot
infer an entire fabric from a hostname.

## Valid Paths

External endpoint:

```text
1. user provides external load balancer, router VIP, or routable DNS target
2. Katl renders kubeadm controlPlaneEndpoint
3. node-local katlc runs kubeadm init and join operations
4. katlctl verifies the endpoint before exporting requested kubeconfig output
5. after katlctl exits, the user installs Cilium, CoreDNS, Flux, and workloads
```

Bootstrap-node endpoint:

```text
1. katlctl uses the selected init node address for initial API access
2. node-local katlc runs kubeadm joins against the selected bootstrap endpoint
3. katlctl exports kubeconfig output for the bootstrap endpoint, or verifies a
   declared stable endpoint before using it
4. after katlctl exits, the user installs CNI and other add-ons
```

Future opt-in platform helper:

This path is intentionally unavailable until the helper app sysext contract
exists. Before that, valid day-one bootstrap paths require user-owned external
or bootstrap-node reachability.

```text
1. helper config and app sysext are present before bootstrap phases that need it
2. host/platform helper advertises or serves the endpoint before kubeadm or joins
   need it
3. katlctl validates endpoint reachability before exporting kubeconfig output
4. Cilium may later peer locally or advertise service routes after it is healthy
```

The helper path is not required for every Katl cluster. It is a future
capability for users who want platform-owned pre-Cilium endpoint reachability
without external load-balancer infrastructure.

## Invalid Or Action-Required Paths

Katl must reject or mark action-required:

```text
Cilium k8sServiceHost points at a VIP created only by the same Cilium manifests
Flux or Helm resources are expected to create the endpoint before they can apply
DNS resolves only after user-installed resources update the fabric
multi-control-plane bootstrap has no explicit controlPlaneEndpoint or equivalent
```

These are not merely timing problems. They are dependency loops.

## Cilium And The API VIP

Cilium may advertise an apiserver VIP after Cilium is healthy. That can be a
valid post-Cilium external advertisement path.

It is not a valid platform bootstrap endpoint when Cilium itself depends on that
same VIP for API access. In that case Cilium is working around a platform
endpoint gap. Katl should surface the gap instead of encoding the loop.

For Cilium-based clusters, the recommended Katl story is:

```text
pre-Cilium
  use an independently reachable platform or external API endpoint

Cilium install
  point Cilium at that reachable endpoint

post-Cilium
  optionally let Cilium advertise service VIPs or an API VIP, but validate this
  as a later state
```

## How This Appears In Katl Tools

`katlc` should eventually compile endpoint intent into the cluster plan:

```text
controlPlaneEndpoint
stableEndpoint
stableEndpoint provenance
whether endpoint reachability is required for kubeadm, joins, or kubeconfig output
whether endpoint provenance is compatible with that phase
```

`katlctl cluster bootstrap` already has the operational shape:

```text
--control-plane-endpoint
--bootstrap-stable-endpoint
```

The missing follow-up is provenance modeling. Until provenance exists, the
operator contract is explicit: endpoint waits during cluster bootstrap are only
for kubeadm, join, and kubeconfig phases. User add-on installation happens after
cluster bootstrap exits.

`katlc` owns node-local helper validation, generation selection, operation
execution, rollback bookkeeping, and status records once this capability exists.
`katlctl` is only a control client: it may request an explicit operation and
report progress or relay explicit client-side command output, but its own
persistent state is limited to communication and known-node config. It must not
render helper artifacts, synthesize cluster state, own durable helper state, or
act as a background reconciler.

## Fit With Katl Scope

This endpoint story keeps Katl inside its boundary:

```text
Katl owns node readiness, kubeadm input, bounded bootstrap sequencing, endpoint
waits, diagnostics, and kubeconfig materialization.

Katl does not own Cilium, Flux, CoreDNS, BIRD, DNS, router policy, or an
always-on endpoint controller by default.
```

Optional endpoint helpers can be supported later through app sysexts and bounded
native inputs. They must still be explicit user choices with tests, status, and
rollback behavior. They do not become hidden behavior behind `controlPlane`.

## Readiness Gates

Greenfield readiness should prove:

```text
katlctl can bootstrap with an independently reachable controlPlaneEndpoint
katlctl can fail clearly when the endpoint required for kubeadm, joins, or
  kubeconfig output is not reachable
endpoint waits record whether they probe the operator runner or node vantage
  point
Cilium values can target the same stable identity without creating a loop
kubeconfig output is exported only after the selected endpoint is reachable
post-Cilium API advertisement, if used, is validated separately
```

The optional routing helper adds later gates for route export, health-gated
withdrawal, local Cilium peering, and fabric diagnostics.

## Follow-Up Work

Relevant follow-up work:

```text
model bootstrap API endpoint provenance
assess bootstrap endpoint reachability gaps before user-installed Cilium
support richer bootstrap readiness waits
maintain the opt-in platform API endpoint routing capability as later work
```
