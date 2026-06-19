# Generic BIRD Extension Contract

Status: accepted app-specific contract for the generic BIRD node extension.
Concrete packaging, renderer, operation, and VM proof remain follow-up work.

The generic BIRD extension is a reusable Katl node application sysext that
provides a routing daemon capability to higher-level Katl apps. It builds on
`docs/internal/node-app-sysext-contract.md` and
`docs/internal/node-extension-bundle-format.md`.

Generic BIRD is not part of the KatlOS base runtime. Selecting it does not by
itself configure or advertise a Kubernetes API VIP. The productized BGP API VIP
extension composes with this capability and owns VIP schema, health-gated
advertisement, kubeadm endpoint integration, and VM route proof.

## Decision

The BIRD extension is delivered as a node extension bundle with:

```text
appID: bird
artifactKind: katl.node-app-sysext.v1
payloadVersion: bird-<upstream-version>-katl.<revision>
capability: dev.katl.routing.bird
activationPhase: selected by the consuming app
```

The extension bundle carries only immutable daemon payload, helper binaries,
base units, extension metadata, and package provenance. Node-specific routing
configuration is generated confext rendered by a Katl app-specific config
domain. Operators do not hand Katl raw BIRD config, raw sysext paths, arbitrary
systemd units, package names, or arbitrary `/etc` patches.

The first supported consumer is the BGP API VIP extension. Generic BIRD may be
selected by other future routing apps only after those apps declare their own
typed schema, status fields, operation behavior, and VM tests.

Required manifest fields include:

```text
capabilities[]
  name: dev.katl.routing.bird
  version: v1alpha1
  configSchemaIDs:
    dev.katl.routing.bird.generated.v1alpha1
  operationKinds:
    bird-config-validate
    bird-config-reload, only when the consuming app enables live apply

compatibility.requiredCapabilities[]
  CAP_NET_ADMIN
  CAP_NET_BIND_SERVICE

systemd.activationPhases[]
  pre-kubeadm, when selected by the BGP API VIP extension
  post-kubeadm, for future consumers that do not gate bootstrap
  maintenance, for validation or repair operations

status.statusSchemaID
  dev.katl.routing.bird.status.v1alpha1
```

## Payload Contents

The sysext payload contains:

```text
BIRD daemon
  bird, using the package path selected by the build, for example /usr/sbin/bird

BIRD client
  birdc, using the package path selected by the build, for example /usr/sbin/birdc

Katl status helper
  small helper or script that reads the control socket and emits bounded JSON

base systemd units
  katl-app-bird.target
  katl-app-bird.service
  katl-app-bird-ready.service
  katl-app-bird-status.service or equivalent oneshot helper

extension metadata
  extension-release metadata compatible with the Katl runtime interface

read-only defaults
  empty or safe default config fragments that do not establish peers or
  advertise routes
```

The sysext must not contain kubeadm, kubectl, Helm, CNI binaries, package
managers, build tools, arbitrary operator shells, or BGP API VIP-specific
configuration. It may include only runtime libraries required by BIRD and the
declared helper.

## Systemd Units

The manifest must list every provided unit. The base unit names are:

```text
katl-app-bird.target
katl-app-bird.service
katl-app-bird-ready.service
katl-app-bird-status.service
```

`katl-app-bird.service` starts BIRD with explicit paths:

```text
config: /etc/katl/apps/bird/bird.conf
control socket: /run/katl/apps/bird/bird.ctl
pid or runtime state: /run/katl/apps/bird/
```

The service must be ordered after systemd sysext/confext activation and after
the network is configured enough for the consuming app's phase. It must not be
enabled by the sysext itself. Generated confext or the selected generation
enables the declared target only when a consuming app selects BIRD.

The base service should use systemd hardening where practical:

```text
Type=simple or notify, depending on the packaged daemon support
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/run/katl/apps/bird
RuntimeDirectory=katl/apps/bird
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
RestrictAddressFamilies=AF_UNIX AF_NETLINK AF_INET AF_INET6
```

`CAP_NET_ADMIN` is required for route-table and netlink work.
`CAP_NET_BIND_SERVICE` is required when BIRD listens on privileged BGP port 179.
`CAP_NET_RAW`, BFD, raw socket probes, or other extra capabilities are not in
the v0.1 generic surface and require a follow-up contract and tests.

## Configuration Ownership

Generated confext owns these paths:

```text
/etc/katl/apps/bird/config.yaml
/etc/katl/apps/bird/bird.conf
/etc/systemd/system/katl-app-bird.service.d/10-katl-config.conf
```

`config.yaml` is the normalized Katl input consumed by helpers and diagnostics.
`bird.conf` is generated native BIRD configuration from a bounded Katl schema.
The drop-in points the base service at the selected generated config and may
set app-specific environment only from declared fields.

The generic BIRD contract does not own dummy interfaces, loopback VIP
addresses, kubeadm `controlPlaneEndpoint`, Cilium configuration, or platform
API endpoint policy. Those are owned by the app that consumes BIRD.

Secrets are not inline. If a consuming app needs BGP authentication, it must
declare secret refs and a materialization policy before generating BIRD
password material. Status and debug output must redact whether that secret is
present without exposing the value.

## Control Socket

The only managed control socket path is:

```text
/run/katl/apps/bird/bird.ctl
```

Only root and Katl-owned helper services may access it. Operators should use
`katlctl` diagnostics or captured debug bundles for routine inspection, not the
socket as a stable API.

`birdc` is an implementation detail used by systemd reload, status helpers, and
debug collection. The stable status API is the app status JSON, not raw
`birdc show` output.

