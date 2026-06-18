# Platform API Endpoint Routing Capability

Status: deferred proposal. This is not a supported Katl capability until the
helper-specific app bundle, status schema, operation/apply/rollback behavior,
and ownership rules are defined and tested against the node app sysext
contract.

Katl needs a way to support greenfield clusters where Cilium must reach the
Kubernetes API before Cilium can advertise any service or API VIP. The
capability described here is not part of the default kubeadm-ready node path.
It is an opt-in platform endpoint and dynamic-routing integration for clusters
that do not already have an external load balancer or router-owned API endpoint.

This decision builds on:

```text
docs/internal/cluster-bootstrap-cli.md
docs/internal/node-app-sysext-contract.md
docs/internal/platform-api-endpoint-user-story.md
docs/internal/platform-api-endpoint-helper-input-schema.md
docs/internal/system-roles-and-capabilities.md
docs/internal/supported-node-config-domains.md
```

`docs/internal/platform-api-endpoint-user-story.md` defines the baseline Katl
operator story for endpoint reachability. This document only covers the opt-in
dynamic-routing helper for users who do not already have an independently
reachable platform API endpoint.

## Deferral Gate

This document must not be used as implementation approval. `katlc` must reject
platform API endpoint helper input, and `katlctl` must not expose helper
enablement, until follow-up docs define:

```text
helper app bundle metadata, compatibility, unit, config, health, and status
  schema using the node app sysext contract
exact helper appID, status schema, durability, redaction, and owner
node-local operation records for enable, disable, live apply, withdrawal,
  rollback, and repair
ownership boundaries between Katl, the app sysext, Cilium, GitOps, and user
  infrastructure
```

## Decision

Katl treats pre-Cilium Kubernetes API reachability as platform or external
infrastructure responsibility. Cilium may advertise the apiserver VIP after
Cilium is healthy, but Katl must not rely on Cilium to create the endpoint that
Cilium itself needs in order to start.

The opt-in platform API endpoint routing capability is a dynamic-routing
oriented node application for making the kubeadm `controlPlaneEndpoint`
reachable before Cilium is responsible for advertising anything.

Endpoint roles are separate:

```text
bootstrap API reachability
  the path used by katlctl after kubeadm init and while sequencing explicit join
  operation requests; this path must already be reachable before cluster add-ons
  are user-installed

stable API identity
  the kubeadm controlPlaneEndpoint used for certificate SANs, kubeconfig output,
  and Cilium k8sServiceHost/k8sServicePort values

external API advertisement
  how the stable endpoint becomes reachable from the user network fabric; this
  may be external/router-provided, host-helper-provided, or Cilium-provided
  after Cilium readiness
```

## Future Required Shape

Preferred implementation:

```text
Katl-provided app sysext
  contains the endpoint helper runtime, BIRD or equivalent routing component,
  systemd units, health gate, status reporter, and compatibility metadata
```

Deferred extension path:

```text
user-provided app sysext
  possible only through the same node extension bundle and app sysext contract;
  Katl must not accept arbitrary user sysexts as managed endpoint helpers
```

Bounded native config:

```text
generated confext and systemd-networkd inputs
  may provide typed helper inputs such as a dummy interface, VIP address, and
  base routing config selected by Katl, but raw arbitrary systemd unit or
  confext passthrough is not the helper contract
```

External infrastructure remains valid:

```text
external load balancer, router-owned VIP, or already-routable endpoint
  requires only bounded reachability waits and does not start a host advertiser
```

The capability is opt-in. Katl does not require dynamic routing, BIRD, Cilium
BGP, or a platform API VIP helper for every cluster, and this is not a generic
static-network VIP system.

## Inputs

A future helper design should use typed, bounded inputs:

```text
api endpoint address or VIP prefix
dummy or loopback interface name and address
BIRD or equivalent routing configuration snippets through a bounded schema
fabric router peers, authentication, and policy fields
local Cilium BGP peer settings for Cilium-to-local-BIRD peering
protocol boundary, with BGP direct as first class
optional BGP-to-OSPF translation at the local BIRD or fabric boundary
health probe target, defaulting to local kube-apiserver /readyz
advertisement enablement policy
status output path and retention policy
```

The field-level schema proposal is sketched in
`docs/internal/platform-api-endpoint-helper-input-schema.md`; it is not an
accepted implementation contract.

Static and no-dynamic-routing fabrics are allowed as user-owned environments,
but they are not first-class for this helper.

## Routing Adjacency

When the platform endpoint helper is enabled, the preferred topology is:

```text
fabric routers <-> local BIRD or equivalent <-> Cilium
```

The local host routing process is the node's fabric edge:

```text
fabric routers peer with local BIRD
Cilium peers locally with BIRD for service or workload route export
BIRD owns fabric-facing export policy
BIRD owns route tagging, loop prevention, and protocol translation
```

Direct Cilium-to-router peering remains supported for users who opt out of the
platform helper or deliberately own that topology. It is not the preferred
topology when the helper is enabled because it splits pre-Cilium API reachability
and post-Cilium service advertisement across separate fabric policy boundaries.

External/router-provided API endpoints remain the simplest path when available.
In that mode Katl only verifies reachability and does not run a host advertiser.

