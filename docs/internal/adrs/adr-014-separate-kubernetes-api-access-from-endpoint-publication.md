# ADR-014: Separate Kubernetes API access from endpoint publication

Status: accepted.

Date: 2026-08-31. Accepted: 2026-08-31.

## Context

Katl must provide a reliable path from installed nodes to a Kubernetes API that
an operator can use to install their chosen CNI and other cluster components.
That path should not require an external load balancer or require the operator
to express their routing fabric through a Katl-specific abstraction.

Katl's current endpoint design combines several responsibilities: the kubeadm
control-plane endpoint, local VIP ownership, API health, BGP configuration, and
route publication. These responsibilities do not need the same owner.

A network with multiple per-node `/31` links, distinct peers, source addresses
and site-specific routing policy is better represented by native BIRD
configuration than an expanding Katl BGP schema. Conversely, requiring a user
to implement their permanent API endpoint before bootstrap prevents them from
using Kubernetes components such as Cilium to publish that endpoint afterwards.

The North Star places predictable Kubernetes host prerequisites and bootstrap
handoff within Katl, while leaving CNI and site routing to the user. This
decision establishes a bounded API-access capability at that boundary.

## Decision

**Katl will provide healthy Kubernetes API access independently of how the
canonical endpoint is published.**

Katl will provide a node-local API proxy, expose a Kubernetes-facing proxy
listener for workstation access, and retain optional health-gated ownership of
an API VIP.

Users will own routing configuration and route publication through native host
extensions, cluster components or external infrastructure. Katl's built-in BGP
configuration and routing implementation will be removed once the replacement
journeys and migration path are proven.

The two supported user journeys are:

1. **Host-provided publication:** the user configures BIRD or equivalent
   infrastructure before bootstrap. Katl manages local API eligibility, and
   the user's routing system publishes eligible API paths.
2. **Proxy-first bootstrap:** the user bootstraps through Katl's API proxy,
   installs CNI with ordinary Kubernetes tooling, and then configures
   publication of the canonical endpoint.

These are publication strategies, not mutually exclusive installation modes.
Both can use the same proxy and optional health-gated VIP. An existing external
load balancer or router-provided endpoint remains supported.

## Endpoint model and ownership

| Concept | Responsibility |
|---|---|
| **Canonical control-plane endpoint** | The durable endpoint recorded in kubeadm configuration and API certificates. The user selects its identity; Katl preserves it and integrates it with bootstrap. |
| **Direct control-plane addresses** | Node-specific API addresses reachable through host networking. Katl uses them for proxy backends and bootstrap operations. |
| **Node-local API proxy** | Katl-provided access to healthy API backends for kubelet, Cilium and other host-local clients. |
| **Workstation-facing proxy listener** | A normal Kubernetes-facing TCP listener on a reachable control-plane node address. Katl can export a kubeconfig targeting it. |
| **Optional API VIP** | Katl owns the exact local address and its eligibility according to local API readiness. |
| **Underlay and endpoint publication** | The user owns links, routes, BGP sessions, policy, DNS, load balancing and fabric integration. |

`controlPlaneEndpoint` is not a routing-mode selector. It retains its native
kubeadm meaning; Katl must not treat it as an inert label or assume that
changing discovery alone removes kubeadm's dependencies on it.

For proxy-first bootstrap, Katl must provide a tested integration that preserves
the canonical endpoint while completing the necessary operations through
independently reachable API paths.

## Requirements

### R1. Bootstrap must not depend on CNI or canonical endpoint publication

Katl must be able to complete supported kubeadm initialization and joins when
the canonical endpoint does not resolve or is not routable.

Bootstrap must not require Cilium, Kubernetes Service forwarding, cluster DNS,
or a published API VIP.

When kubeadm control-plane join phases internally return to the canonical
endpoint after direct discovery, Katl temporarily directs that canonical
destination to the selected coordinator's direct API address. The TLS name and
cluster-wide endpoint configuration remain canonical, and the temporary host or
packet-routing state is removed when the join command exits.

The successful handoff is a functioning control plane, completed supported node
joins, and an authenticated kubeconfig that works from the workstation. Node
`Ready` and workload networking are subsequent outcomes of installing CNI, not
mandatory bootstrap completion gates.

No mandatory “install CNI, then resume bootstrap” workflow is introduced.

### R2. Host connectivity remains a prerequisite

Nodes must have the host-network connectivity required for the selected kubeadm
topology, including access to direct control-plane API addresses and the
required control-plane peer communication.

