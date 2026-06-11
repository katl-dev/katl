# Platform API Endpoint Helper Input Schema

Status: current decision for the opt-in platform API endpoint helper schema.

This document defines the typed input shape for the platform API endpoint
routing capability described in
`docs/internal/platform-api-endpoint-routing-capability.md`. The schema is for a
day-2 opt-in node application capability, not the initial kubeadm-ready node
configuration API.

The first concrete helper target is a host-owned API VIP on a dummy or loopback
interface, advertised through BIRD or an equivalent routing process, with a
health-gated `/readyz` probe before advertisement.

## Decision

Katl models the platform API endpoint helper as typed, bounded node application
configuration. It must not expose arbitrary BIRD configuration, arbitrary
systemd units, arbitrary confext paths, package installation, kubeadm or kubectl
actions, or Cilium lifecycle management.

Users choose one endpoint mode per node:

```text
disabled
  helper is not configured and renders no helper artifacts

external
  an external router, load balancer, or already-routable endpoint owns API
  reachability; Katl may wait for reachability but does not advertise routes

hostAdvertised
  this node owns a local API VIP or address and advertises it through a host
  routing process before Cilium depends on the API endpoint
```

Only `hostAdvertised` renders helper artifacts. `external` exists so the
cluster plan can record endpoint provenance and avoid treating a reachable load
balancer as a Katl-owned route.

## Input Shape

The initial schema should fit under a future node application or capability
block, with a shape like:

```yaml
apiVersion: katl.dev/v1alpha1
kind: NodeApplicationConfiguration
spec:
  platformAPIEndpoint:
    mode: hostAdvertised
    endpoint:
      host: api.home.example
      port: 6443
      vip: 10.40.0.10/32
      addressFamily: ipv4
      provenance: platform-host
      tlsServerName: api.home.example
    hostInterface:
      kind: dummy
      name: katl-api0
      addresses:
      - 10.40.0.10/32
      mtu: 1500
    routing:
      daemon: bird
      routerID: 10.0.0.11
      platformPrefixes:
      - 10.40.0.10/32
      exportPolicy:
        default: deny
        allowPlatformAPI: true
        allowCiliumRoutes: true
        communities:
        - "64512:100"
        localPreference: 100
        med: 0
      importPolicy:
        default: deny
        allowCiliumServiceRoutes: true
      protocolBoundary:
        kind: bgp
    fabricPeers:
    - name: router-a
      address: 10.0.0.1
      asn: 64500
      localASN: 64512
      authRef: secret/bgp-router-a
      holdTime: 90s
      keepaliveTime: 30s
      allowedExportPrefixes:
      - 10.40.0.10/32
    ciliumPeer:
      enabled: true
      listenAddress: 127.0.0.1
      listenPort: 1179
      ciliumAddress: 127.0.0.1
      ciliumASN: 64513
      localASN: 64512
      allowedImportPrefixes:
      - 10.96.0.0/12
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

The final location of this block depends on the app sysext contract design. The
field names above are the schema decision for the helper itself.

## Endpoint Fields

`endpoint.host` is the stable API identity used by kubeadm
`controlPlaneEndpoint`, kubeconfig output, and Cilium `k8sServiceHost`.

`endpoint.port` defaults to `6443` when omitted.

`endpoint.vip` is required for `hostAdvertised`. It must be a single-host IPv4
`/32` or IPv6 `/128` prefix for the first implementation. Wider prefixes are
rejected until route ownership and filtering have stronger tests.

`endpoint.addressFamily` is `ipv4` or `ipv6` and must match `endpoint.vip`.

`endpoint.provenance` distinguishes endpoint ownership:

```text
external
  endpoint is owned by external infrastructure; valid with mode external

platform-host
  endpoint is owned by this host helper; valid with mode hostAdvertised

cilium
  rejected for pre-Cilium helper use; post-Cilium provenance belongs to a
  separate endpoint provenance model
```

`endpoint.tlsServerName` defaults to `endpoint.host` and is used by the health
probe when it validates apiserver TLS through the advertised path.

`external` mode may set `endpoint.host`, `endpoint.port`, and
`endpoint.provenance: external`, but must not set `endpoint.vip` or routing
fields.

## Host Interface Fields

`hostInterface` is required for `hostAdvertised`.

Supported interface kinds:

```text
dummy
  preferred for a host-owned API VIP

loopback
  allowed when the network fabric or routing daemon is configured to advertise
  a loopback-owned address
