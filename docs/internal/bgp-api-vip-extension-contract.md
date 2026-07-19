# BGP API VIP Extension Contract

Status: accepted and partially implemented app-specific contract for the BGP
API VIP node extension. The v0.1 implementation has typed validation,
rendering, cluster-plan composition, fixture bundle metadata, health-gated
controller behavior, and unit coverage. Real mkosi-built packaging, operation
integration, and VM proof remain follow-up work.

The BGP API VIP extension is a Katl-owned node application sysext that makes a
Kubernetes control-plane endpoint reachable through a host-owned VIP before
Cilium can advertise service or API routes. It builds on:

```text
docs/internal/generic-bird-extension-contract.md
docs/internal/node-app-sysext-contract.md
docs/internal/node-extension-bundle-format.md
docs/internal/platform-api-endpoint-routing-capability.md
```

The extension composes with the generic BIRD capability. It owns the API VIP
schema, systemd-networkd VIP ownership, kube-apiserver health gate,
advertisement policy, status schema, and operation semantics. It does not
duplicate BIRD packaging, expose arbitrary BIRD configuration, or make BIRD part
of the KatlOS base runtime.

## Decision

The extension is delivered as a node extension bundle with:

```text
appID: bgp-api-vip
artifactKind: katl.node-app-sysext.v1
payloadVersion: bgp-api-vip-v<katl-semver>
capability: dev.katl.api-endpoint.bgp-vip
required capability: dev.katl.routing.bird
activationPhase: pre-kubeadm
```

The first supported mode is host-advertised API VIP. External load balancers or
router-owned endpoints remain valid platform inputs, but they are not this app.
Cilium-originated API advertisement is post-Cilium state and cannot satisfy the
pre-Cilium endpoint path that this app provides.

Required manifest fields include:

```text
capabilities[]
  name: dev.katl.api-endpoint.bgp-vip
  version: v1alpha1
  configSchemaIDs:
    dev.katl.api-endpoint.bgp-vip.config.v1alpha1
    dev.katl.routing.bird.generated.v1alpha1
  operationKinds:
    bgp-api-vip-validate
    bgp-api-vip-status
    bgp-api-vip-withdraw
    bgp-api-vip-advertise, only after the health gate is satisfied

compatibility.requiredCapabilities[]
  CAP_NET_ADMIN, inherited through generic BIRD
  CAP_NET_BIND_SERVICE, inherited through generic BIRD when BIRD listens on 179

compatibility.requiredKernelModules[]
  dummy, when vipInterface.kind is dummy

status.statusSchemaID
  dev.katl.api-endpoint.bgp-vip.status.v1alpha1

configuration.secretRefKinds[]
  bgp-peer-auth, reserved until the Katl secret materialization contract exists
  kube-apiserver-ca
```

The app bundle contains helper binaries, base units, status helpers, and
metadata. Generated confext contains node-specific config, networkd files, and
bounded config handed to generic BIRD.

The v0.1 cluster-plan composition path uses:

```yaml
spec:
  platformAPIEndpoint:
    mode: hostAdvertisedBGP
    bgpAPIEndpoint:
      endpoint:
        host: api.home.example
        vip: 10.40.0.10/32
```

The composer normalizes this BGP API endpoint, renders the owned native
artifacts for control-plane nodes, exposes app status metadata, and uses the
selected `host:port` value as kubeadm `controlPlaneEndpoint`. External endpoint
inputs use `mode: external` and do not render this app. Cilium-owned API
endpoint provenance is rejected for bootstrap readiness.

## Input Shape

The normalized config shape is:

```yaml
apiVersion: katl.dev/v1alpha1
kind: NodeApplicationConfiguration
spec:
  bgpAPIEndpoint:
    endpoint:
      host: api.home.example
      port: 6443
      vip: 10.40.0.10/32
      addressFamily: ipv4
      tlsServerName: api.home.example
    vipInterface:
      kind: dummy
      name: katl-api0
      mtu: 1500
    routing:
      routerID: 10.0.0.11
      localASN: 64512
      sourceAddress: 10.0.0.11
      sourceInterface: enp1s0
      exportPolicy:
        communities:
        - "64512:100"
        localPreference: 100
        med: 0
    advertiseOn:
      roles:
      - control-plane
    fabricPeers:
    - name: router-a
      address: 10.0.0.1
      asn: 64500
      localASN: 64512
      kind: fabric
      authRef: secret/bgp-router-a
      holdTime: 90s
      keepaliveTime: 30s
      allowedExportPrefixes:
      - 10.40.0.10/32
    devHostPeers:
    - name: operator-laptop
      address: 10.0.0.50
      asn: 64520
      localASN: 64512
      kind: dev-host
      allowedExportPrefixes:
      - 10.40.0.10/32
    advertisement:
      enabled: true
      startWithdrawn: true
      advertiseAfterHealthy: true
      withdrawOnFailure: true
    health:
      probe: readyz
      scheme: https
      host: 10.40.0.10
      port: 6443
      path: /readyz
      interval: 2s
      timeout: 1s
      successThreshold: 2
      failureThreshold: 3
      caRef: kube-apiserver-ca
```

Field names are the accepted product contract for this app. Follow-up Go work
may move this block under the selected Katl generation schema, but the meaning
and ownership rules stay here.

## Endpoint And VIP Rules

`endpoint.host` is the stable Kubernetes API identity used for kubeadm
`controlPlaneEndpoint`, kubeconfig output, and Cilium `k8sServiceHost`.

`endpoint.port` defaults to `6443`.

`endpoint.vip` is required and must be a single-host prefix:

```text
IPv4: /32
IPv6: /128
```

Wider prefixes are rejected. The VIP must not be a pod CIDR, service CIDR,
Cilium-owned advertisement prefix, link-local address, multicast address, or
unspecified address. The VIP prefix is the only platform API prefix exported by
this app in v0.1.

`endpoint.addressFamily` must match `endpoint.vip`. Dual-stack endpoint
advertisement is represented as two explicit app instances only after a
follow-up contract defines multi-instance ownership. v0.1 accepts one VIP per
node generation.

`endpoint.tlsServerName` defaults to `endpoint.host`. Health probes use it when
probing the VIP address through TLS.

## VIP Interface Ownership

`vipInterface` is required. Supported kinds are:

```text
dummy
  preferred v0.1 mode; generated confext owns the dummy netdev and address

loopback
  allowed when the fabric expects loopback-owned host routes; generated confext
  owns only the Katl VIP address assignment and must not replace unrelated lo
  configuration
```

`vipInterface.name` must be a safe Linux interface name, length-bounded, unique
within the node plan, and must not collide with physical interfaces,
user-managed networkd units, Kubernetes CNI interfaces, or Katl runtime
interfaces.

Generated confext owns:

```text
/etc/systemd/network/05-katl-bgp-api-vip.netdev
  dummy netdev, only when vipInterface.kind is dummy

/etc/systemd/network/05-katl-bgp-api-vip.network
  address assignment for endpoint.vip

/etc/katl/apps/bgp-api-vip/config.yaml
  normalized app input and validation digest

/etc/katl/apps/bird/bird.conf
  deterministic BIRD config generated from this bounded app schema

/etc/systemd/system/katl-app-bgp-api-vip.service.d/10-katl-config.conf
  app config and status paths

/etc/systemd/system/katl-app-bird.service.d/20-katl-bgp-api-vip.conf
  selected BIRD config path and readiness requirements
```

The BGP API VIP app owns the VIP interface and address. Generic BIRD owns only
its daemon, control socket, daemon status, and parsed routing config target.

## BGP Local Identity And Peers

`routing.routerID` is required and must be an IPv4 address. IPv6-only BGP still
uses an explicit router ID in v0.1.

`routing.localASN` is required. Peer `localASN` may be omitted only when it
matches `routing.localASN`; normalized config records the effective value on
every peer. Local and peer ASNs may be equal for iBGP or different for eBGP, but
they must be explicit after normalization.

Peers are split by purpose:

```text
fabricPeers[]
  routers or fabric route reflectors that make the API VIP reachable from the
  operator and cluster network

devHostPeers[]
  explicit operator or development hosts used in early VM and lab workflows
```

Both peer lists use the same required fields:

```text
name
address
asn
localASN, explicit or normalized from routing.localASN
kind
allowedExportPrefixes[]
```

Optional fields are:

```text
authRef
holdTime
keepaliveTime
sourceAddress
sourceInterface
communities[]
localPreference
med
```

Peer names are unique safe labels. Peer addresses must be IP addresses, not DNS
names, so early bootstrap does not depend on DNS. `allowedExportPrefixes[]`
must contain only `endpoint.vip` in v0.1.

## Source Address And Interface

`routing.sourceAddress` and peer-level `sourceAddress` are optional. When set,
they must be local non-VIP addresses already owned by the node network plan or
verified host inventory. They must not be `endpoint.vip`.

`routing.sourceInterface` and peer-level `sourceInterface` are optional. They
select the local interface for BGP sessions when route lookup alone is
ambiguous, for link-local IPv6 peers, or for multi-homed hosts. When both global
and peer-level values are present, the peer-level value wins.

When both `sourceAddress` and `sourceInterface` are set, validation must prove
that the source address belongs to that interface. When neither is set, the
kernel route decision may select the BGP TCP session source, but status must
report the observed local session address for each peer.

Validation rejects a source interface that is the VIP dummy interface unless a
follow-up contract proves that topology. The API VIP address is advertised as a
route, not used as the default BGP TCP session source.

## Advertise-On Policy

`advertiseOn.roles[]` controls which Katl node roles may advertise the VIP.
v0.1 supports:

```text
control-plane
```

Worker advertisement is rejected. A control-plane node still starts withdrawn
until local kube-apiserver health succeeds. Non-selected nodes render no VIP
interface, no BGP sessions for this app, and a status record explaining that the
node role is not selected.

## Health Gate

The default health target is local kube-apiserver `/readyz` reached through the
same endpoint path that will be advertised:

```text
scheme: https
host: <endpoint.vip address>
port: <endpoint.port>
path: /readyz
interval: 2s
timeout: 1s
successThreshold: 2
failureThreshold: 3
tlsServerName: <endpoint.tlsServerName>
caRef: kube-apiserver-ca
```

The health gate is node-local. Remote fabric reachability and peer session
state are diagnostic inputs, not the authority for whether this node may
advertise a healthy local API path.

Inline CA material, client certificates, tokens, or passwords are not accepted.
`health.caRef` points at Katl-managed secret or certificate material once that
materialization contract exists.

## Advertisement And Withdrawal

Advertisement is explicit and fail-closed:

```text
advertisement.enabled
  false keeps the route withdrawn even when BIRD and health are otherwise ready

advertisement.startWithdrawn
  must be true

advertisement.advertiseAfterHealthy
  must be true

advertisement.withdrawOnFailure
  must be true
```

The app starts with the API VIP route withheld from all BGP export filters. When
the VIP interface exists, generic BIRD is ready, peers are configured, and the
local `/readyz` health gate satisfies `successThreshold`, the app enables export
of exactly `endpoint.vip` to peers whose `allowedExportPrefixes[]` include it.

If health fails for `failureThreshold`, BIRD reload fails, the selected
generation is deactivated, or the app is stopped, the app withdraws the route.
If withdrawal cannot be proven, status must report a recovery-required state and
the next boot must prefer the last selected safe generation.

## Anycast Stance

v0.1 uses anycast semantics across healthy selected control-plane nodes. Every
selected healthy control-plane node may advertise the same `/32` or `/128`.
Fabric policy chooses the path using ordinary BGP attributes and ECMP support.

Active/passive leader election is not part of v0.1. The app does not use a
Kubernetes Lease, etcd lock, or cluster API object to decide which node
advertises. A single selected control-plane node behaves as active-only.

Peer-level `localPreference`, `med`, and `communities[]` are allowed as bounded
fabric policy hints, but they do not turn the app into a fabric-wide route
orchestrator.

## Route Policy

Import and export are deny by default.

Export rules:

```text
only endpoint.vip may be exported in v0.1
each peer must explicitly allow endpoint.vip
optional communities, local preference, and MED are bounded scalar fields
no Cilium service, pod, workload, or user prefixes are exported by this app
```

Import rules:

```text
fabric and dev-host imports default to reject
endpoint.vip must never be accepted from a peer
Cilium-originated API VIP routes cannot satisfy this pre-Cilium helper
```