The workstation must reach the Katl management interfaces used for bootstrap
and at least one supported Kubernetes API access path. In the proxy-first
journey, that path is a control-plane node's workstation-facing proxy listener.

**Management connectivity alone is not sufficient for this design:** the
Kubernetes-facing listener is separate from the management API.

Where BIRD supplies this underlying connectivity, the user must configure and
activate it before bootstrap. The proxy does not create missing routes.

### R3. Katl must return an ordinary, usable kubeconfig

The exported kubeconfig must work with unmodified Helm, kubectl and other
Kubernetes clients after `katlctl` exits.

Katl must verify the selected access path before exporting it as usable. It may
use the canonical endpoint when verified; otherwise it must use a verified node
proxy and clearly identify that choice.

The normal workflow must not require a foreground workstation tunnel, a resident
`katlctl` process, or Katl-mediated CNI installation.

“Bootstrap kubeconfig” describes the access path. It does not mean that a
kubeadm join token is used as the operator's credential.

### R4. Node-local API access must remain independent of publication

Katl must provide a documented local endpoint on each Kubernetes node. This ADR
uses:

```text
127.0.0.1:7445
```

Proxy backends must be direct, node-specific API server addresses, not the
shared VIP or another proxy. Backend information must be available before
Kubernetes and persist across reboot.

The proxy must select eligible API backends and stop selecting failed backends
within a defined, tested bound. Control-plane nodes may prefer their local API
when eligible, but must be able to use a peer.

Kubelet and Cilium must be able to retain their local endpoint after the
canonical endpoint is published. Publishing or withdrawing the VIP must not
require migrating those clients.

Failover applies to new or retried connections. Existing TCP streams are not
transparently transferred between API servers.

### R5. Optional VIP ownership must represent local API readiness

Katl will retain the health-gated local VIP contract described in ADR-012: the
`katl-api` interface is a native integration point, and Katl adds or removes the
exact configured VIP according to local API health. Routing software observes
that address and owns its propagation.

The contract is:

```text
Before local API readiness:
  katl-api exists without the VIP

After local API readiness:
  Katl assigns the configured VIP

On local API readiness failure or lifecycle withdrawal:
  Katl removes the configured VIP
```

The health target must be the **actual local kube-apiserver**, not a proxy that
can answer through a healthy peer. Eligibility must not depend on CNI readiness,
Node `Ready`, or the external VIP itself.

Startup must be withdrawn. Service failure and crash recovery must provide
bounded cleanup so that an abandoned address does not indefinitely imply API
eligibility.

Katl owns local address eligibility, not remote convergence. It cannot guarantee
withdrawal if user routing policy independently originates the same prefix or
retains stale routes.

### R6. TLS and Kubernetes authorization must remain end-to-end

The proxy must forward Kubernetes TLS without terminating it, injecting
credentials or bypassing API-server authorization.

Generated certificates and kubeconfigs must support the intended canonical,
direct and localhost access paths without disabling certificate verification.

For example, a workstation kubeconfig may connect to a node proxy while
validating the canonical API identity:

```yaml
server: https://10.20.0.11:7445
tls-server-name: api.home.arpa
certificate-authority-data: ...
```

The workstation-facing listener must use declared node addresses and an
explicit exposure policy, rather than silently opening an unrestricted wildcard
listener. Its address, port and exposure must be visible in planning and status.

Forwarding must not require administrator credentials. Any authenticated health
checking must use an appropriately bounded credential.

### R7. Lifecycle and recovery must preserve independent API access

Proxy configuration must be persistent, generation-aware host state. It must
not depend exclusively on live Kubernetes discovery.

Control-plane addition, replacement and removal must update backend information
through supported operations without leaving clients permanently dependent on
the original init node.

Reboot, kubelet restart, client-certificate bootstrap and rotation, and
supported Kubernetes upgrades must preserve the local API access path.

A cold start must not depend on Cilium or canonical endpoint publication to
recover the API access that those components need. Host-generation rollback
must not be presented as rollback of Kubernetes membership, etcd state or user
routing policy.

### R8. Status must distinguish access, readiness and publication

Katl must report these independently:

| Status dimension | Example |
|---|---|
| Bootstrap | Supported init/join operations complete |
| Proxy access | Listener available; eligible API backends identified |
| Local API eligibility | Ready, unready or administratively withdrawn |
| Local VIP ownership | Configured address present or absent, with reason |
| Canonical endpoint | Verified reachable, unreachable or not checked |
| Cluster networking | Awaiting user-installed CNI or observed ready |