```

`hostInterface.name` must be a safe Linux interface name, length-bounded, unique
within the node plan, and must not collide with known physical or user-owned
networkd interfaces.

`hostInterface.addresses[]` must contain exactly `endpoint.vip` for the first
implementation. Split local and advertised addresses can be reconsidered later
if a real routing use case needs them.

`hostInterface.mtu` is optional. It must be within the Linux interface MTU range
and should usually be omitted for dummy or loopback helpers.

## Routing Fields

`routing.daemon` is required for `hostAdvertised`. The initial accepted value is:

```text
bird
```

Other daemons may be added only after their app sysext contract, rendered
artifacts, status model, and VM tests exist.

`routing.routerID` is required for BIRD configurations and must be an IPv4
address. IPv6-only routing still needs an explicit router ID unless the daemon
design proves a safe alternative.

`routing.platformPrefixes[]` must include `endpoint.vip` and initially may not
contain other prefixes.

The schema does not allow arbitrary daemon snippets. Instead it exposes bounded
policy fields:

```text
routing.exportPolicy.default
  must be deny

routing.exportPolicy.allowPlatformAPI
  explicitly allows endpoint.vip export

routing.exportPolicy.allowCiliumRoutes
  allows post-Cilium imported routes to be exported to fabric peers

routing.exportPolicy.communities[]
  optional BGP communities attached to exported routes

routing.importPolicy.default
  must be deny

routing.importPolicy.allowCiliumServiceRoutes
  allows explicitly listed Cilium routes to enter the routing process

routing.protocolBoundary.kind
  bgp or bgpToOspf, when the selected helper supports it
```

`bgpToOspf` is a design allowance, not a first implementation requirement. It
must fail validation unless the selected helper package declares support.

## Fabric Peer Fields

Each `fabricPeers[]` entry requires:

```text
name
address
asn
localASN
allowedExportPrefixes[]
```

Optional fields:

```text
authRef
holdTime
keepaliveTime
communities[]
localPreference
med
```

`authRef` is a reference to a future secret source. Inline BGP passwords are not
allowed in committed node config, generated confext, inventory, or docs
fixtures.

Peer names must be safe single labels and unique. Peer addresses must be IP
addresses, not DNS names, so route establishment does not depend on DNS during
early bootstrap. `allowedExportPrefixes[]` must be a subset of
`routing.platformPrefixes[]` plus allowed Cilium import prefixes.

Peer status must be redacted. A status record may say that authentication is
configured, but must not expose secret material or inline credential content.

## Local Cilium Peer Fields

`ciliumPeer.enabled` controls whether the local routing process accepts routes
from Cilium after Cilium is installed.

When enabled, the schema requires:

```text
listenAddress
listenPort
ciliumAddress
ciliumASN
localASN
allowedImportPrefixes[]
```

`listenAddress` and `ciliumAddress` must be local addresses, usually loopback.
Remote fabric addresses are rejected for the local Cilium peer because the
preferred topology is Cilium-to-local-BIRD, not Cilium-to-router.

The Cilium peer import policy must be deny-by-default. At least one explicit
service or workload prefix is required before Cilium-originated routes can be
exported to fabric peers. The platform API endpoint prefix must not appear in
`allowedImportPrefixes[]` for a pre-Cilium helper.

The local Cilium peer is never required for `external` mode and cannot satisfy
pre-Cilium reachability.

## Advertisement Fields

Advertisement is explicit and starts withdrawn.

Accepted values:

```text
advertisement.enabled
  false disables route export even when local config renders

advertisement.startWithdrawn
  must be true for hostAdvertised

advertisement.advertiseAfterHealthy
  must be true for hostAdvertised

advertisement.withdrawOnFailure
  must be true for hostAdvertised
```

Advertising an apiserver route without health gating is not part of this helper
contract.

## Health Fields

Health defaults:

```text
probe: readyz
scheme: https
host: <endpoint.vip address>
port: <endpoint.port>
path: /readyz
interval: 2s
timeout: 1s
successThreshold: 2
failureThreshold: 3
```

`health.host` defaults to the address portion of `endpoint.vip`; `health.port`
defaults to `endpoint.port`; and `health.path` defaults to `/readyz`. The probe
target is local kube-apiserver `/readyz` reached through the local advertised
VIP path before that route is exported to the fabric. `health.tlsServerName`
defaults to `endpoint.tlsServerName` so the helper can probe an IP VIP while
validating the stable API identity. `health.caRef` identifies the CA material
the helper uses to validate apiserver TLS.
`health.clientCredentialRef` may be added later if unauthenticated `/readyz`
probing is not sufficient in practice. Inline client certificates, tokens, or
passwords are not allowed.

Local advertised-path health gates fabric advertisement. Remote or fabric
reachability is reported as status and diagnostics, not as the first authority
for whether the node may advertise its local API path.

## Rendered Artifacts

For `hostAdvertised`, the renderer should produce generation-scoped generated
confext content and app-sysext configuration inputs like:

```text
/etc/systemd/network/20-katl-api0.netdev
  dummy interface, when hostInterface.kind is dummy