## Reload Behavior

Config validation must run before a selected generation or live operation asks
BIRD to reload. Validation includes:

```text
Katl schema validation
BIRD config parse check, for example bird -p -c /etc/katl/apps/bird/bird.conf
systemd-analyze verify for units and generated drop-ins where practical
```

The base service may provide:

```text
ExecReload=birdc -s /run/katl/apps/bird/bird.ctl configure
```

Reload is not a generic user-facing live-apply mechanism. By default, BIRD
configuration changes are next-boot generation changes. A consuming app may
enable live reload only after it proves preflight, resource locking, route
withdrawal or rollback behavior, status snapshots, and VM tests for the routes
it owns.

If reload fails, the previous daemon configuration should remain running when
BIRD supports that behavior. Status must report the rejected config digest,
failure reason, and required operator action. If the consuming app cannot prove
safe live recovery, it must fail closed and require reboot or explicit repair.

The first packaging implementation must add deterministic test fixtures for:

```text
bird -p -c generated-good.conf succeeds
bird -p -c generated-bad.conf fails with a stable diagnostic category
systemd-analyze verify katl-app-bird.service and generated drop-ins succeeds
reload operation refuses when the selected consuming app has no live-apply
  contract
```

## Status

The generic live status path is:

```text
/run/katl/apps/bird/status.json
```

The durable operation snapshot path follows the node app contract:

```text
/var/lib/katl/operations/<operation-id>/apps/bird/status.json
```

Generic BIRD status must include the common app fields plus:

```text
birdVersion
daemonActive
readinessState
configDigest
loadedConfigDigest
controlSocketPath
lastReloadTime
lastReloadResult
protocolSummary[]
peerSummary[]
routeTableSummary[]
failureReason
```

`protocolSummary[]` and `peerSummary[]` are bounded summaries: protocol name,
protocol type, administrative state, session state, last transition, and
redacted error. They must not include passwords, raw full config, or unbounded
route dumps. Debug artifacts may include selected `birdc show` output and logs
only with declared redaction.

Generic BIRD readiness means the daemon is running, the selected config parsed
and loaded, the control socket is reachable, and the declared protocols reached
the consuming app's readiness condition. It does not mean any API VIP route is
safe to advertise unless the consuming app defines that condition.

## Supported Protocol Surface

The v0.1 generic surface supports only the protocol pieces needed by bounded
Katl routing apps:

```text
device and direct protocol support needed to observe local interfaces
kernel protocol support needed to read or install selected routes
static routes generated by a consuming app
BGP IPv4 and IPv6 unicast sessions generated by a consuming app
deny-by-default import and export filters
```

Unsupported in the generic v0.1 surface:

```text
raw arbitrary BIRD config passthrough
OSPF, RIP, Babel, MRT, RPKI, BMP, BFD, flowspec, multicast, or pipe protocols
unless a follow-up app-specific contract declares and tests them
arbitrary route-table ownership
route reflector policy
global fabric policy management
Cilium lifecycle or Kubernetes object management
```

The BGP API VIP extension may use generic BIRD to render BGP sessions and
prefix filters, but it owns the API VIP prefix, health gate, advertisement
state, and withdrawal semantics.

## Bounded Config Model

Generic BIRD exposes capability metadata and a native config target, not a
free-form routing language API. A consuming app must declare:

```text
supported config schema ID
owned prefixes
owned peers
owned protocol types
owned status fields
allowed live operations
rollback or fail-closed behavior
```

Generated BIRD config must be deterministic and deny by default. Import and
export filters must be explicit. A consuming app must not write config for
prefixes, peers, protocols, route tables, or sockets it does not own.

Future passthrough is allowed only as a separate user-provided app contract
that still uses the node extension bundle format, declares its units and status
paths, and does not become Katl-managed generic BIRD.

## Package And Artifact Checks

The packaging Bead for generic BIRD must add checks that prove:

```text
the selected BIRD package or build input is recorded in package provenance
bird and birdc exist at the paths used by systemd units
bird --version matches payloadVersion
extension-release metadata matches the supported runtime interface
only declared units are present
the control socket, config, status, and runtime paths match this contract
package managers, build tools, Kubernetes CLIs, CNI tools, and BGP API
  VIP-specific config are absent
```

If the extension uses Fedora packages, the lock must record exact NEVRAs and
checksum-bearing component artifacts. If it uses a source build, the provenance
must record source revision, build input digest, and resulting binary digest.

Generated units and drop-ins should run through `systemd-analyze verify` where
practical. Generated `bird.conf` fixtures must run through BIRD parse
validation. VM fixtures must use the same node extension bundle manifest and
digest shape as published bundles.

Unsigned fixture bundles are allowed only while signing is not implemented and
must carry the explicit unsigned-fixture marker required by the node extension
bundle format. Published bundles require the project-selected signing envelope
before they are stable distribution artifacts.

## Non-Goals

Generic BIRD does not:

```text
join, initialize, upgrade, or mutate Kubernetes
configure kubeadm controlPlaneEndpoint
create dummy or loopback API VIP interfaces by itself
advertise an API VIP by itself
install or configure Cilium, CoreDNS, Envoy Gateway, Flux, Helm, or workloads
own fabric-wide routing policy
provide a general package installation surface
provide arbitrary BIRD, FRR, systemd, or confext passthrough
make BIRD part of the base KatlOS runtime
```

The BGP API VIP extension must define the productized API endpoint behavior on
top of this capability instead of duplicating generic BIRD packaging or exposing
arbitrary BIRD configuration.