A cluster must not be reported as failed solely because CNI or optional endpoint
publication is pending. Conversely, successful proxy access must not be reported
as proof that the canonical endpoint works.

A kubeconfig targeting one node's proxy must be identified as node-dependent
access, not a fully highly available external endpoint.

## Configuration boundary

The existing endpoint identity and optional VIP shape can be retained:

```yaml
spec:
  controlPlaneEndpoint:
    host: api.home.arpa
    port: 6443
    advertisement:
      vip: 10.40.0.10
```

In this shape, `advertisement.vip` requests **health-gated local address
ownership**, not Katl-managed route propagation. Omitting it leaves address
ownership external.

Katl will not accept BGP peers, ASNs, source interfaces, route exchanges or
export policy within the endpoint configuration.

User-owned BIRD configuration remains native configuration delivered with an
opaque system extension. Katl manages the extension's generic artifact,
generation and service-activation mechanics, not its routing semantics.

The proxy backend set should derive from existing node identity and Kubernetes
address information. Users should not have to maintain a duplicate upstream
list or configure a separate bootstrap endpoint.

This ADR does not authorize arbitrary changes to an initialized cluster's
canonical endpoint. Identity changes still require an explicitly supported
lifecycle operation.

## User journey 1: Host-provided publication

**User need:** Publish the Kubernetes API through an existing routed fabric,
including topologies too complex for a Katl routing schema.

1. **Prepare host networking.** The user configures native networking and a BIRD
   extension, including per-node `/31` links, peers and site routing policy.
2. **Declare the API identity and optional VIP.** The user configures BIRD to
   observe `katl-api` and export the exact API prefix according to their policy.
3. **Run bootstrap from the workstation.** Katl uses its management interfaces
   and local bootstrap operations to start the first control plane. It does not
   wait for a VIP that cannot yet be healthy.
4. **Publish the first healthy API path.** Once the local API passes readiness,
   Katl assigns the VIP. BIRD observes the address and propagates the route.
   Additional healthy control planes become eligible in the same way.
5. **Install cluster networking.** Katl returns a verified kubeconfig. The user
   installs CNI and other components with their existing tools.

The routing configuration exists before bootstrap, but the API endpoint becomes
usable only after an API server starts and passes readiness.

The resulting canonical access path is:

```text
Workstation
  → API VIP
  → fabric-selected eligible control plane
  → local kube-apiserver
```

There is no cluster-wide backend selector in this direct-VIP path. Each node
determines its own eligibility; the user's routing system selects among
published paths.

**Outcome:** The operator keeps full control of the fabric while Katl supplies
the local API-health contract. The proxy remains available as an independent
access path.

## User journey 2: Proxy-first bootstrap and later publication

**User need:** Bootstrap Kubernetes and install Cilium before implementing a
shared, externally reachable API endpoint.

1. **Prepare ordinary host connectivity.** Nodes can communicate as required,
   and the workstation can reach the management interfaces and a control-plane
   proxy listener.
2. **Declare the canonical API identity.** The user may also declare a
   health-gated VIP without publishing it yet.
3. **Run bootstrap.** Katl completes supported kubeadm initialization and joins
   while the canonical endpoint remains unavailable.
4. **Receive a working kubeconfig.** The kubeconfig targets a reachable node
   proxy, which forwards to an eligible API server. `katlctl` exits.
5. **Install Cilium with ordinary tooling.** The user runs Helm, kubectl or their
   existing bootstrap workflow against that kubeconfig. Cilium uses the
   node-local API endpoint.
6. **Publish the canonical endpoint.** The user configures Cilium or other
   infrastructure to make the permanent endpoint reachable, verifies it, and
   switches normal workstation access to it.

During bootstrap:

```text
Workstation
  → control-plane node address:7445
  → Katl API proxy
  → eligible API server's direct address:6443
```

Endpoint-specific Cilium values are:

```yaml
k8sServiceHost: 127.0.0.1
k8sServicePort: "7445"
```

These describe API access only; the remaining Cilium installation and
configuration remain user-owned.

The preferred Cilium integration to validate is publication of the same
health-gated VIP on `katl-api`. Exact support and configuration must be
established against the selected Cilium version before Katl documents that
composition as supported.

A Service-backed API VIP is also a valid user choice, provided it includes
working forwarding and maintained API-backend eligibility. A route announcement
alone is not an endpoint implementation.