/etc/systemd/network/20-katl-api0.network
  address assignment for hostInterface.addresses

/etc/katl/platform-api-endpoint/config.yaml
  normalized helper input consumed by the app sysext

/etc/katl/platform-api-endpoint/routing/bird.conf
  generated BIRD configuration from bounded schema fields

/etc/systemd/system/katl-platform-api-endpoint.service.d/10-config.conf
  bounded drop-in pointing the app sysext helper at the selected config path
```

The app sysext, not generated confext, owns helper executables and base units.
Generated confext supplies selected node-specific configuration only.

For `external`, the renderer may write non-secret metadata under
`/etc/katl/platform-api-endpoint/external.json` so status and diagnostics can
explain why no host advertiser is running.

For `disabled`, no helper artifacts are rendered.

Status path ownership is finalized by the app sysext contract, but the status
content must include configured endpoint, host interface, routing daemon,
health, advertisement, peer state, last transition, and failure reason.

All rendered paths are examples of the accepted ownership model. Final file
names may change during implementation, but the boundaries must not: networkd
configuration is generated confext, helper binaries and base units come from an
app sysext, and raw arbitrary user paths are not accepted.

## Validation Errors

Validation must fail before rendering when:

```text
mode is unknown
hostAdvertised omits endpoint.vip
pre-Cilium/helper provenance is cilium
external sets endpoint.vip, hostInterface, routing, fabricPeers, or ciliumPeer
endpoint host/port shape is invalid
endpoint.addressFamily does not match endpoint.vip
endpoint.vip is not a single-host /32 or /128
endpoint.vip is outside routing.platformPrefixes
hostInterface.addresses does not exactly match endpoint.vip
hostInterface.name is unsafe, reserved, or duplicated
routing.daemon is unsupported by the selected app sysext
routing.routerID is missing or invalid for BIRD
routing export or import policy defaults to open
fabric peer name or address is duplicated
fabric peer address is not an IP address
fabric peer omits ASN, local ASN, or allowed export prefixes
inline peer authentication or inline health credentials are provided
protocolBoundary kind is unsupported by the selected app sysext
advertisement starts unwithdrawn or disables withdrawOnFailure
Cilium peer uses a non-local address
Cilium import policy includes the platform API endpoint prefix
Cilium import/export is enabled without explicit allowed prefixes
any rendered output would write outside supported generated confext paths
any rendered output would install cluster add-ons or run kubeadm/kubectl/Helm
```

Validation should report stable field paths such as:

```text
platformAPIEndpoint.endpoint.vip must be a /32 or /128
platformAPIEndpoint.fabricPeers[router-a].address must be an IP address
platformAPIEndpoint.ciliumPeer.allowedImportPrefixes must not include API prefix
```

## Golden Tests

Implementation follow-up work must add deterministic golden tests for:

```text
disabled mode renders no helper artifacts
external mode renders only provenance or status metadata
minimal hostAdvertised IPv4 dummy VIP
hostAdvertised IPv6 /128 VIP
fabric peer with authRef and redacted status-safe config
two fabric peers with separate export policies
local Cilium peer with service-prefix import and correct ordering metadata
advertisement disabled renders withdrawn or no-export state
health-gated advertisement defaults
BIRD config render with deny-by-default import/export policy
systemd-networkd dummy interface render
status config render
```

Negative tests must cover every validation error category above. Generated
artifact tests should normalize file modes, paths, and content so they do not
depend on host-specific paths or local network state.

Generated systemd/networkd artifacts should be verified with
`systemd-analyze verify` or `networkd` syntax validation where practical.

VM tests must prove pre-Cilium API reachability, local BIRD export, route
withdrawal, and diagnostics with explicit skips when libvirt VM, network,
storage, or routing prerequisites are missing.

## Implementation Follow-Up

Follow-up implementation should cover Go types, normalization, validation,
renderer output, golden tests, negative tests, and syntax verification hooks for
this schema.

Another follow-up should define where `authRef`, `health.caRef`, and future
`health.clientCredentialRef` values come from, how they are materialized for app
sysext consumption, and how status remains redacted.

## Non-Goals

This schema does not install Cilium, configure Cilium Helm values, or own Cilium
CRDs. It only defines the host-side input needed to offer a pre-Cilium platform
API endpoint path and a local routing adjacency for later Cilium route export.

This schema does not expose arbitrary BIRD, FRR, or systemd syntax. Users who
need full custom routing daemon ownership can provide their own app sysext later,
but it must still implement the Katl app contract before Katl treats it as a
managed helper.
