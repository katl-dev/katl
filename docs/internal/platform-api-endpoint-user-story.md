# Platform API Endpoint User Story

Status: current design.

Katl prepares kubeadm-ready nodes and provides a bounded `katlctl` bootstrap
coordinator. It does not become a Kubernetes distribution or an add-on manager.
The Kubernetes API endpoint is the main place where those boundaries meet: the
endpoint must exist early enough for kubeadm, `katlctl`, and early user
bootstrap resources, but the mechanism that advertises or load-balances that
endpoint may be user infrastructure, a later Katl capability, or a post-Cilium
cluster resource.

This document defines the user story for that platform API endpoint. The
optional dynamic-routing helper is a later capability documented separately in
`docs/internal/platform-api-endpoint-routing-capability.md`.

## User Story

A Katl cluster author wants to:

```text
1. describe installed nodes and kubeadm intent with Katl-native input
2. boot or install nodes until they are kubeadm-ready
3. run katlctl cluster bootstrap
4. install user-owned CNI, CoreDNS, CRDs, Flux, and workloads
5. keep the API endpoint usable for operators and later joins
```

For that story to work, the author must choose how the Kubernetes API endpoint
is reachable. Katl should make that choice explicit, validate it where it can,
and fail before applying user bootstrap resources when the endpoint plan is
circular.

The day-one endpoint story should be simple:

```text
use an endpoint that is independently reachable before user manifests apply
```

That endpoint may be an external load balancer, a router-owned VIP, a directly
routable control-plane address, DNS that resolves to one of those paths, or a
future opt-in platform helper. It must not be created by the same Cilium, Flux,
or other user manifests that need the API endpoint in order to start.

## Endpoint Roles

Katl treats these as separate roles:

```text
bootstrap API reachability
  the path katlctl can use after kubeadm init and before user bootstrap
  manifests apply

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
record endpoint choice and CLI overrides in bootstrap run diagnostics
wait for API readiness after kubeadm init
optionally wait for an independently reachable stable endpoint before manifests
optionally wait for a stable endpoint after user bootstrap handoff
write kubeconfig only after the selected endpoint path is known reachable
redact endpoint wait diagnostics that may contain credentials
```

Katl should eventually model endpoint provenance explicitly:

```text
external
  reachable before user manifests through user infrastructure

platform-host
  reachable before user manifests through a host or Katl platform helper

bootstrap-node
  reachable through the selected init node address; useful for lab or staged
  bootstrap flows

post-cilium
  reachable only after Cilium or user manifests advertise it
```

`post-cilium` may be valid for post-handoff validation, but it is invalid as the
only pre-Cilium API path.

## Reachability Vantage Points

Endpoint readiness is not a single boolean. Katl must name the vantage point for
every endpoint wait.

Relevant vantage points:

```text
operator runner
  where katlctl runs; needed for bootstrap coordination and kubeconfig output

init control-plane node
  where kubeadm init creates the first apiserver

joining nodes
  control-plane or worker nodes that must reach the API during kubeadm join

early add-ons
  CNI, CoreDNS, Flux, or other user manifests that must talk to the API while
  they start
```

A successful operator-runner probe does not prove that joining nodes or early
add-ons can reach the same endpoint. The bootstrap plan should say which waits
are operator-only and which waits prove node or add-on reachability.

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
3. katlctl runs kubeadm init
4. katlctl waits for the endpoint before Cilium manifests when requested
5. user bootstrap resources install Cilium, CoreDNS, Flux, and workloads
```

Bootstrap-node endpoint:

```text
1. katlctl uses the selected init node address for initial API access
2. user bootstrap resources install CNI and other add-ons
3. a stable endpoint is validated after the resources that provide it are ready
4. kubeconfig is written or updated only after that endpoint is reachable
```

Opt-in platform helper:

```text
1. helper config and app sysext are present before bootstrap phases that need it
2. host/platform helper advertises or serves the endpoint before Cilium
3. katlctl validates endpoint reachability before Cilium manifests
4. Cilium may later peer locally or advertise service routes after it is healthy
```

The helper path is not required for every Katl cluster. It is a future
capability for users who want platform-owned pre-Cilium endpoint reachability
without external load-balancer infrastructure.

## Invalid Or Action-Required Paths

Katl must reject or mark action-required:

```text
stableEndpointBeforeManifests=true but endpoint provenance is post-cilium
Cilium k8sServiceHost points at a VIP created only by the same Cilium manifests
Flux or Helm resources are expected to create the endpoint before they can apply
DNS resolves only after user bootstrap resources update the fabric
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
whether pre-manifest endpoint reachability is required
whether endpoint provenance is compatible with that phase
```

`katlctl cluster bootstrap` already has the operational shape:

```text
--control-plane-endpoint
--bootstrap-stable-endpoint
--bootstrap-stable-endpoint-before-manifests
```

The missing follow-up is provenance modeling. Until provenance exists, the
operator contract is explicit: before-manifests endpoint waits are only for
endpoints that are already reachable without the manifests being applied.

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
rollback behavior. They do not become hidden behavior behind `systemRole`.

## Readiness Gates

Greenfield readiness should prove:

```text
katlctl can bootstrap with an independently reachable controlPlaneEndpoint
katlctl can fail clearly when the pre-manifest endpoint is not reachable
endpoint waits record whether they probe the operator runner, node, or add-on
  vantage point
user bootstrap manifests can be held until the endpoint gate passes
Cilium values can target the same stable identity without creating a loop
kubeconfig is written only after the selected endpoint is reachable
post-Cilium API advertisement, if used, is validated separately
```

The optional routing helper adds later gates for route export, health-gated
withdrawal, local Cilium peering, and fabric diagnostics.

## Follow-Up Work

Relevant follow-up work:

```text
model bootstrap API endpoint provenance
assess bootstrap sequencing gaps for Cilium and API endpoint handoff
support richer bootstrap readiness waits
maintain the opt-in platform API endpoint routing capability as later work
```