**Outcome:** The user obtains a usable Kubernetes API without first deploying a
load balancer or API advertisement mechanism. Kubernetes components can
subsequently publish the canonical endpoint without becoming dependencies of
their own API access.

## Failure and recovery behaviour

The proxy and direct-VIP paths have different health semantics:

| Event | Required behaviour |
|---|---|
| Local API fails but a peer remains healthy | The node proxy may use the peer. The failed node's direct API VIP must nevertheless be removed. |
| Cilium or VIP publication fails | Node-local clients and workstation proxy access remain usable where independent host connectivity survives. |
| The node named in the bootstrap kubeconfig fails | The operator must select another reachable node proxy or use the canonical endpoint. A single-address kubeconfig does not provide automatic host failover. |
| All API backends are unavailable | The proxy reports no eligible backend. It must not claim API readiness. |
| The whole cluster restarts | Persistent direct backend information and host networking permit recovery without requiring Cilium publication first. |

## Non-goals

This decision does not make Katl a routing distribution, DNS manager, CNI
manager or cluster add-on lifecycle controller.

It does not introduce a required management-channel tunnel, arbitrary port
forwarding, mandatory manifest application, or a second bootstrap command after
CNI installation.

It does not promise a highly available external endpoint merely because every
node has a proxy. External availability still depends on the user's publication
strategy and network.

## Acceptance criteria

| Scenario | Required result |
|---|---|
| **Unpublished canonical endpoint** | Multi-control-plane and worker bootstrap succeeds with deliberately unavailable canonical DNS/VIP, and exports a usable proxy kubeconfig. |
| **Ordinary workstation tooling** | After `katlctl` exits, unmodified Helm or kubectl can install CNI using the exported kubeconfig without a tunnel or helper process. |
| **Complex host routing** | A user-owned BIRD extension with multiple per-node `/31` peerings publishes the API VIP without any Katl BGP schema fields. |
| **Local eligibility** | A local API failure withdraws that node's VIP even while its proxy can successfully reach a peer. |
| **Proxy failover** | Failed API backends stop receiving new connections within the documented bound; clients reconnect to eligible peers with normal TLS validation. |
| **Late publication** | A pinned, tested Cilium configuration or other publication mechanism makes the canonical endpoint usable without reinitializing Kubernetes or moving node-local clients away from their proxies. |
| **Cold start and publication outage** | With sufficient healthy control-plane members and independent host connectivity, API access recovers while Cilium publication is unavailable. |
| **Lifecycle persistence** | Reboots, kubelet restarts, certificate issuance/rotation, supported upgrades and control-plane replacement preserve usable local API access. |
| **Security and exposure** | Listener binding follows the declared exposure policy; untrusted certificates and unauthorized Kubernetes requests are rejected normally. |
| **Truthful status** | Tests distinguish bootstrap completion, CNI readiness, proxy availability, local VIP eligibility and canonical endpoint reachability. |

## Implementation and migration gate

The architecture is accepted, but replacement of the existing implementation
remains gated on proof.

Before replacing the existing implementation, Katl must demonstrate the
supported kubeadm paths with the canonical endpoint unavailable. Coverage must
include initialization, discovery, subsequent join phases, generated
kubeconfigs, kubelet TLS bootstrap, certificate handling and supported
upgrades—not merely the first successful API connection.

The integration must remain bounded. This decision does not authorize a kubeadm
fork or an extensive reimplementation of its state machine. If compatibility
requires that, the decision must be revisited before removing the existing path.

Existing built-in BGP users need an explicit migration to user-owned
publication. Katl must not silently ignore removed BGP fields or withdraw an
existing endpoint during an unrelated upgrade.

This ADR supersedes the built-in BGP portions of the routed endpoint design
while retaining ADR-012's user-owned extension and health-gated VIP boundaries.

## Consequences and intended outcome

Katl gains a persistent API-access component and the responsibility to integrate
it correctly with kubeadm, certificates, membership changes and lifecycle
operations.

In exchange, Katl no longer needs to model the user's routing fabric. The same
bootstrap behaviour works with ordinary host networking, native BIRD
configurations and independently provided load balancers.

The operator-facing outcome is:

> **Katl provides a working Kubernetes API and kubeconfig. Users may publish
> that API through host infrastructure prepared before bootstrap, or use the
> proxy to install the cluster components that publish it afterwards.**

Katl owns healthy API access and optional local VIP eligibility. Users own how
those paths are exposed to their network.