## Route Ownership And Policy

Route ownership is phase-specific:

```text
API VIP routes
  platform or host owned before Cilium; used to make controlPlaneEndpoint
  reachable before kubeadm, joins, or initial kubeconfig output need it

Cilium service or workload VIP routes
  Kubernetes and Cilium owned after Cilium is healthy

fabric export policy
  local BIRD or equivalent owns fabric-facing filtering, communities, local-pref
  or MED policy hooks, deny-by-default controls, and protocol translation
```

Loop prevention requirements:

```text
do not import back into Cilium the same service routes Cilium originated
do not let Cilium-originated API VIP advertisement satisfy a pre-Cilium API wait
separate platform API prefixes from Cilium service prefixes
use explicit import/export filters, prefix ownership, tags, or communities
```

## Health And Advertisement

Pre-Cilium API route advertisement is host/platform-owned.

Advertisement is enabled only after local API health passes and is withdrawn on
failure. The default health target is local kube-apiserver `/readyz` reached
through the endpoint path being advertised.

Health semantics:

```text
local health
  gates whether this node should advertise its API path

remote or fabric health
  diagnostic and status input; not the first advertisement authority

Cilium-originated API VIP advertisement
  separate post-Cilium state; must not satisfy the pre-Cilium wait
```

Relying on Cilium to advertise the API VIP as the only API path is a workaround
for missing platform endpoint support and is circular if used before Cilium has
API access.

## Systemd Ordering

The helper must use native systemd ordering:

```text
dummy or loopback VIP setup before routing daemon
routing daemon before advertisement health gate
health-gated advertisement before kubeadm or Cilium phases that require the
  stable endpoint
Cilium local BGP peering after Cilium is installed
```

The helper must not hide kubeadm, kubectl, Helm, CNI installation, or arbitrary
cluster mutations in generated confext activation.

## Status

The node app sysext contract defines the live status path pattern and
operation snapshot path. This helper still needs an app-specific status schema,
durability rules, and redaction policy before implementation. `katlc` owns
node-local status collection and reporting. `katlctl` may display helper status
only by querying node-local Katl state; it must not become the owner of helper
state.

Status must be readable without inspecting raw daemon internals. It should
eventually report:

```text
configured endpoint and prefix
dummy or loopback interface state
routing daemon state
local /readyz result
advertisement state
fabric peer and local Cilium peer session state
last transition time
failure reason
whether routes are currently withdrawn
selected generation or app sysext version
```

Debug bundles may include richer daemon logs, but normal status must be bounded
and redacted.

## Deferred Apply And Rollback Contract

No live helper apply is supported until a node-local `katlc` operation model
exists for this capability. The design must define operation kinds, resource
locks, preflight checks, withdrawal behavior, rollback limits, recovery-required
states, and status snapshots.

Generation rollback may switch selected sysext and confext as a unit, but it
must not claim to roll back user fabric state, Cilium state, GitOps resources, or
Kubernetes API objects. Failed helper activation must fail closed, withdraw
advertisement where the contract permits, and report explicit operator action
when safe restoration is not proven.

Future next-boot changes stage a new generation and become active after boot
selection.

Future live changes are allowed only after domain-specific preflight proves
operator access and routing safety. Examples that require fail-closed preflight
include:

```text
changing the API endpoint address or prefix
changing fabric peers or authentication
changing export filters for platform API prefixes
enabling or disabling local Cilium peering
switching between external, host-helper, and post-Cilium advertisement paths
```

Failed live activation must withdraw unsafe advertisement and roll back to the
prior generation or prior helper configuration where possible. If rollback
cannot restore safe live state, the previous generation remains selected for the
next boot and status must require reboot or operator action.

## Transition Paths

Supported transitions:

```text
external/router endpoint to Katl helper
  add helper, verify local readyz and route export, then move waits and
  kubeconfig to the helper endpoint

Katl helper to Cilium advertisement
  keep helper for pre-Cilium bootstrap, then optionally let Cilium advertise
  post-Cilium routes after Cilium readiness

Cilium-only workaround to platform helper
  add helper first, switch bootstrap endpoint waits to platform provenance, then
  keep or remove post-Cilium API advertisement intentionally
```

Katl must distinguish pre-Cilium platform API reachability from post-Cilium API
VIP advertisement in validation, status, and VM tests.

## Non-Goals

This capability does not make Katl a CNI, BIRD, VIP, ingress, or cluster add-on
lifecycle manager.

This capability does not add BIRD to day-one generated node config domains.

This capability does not provide Talos kubePrism compatibility or a default
localhost API proxy.

This capability does not support arbitrary systemd unit passthrough or generic
package installation.

## Follow-Up Work

Implementation must be split into focused follow-up work:

```text
finalize the platform API endpoint helper app-specific bundle contract, input
schema, status schema, and candidate generated artifacts

package BIRD or equivalent helper as a Katl or fixture app sysext only after the
helper contract defines metadata, unit, status, and compatibility requirements

implement the advertisement health gate and status record

define live versus next-boot apply planner behavior for helper config

verify pre-Cilium reachability, local BIRD export, withdrawal, and diagnostics
in VM tests
```