BGP-to-OSPF translation, route reflection, local Cilium peering, service VIP
exports, and arbitrary route-table ownership are follow-up app contracts. They
must not be smuggled into this app through raw BIRD snippets.

## BGP Authentication And Secret Refs

Peer `authRef` is the only BGP authentication input. Inline BGP passwords are
rejected in user config, generated confext, fixtures, logs, and status.

`authRef` is reserved for a future Katl secret materialization contract. Until
that contract exists, implementation may either reject non-empty `authRef` with
a stable validation error or accept it only in non-runnable fixture manifests.
Runnable generated BIRD config must not contain secret material from an
undefined source.

Status may report `authConfigured: true` for a peer. It must not reveal the
secret reference target, secret value, or derived password.

## Status

The live status path is:

```text
/run/katl/apps/bgp-api-vip/status.json
```

The durable operation snapshot path follows the node app contract:

```text
/var/lib/katl/operations/<operation-id>/apps/bgp-api-vip/status.json
```

Status includes the common node app fields plus:

```text
endpointHost
endpointPort
vipPrefix
addressFamily
vipInterfaceName
vipInterfaceKind
vipInterfaceReady
nodeRoleSelected
advertiseOnRoles[]
healthState
healthTarget
lastHealthTransition
advertisementState
withdrawn
withdrawReason
lastAdvertisementTransition
birdCapabilityVersion
birdReadinessState
peerSummary[]
redactionVersion
routePolicyDigest
configDigest
loadedConfigDigest
selectedGeneration
appPayloadVersion
failureReason
recoveryRequired
```

`peerSummary[]` is bounded: peer name, peer kind, peer address family, session
state, administrative state, observed local session address, last transition,
`authConfigured`, and redacted failure category. It must not include BGP
passwords, raw full BIRD config, or unbounded route dumps.

Debug bundles may include selected service logs, `birdc` summaries, generated
config digests, and route tables with declared redaction. Routine status must
remain bounded JSON.

## Live Versus Next-Boot Behavior

Generation selection is the default apply model. Changes to VIP, interface
kind, interface name, endpoint host, local ASN, peer list, peer auth, source
interface, source address, or route policy are next-boot changes unless an
explicit live operation is implemented and selected.

Health-driven advertise and withdraw transitions are live behavior inside the
selected generation. They do not change the selected generation.

Future live apply requires an operation contract that proves:

```text
schema and BIRD parse validation completed before reload
the new config preserves operator access or intentionally starts withdrawn
the old route is not withdrawn until the new safe state is known, when changing
  endpoint identity
failure rolls back to the previous loaded config when BIRD supports it
status snapshots record preflight, activation, rollback, and recovery-required
  states
```

Until those tests exist, `katlc` must stage config for next boot and refuse
user-requested live mutation of this domain.

## Verification Expectations

Implementation follow-up work must add:

```text
Go schema normalization and validation tests for every rejected field category
golden generated confext tests for IPv4 /32 dummy VIP and IPv6 /128 loopback VIP
negative tests for wider prefixes, worker advertisement, inline auth, bad peer
  addresses, VIP-as-source, and open import/export policy
BIRD parse validation for generated config
systemd-analyze verify for app units and generated drop-ins where practical
networkd syntax validation for generated VIP interface files where practical
VM tests proving pre-Cilium API reachability, BGP export, health withdrawal,
  status output, and retained debug artifacts on failure
```

The VM proof must use `scripts/vmtest-run` and explicit timeouts on capable
hosts. Skipped VM gates must record the host capability gap and exact command.

## Non-Goals

The BGP API VIP extension does not:

```text
initialize, join, upgrade, or mutate Kubernetes
install or manage Cilium, CoreDNS, Envoy Gateway, Flux, Helm, or workloads
provide a generic VIP, load balancer, ingress, or DNS lifecycle manager
support worker-node API advertisement in v0.1
provide active/passive leader election
own fabric-wide routing policy
accept arbitrary BIRD, FRR, systemd, networkd, or confext passthrough
export Cilium service, pod, workload, or post-Cilium API routes
make generic BIRD part of the KatlOS base runtime
define secret materialization for BGP authentication by itself
```

Users who need custom routing daemon ownership may provide a future app sysext,
but it must implement the node app contract, declare its own typed schema and
status, and pass its own tests before Katl manages it.
