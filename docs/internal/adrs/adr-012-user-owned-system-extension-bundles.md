# ADR-012: User-owned system extensions are opaque generation inputs

Status: accepted.

Date: 2026-07-25.

## Context

KatlOS has an immutable, versioned runtime and already uses `systemd-sysext`
and `systemd-confext` internally. Kubernetes is delivered as a sysext selected
by Katl, while Katl-owned configuration is rendered into a generated confext.
Operators also need software which Katl does not ship or understand, including
routing daemons, hardware agents, storage helpers and site-specific services.

Requiring every such application to become a Katl-owned capability would make
Katl responsible for its schema, validation, status and lifecycle. It would
also require a Katl release before an operator could extend the host for a
local use case. Accepting packages into the mutable runtime would undermine the
immutable-host model.

The routed Kubernetes API endpoint exposes this boundary. Katl can provide a
simple BGP configuration for common networks, but a topology may instead have:

```text
one point-to-point /31 per node
a separate dummy interface carrying the node's router identity
OSPF towards the physical router
BGP from Cilium into a node-local routing daemon
site-specific filters, tables, communities and convergence policy
```

Katl should not grow an abstraction for that routing policy. It should own the
API VIP dummy interface and add or remove the exact VIP address according to
local kube-apiserver health. The operator should be able to supply any routing
system which observes that address.

Earlier node-application designs require a user-provided extension to declare
Katl-known capabilities, configuration schema IDs, Katl-scoped units and
application status. Those requirements are appropriate for a Katl-supported
application contract, but not for an expert extension mechanism whose purpose
is to support software Katl does not know about.

ADR-011 established native `/etc` files as the operator configuration
boundary. ADR-001 established generated confext as Katl's internal mechanism.
This decision composes opaque system extension payloads with those existing
generation and configuration contracts.

## Decision

ClusterConfig supports user-owned system extension entries under
`systemExtensions`. Each entry selects one immutable extension bundle and may
carry configuration and systemd activation requirements for that bundle.

Katl treats the entry as an opaque, trusted host extension. It understands:

```text
artifact transport and integrity
systemd sysext and confext compatibility
safe /usr, /opt and /etc ownership
generation selection and rollback
systemd unit activation and boot-health participation
```

Katl does not understand the application contained in the extension. In
particular, it does not infer application identity from the entry name,
executable names, unit names, configuration paths or runtime behavior.

The sysext payload and its selected configuration are one generation-scoped
desired-state unit. They are installed, activated, updated, removed and rolled
back together.

An extension bundle contains:

```text
one or more sysext images
zero or more confext images
descriptor metadata and calculated integrity information
```

At least one sysext is required in the first implementation. Pure native host
configuration continues to use `hostConfiguration`.

A bundled confext carries immutable `/etc` content coupled to that extension
build. Operator configuration remains a separate, higher-precedence generated
configuration layer so users do not rebuild an image to change site or
node-specific values.

## User-Facing Configuration

The source ClusterConfig shape is:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig

spec:
  defaults:
    systemExtensions:
      - name: routing
        state: present
        bundle: ghcr.io/example/katlos-routing:v1
        configuration:
          files:
            - path: /etc/routing/routing.conf
              source: ./routing/routing.conf
        units:
          - name: routing.service
            enable: true
            requiredForBootHealth: true
            dropIns:
              - name: 10-site.conf
                content: |
                  [Service]
                  RestartSec=2s
```

`systemExtensions` is a typed list. Every entry requires a unique
operator-selected `name`; `routing` has no semantic meaning to Katl.

`systemExtensions` may appear under `spec.defaults` or a concrete node.
Entries merge by `name`:

```text
a node entry with a new name is added
a node entry with an inherited name replaces the complete inherited entry
state: absent removes an inherited entry
omitting a previously selected non-inherited entry removes it from desired state
```

`state` defaults to `present`. An absent entry may not contain a bundle,
configuration or units.

The initial `bundle` field accepts only an OCI reference:

```yaml
bundle: ghcr.io/example/katlos-routing:v1
```

ClusterConfig does not accept a loose sysext path, local bundle directory or
catalog name in the first implementation. Extension authors may build and
validate payloads locally, but they publish the resulting bundle to an OCI
registry before selecting it.

The reference has the same shape and resolution semantics as a Kubernetes
payload bundle:

```text
REGISTRY/REPOSITORY:TAG[@sha256:<OCI-manifest-digest>]
```

`katlctl` resolves a tag-only reference once, verifies and downloads the
manifest and descriptors, and records the OCI manifest digest, Katl custom
bundle manifest digest and every payload digest. It embeds or vendors the exact
bytes in the self-contained Katl config bundle. Installers and node agents do
not independently resolve mutable tags. Registry authentication follows the
Kubernetes bundle acquisition policy; credentials are never embedded in
ClusterConfig or delivered to nodes.

Configuration `source` paths remain relative to the ClusterConfig source
directory. The OCI-only restriction applies to extension payload acquisition,
not to authoring the native `/etc` files which accompany a selected bundle.

## Extension Bundle Format

System extension delivery converges on the bundle contract defined by
`docs/internal/kubernetes-sysext-delivery.md`: the same custom-manifest
envelope, descriptor schema, canonical encoding, resolver, verification order,
content-addressed cache, retention and generation staging machinery. It is not
a second artifact system.

The three identities have the same meaning:

```text
OCI manifest digest
  distribution reference resolved from the operator's tag or digest

Katl custom bundle manifest digest
  canonical config-object bytes describing payloads and compatibility

sysext or confext payload digest
  activation identity recorded for each selected generation extension
```

The system extension custom manifest specializes the common bundle envelope:

```text
apiVersion: payload.katl.dev/v1alpha1
kind: SystemExtensionBundle
name
artifactKind: katl.system-extension.v1
artifactVersion
payloadVersion
architecture
payloads[]
  role: systemd-sysext | systemd-confext
  mediaType
  digest
  sizeBytes
  fileName
  annotations
metadata[]
  role
  mediaType
  digest
  sizeBytes
  fileName
  annotations
supportedRuntimeInterfaces[]
createdAt
signatures[]
```

It reuses the Kubernetes delivery media types for identical payloads:

```text
systemd-sysext
  application/vnd.katl.sysext.raw.v1

systemd-confext
  application/vnd.katl.confext.raw.v1

package provenance, SBOM and signatures
  the shared Katl metadata media types
```

The OCI representation follows the same layout:

```text
artifactType: application/vnd.katl.system-extension.bundle.v1
config.mediaType: application/vnd.katl.system-extension.bundle.v1+json
config.digest: sha256:<Katl-custom-bundle-manifest-digest>
layers[]: payload and metadata descriptors listed by the custom manifest
```

Custom manifest JSON uses the same canonical encoding and digest rules as the
Kubernetes bundle. Every OCI layer must match a custom-manifest descriptor by
role, media type, digest and size. Payload names are native systemd extension
image names, must be unique across the selected generation, and must match
their extension-release metadata.

Acquisition follows the Kubernetes sequence:

```text
resolve OCI ref to one OCI manifest
fetch the Katl custom manifest through the config descriptor
validate its schema, architecture and runtime compatibility
fetch every declared payload and metadata layer
verify every digest and size
commit an immutable content-addressed cache entry
select cached sysext and confext payloads only through generation metadata
```

The implementation should factor the existing Kubernetes OCI resolver,
descriptor verifier, cache transaction and retention logic into common bundle
machinery. System extensions add no Kubernetes version, kubeadm API or skew
fields.

The system extension manifest remains mechanical. It does not declare Katl
capability names, application schemas, owned routing protocols, app-specific
operations or app-specific status.

## Sysext And Confext Contents

Sysext payloads use the native systemd extension layout:

```text
/usr
/opt
/usr/lib/extension-release.d/extension-release.<image>
```

They may contain executables, libraries, udev data, systemd units and read-only
vendor defaults. Configuration which the application can naturally read from
`/usr/lib` should remain there.

Bundled confexts use the native systemd confext layout and may contain fixed
`/etc` content which is inseparable from that bundle build, including:

```text
base service drop-ins
fixed activation wiring
baseline application configuration
an application's required /etc directory structure
```

A confext must not be used merely because an application has configuration.
Site policy, node identity, addresses and other user intent belong in the
ClusterConfig entry's `configuration.files`.

System extension bundles are not a secret transport. Sysext, confext and
operator configuration bytes may be retained in compiled bundles, generation
state, rollback state and diagnostics. A later secret-reference contract must
handle secrets without embedding them in these payloads.

## Configuration Ownership And Layering

`systemExtensions[].configuration.files` uses the same authoring and file
safety rules as ADR-011:

```text
exactly one of inline content or a source-relative file
regular files below /etc
root ownership and bounded non-executable modes
size limits and path normalization
no traversal, devices, sockets, links or mutable runtime state
```

Keeping configuration inside the extension entry prevents these unsupported
states:

```text
selected software with missing desired configuration
orphaned configuration after extension removal
payload and configuration rollback to different generations
an extension update without revalidating its effective configuration
```

The effective layering order is:

```text
base KatlOS /etc
bundled extension confexts in deterministic selected-extension order
one generated top layer containing extension configuration, ordinary native
  host configuration and Katl-owned output
```

An operator file may intentionally replace the same path supplied by its
bundle's confext. The plan identifies that replacement.

Katl renders and inspects the complete effective tree before selecting a
generation. A selected extension may not replace Katl-owned identity,
management, boot, Kubernetes, generation, installer or other release-critical
paths. This is a host-integrity boundary, not application knowledge.

Bundle payload names establish stable native systemd extension ordering.
Collisions between non-Katl bundled confexts are reported in the plan with the
effective winner. Katl does not reject them merely because it suspects the
applications are related. Operator-authored files in the generated top layer
must still have one unambiguous owner because Katl cannot materialize two
desired byte strings at the same path.

## Unit Activation And Drop-Ins

An extension may ship systemd units without asking Katl to understand them.
The optional `units` list gives Katl bounded activation intent and a native
drop-in passthrough:

```yaml
units:
  - name: bird.service
    enable: true
    requiredForBootHealth: true
    dropIns:
      - name: 10-site.conf
        content: |
          [Service]
          Restart=always
          RestartSec=2s
```

Every unit entry requires one unique, valid, non-Katl-protected systemd unit
`name`. `enable: true` creates generation-scoped native systemd enablement for
that unit. An extension may instead carry fixed activation wiring in a bundled
confext.

Every drop-in entry requires a unique `name` which is a safe basename ending in
`.conf`. Katl materializes its content at:

```text
/etc/systemd/system/<unit>.d/<drop-in>
```

A drop-in accepts either inline `content` or a ClusterConfig-relative `source`
with the same bounds as other native configuration files. Katl does not
translate systemd directives or provide typed fields for capabilities,
environment, restart policy, ordering, sandboxing or command arguments. It
passes the native drop-in through and runs `systemd-analyze verify` against the
effective unit where practical.

Drop-ins are part of the owning system extension entry. Removing or rolling
back the extension removes or rolls back its drop-ins and enablement in the
same generation. A generic BIRD bundle can therefore ship a conservative
`bird.service`, while an operator adds the capabilities, command-line
replacement or restart policy required by their native BIRD configuration.

Katl reloads the systemd manager after the candidate sysext and confext set is
active and before it asks extension-provided units to start. Extensions must
not depend on their units being visible during earlier boot phases.

`requiredForBootHealth: true` makes that unit a direct candidate-generation
health requirement. Katl verifies the unit exists after the selected sysext
and confext set is assembled and reports its native systemd failure. It does
not infer that a running service has established routes, connected hardware or
completed another application-specific task.

Application preflight remains native systemd policy. A bundle may ship
`ExecStartPre=` in its service, or an operator may add it through a drop-in. A
bundle may instead ship a oneshot unit and make the service depend on it with
normal `Requires=` and `After=` relationships. If that policy fails the
declared service, `requiredForBootHealth` prevents candidate-generation
promotion and Katl reports the native unit failure.

Dependency ordering, restart behavior, capabilities, namespaces, preflight and
application-specific readiness remain native systemd policy in the supplied
units and drop-ins. Katl does not add a second service orchestration or
application-validation language.

## Validation Boundary

Katl validates only generic extension mechanics:

```text
recognized bundle and descriptor formats
successfully resolved OCI manifest and content descriptors
payload digests and sizes
systemd sysext and confext release metadata
unique activation names which match their release metadata
target architecture and KatlOS runtime compatibility
permitted filesystem roots
absence of collisions with protected Katl paths
deterministic effective /etc construction
declared unit presence and valid native drop-in paths
effective systemd unit verification where practical
declared unit boot-health results when requested
```

Katl does not validate:

```text
which application a payload contains
application configuration syntax itself
ports, sockets, protocols, peers, tables, routes or addresses
whether two selected extensions provide related software
whether a user extension conflicts semantically with a Katl feature
whether two daemons will bind the same port or originate the same route
```

Selecting a custom extension is equivalent to trusting native host software.
It may run with the privileges declared by its own systemd units and may break
boot or workloads. Katl provides bounded staging, status and rollback; it does
not sandbox the extension into safety or claim support for its behavior.

## Routed API VIP Boundary

The public endpoint shape has three structural states:

```text
no advertisement
  endpoint identity and reachability are externally owned

advertisement.vip only
  Katl owns the health-gated local VIP; route propagation is user-owned

advertisement.vip with bgp
  Katl owns the VIP and provides its simple self-contained BGP implementation
```

For the user-routed form:

```yaml
spec:
  controlPlaneEndpoint:
    host: api.home.arpa
    advertisement:
      vip: 10.40.0.10
```

Katl creates and reserves the stable `katl-api` dummy interface. The interface
remains present and up. The endpoint controller adds `10.40.0.10/32` only
while the local kube-apiserver health gate is satisfied and removes only that
exact address on health failure, service stop and lifecycle withdrawal.

`katl-api` becomes a documented native integration point. Katl exposes its
name and current address ownership in plan and status. User routing software
may observe it through a direct, device or kernel protocol.

Katl's simple BGP implementation is not an implicit selection of a system
extension named `bird`. It remains an internal endpoint implementation.
Likewise, Katl never searches `systemExtensions` for BIRD or another routing
daemon.

The internal endpoint implementation must use Katl-private executable,
configuration, unit and runtime paths so a user extension can coexist
mechanically:

```text
/usr/lib/katl/endpoint-routing/
/etc/katl/endpoint-routing/
/run/katl/endpoint-routing/
katl-endpoint-routing.service
```

It must not claim generic paths such as `/usr/bin/bird`, `/etc/bird.conf` or
`bird.service`. An operator may deliberately configure both the simple Katl
BGP implementation and an independent routing extension. Katl does not reject
that combination. Runtime port, protocol and route interactions are the
operator's responsibility.

## BIRD User Story

As a home-lab operator, I have one point-to-point `/31` between each
Kubernetes node and my router. Every node also has a `bird0` dummy interface
with a stable `/32` router identity. BIRD uses that address as its router ID,
learns Cilium routes over a local BGP session and exports selected routes to
the physical router with OSPF.

I want Katl to own the Kubernetes API VIP and withdraw the local address when
the node's kube-apiserver is unhealthy. I do not want Katl to model or replace
my BIRD configuration.

A generic BIRD extension bundle contains a sysext with BIRD, `birdc` and
`bird.service`. It contains no API VIP policy or generated BIRD configuration.
Katl may publish this generic bundle from its in-tree extension producer, or I
may publish a compatible replacement; Katl consumes the same OCI bundle format
in either case.

I select Katl's published generic BIRD bundle and tie native configuration and
a service drop-in to it:

```yaml
apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: home

spec:
  controlPlaneEndpoint:
    host: api.home.arpa
    advertisement:
      vip: 10.40.0.10

  defaults:
    systemExtensions:
      - name: bird
        bundle: ghcr.io/katl-dev/katl/extensions/bird:v3.1.2-katl.1
        configuration:
          files:
            - path: /etc/bird.conf
              source: ./bird/bird.conf
        units:
          - name: bird.service
            enable: true
            requiredForBootHealth: true
            dropIns:
              - name: 10-site.conf
                content: |
                  [Service]
                  ExecStartPre=/usr/sbin/bird -p -c /etc/bird.conf
                  AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
                  CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW

  nodes:
    - name: cp-1
      controlPlane: true
      networkd:
        files:
          - name: 10-fabric.network
            content: |
              [Match]
              Name=bond0

              [Network]
              Address=10.254.1.1/31
              Gateway=10.254.1.0
          - name: 20-bird0.netdev
            content: |
              [NetDev]
              Name=bird0
              Kind=dummy
          - name: 20-bird0.network
            content: |
              [Match]
              Name=bird0

              [Network]
              Address=10.254.254.128/32
```

Other nodes use their own `/31` and `bird0` `/32`. The BIRD configuration can
remain shared because it derives the router ID from the interface:

```bird
router id from "bird0";

protocol device {
}

protocol direct katl_api {
    ipv4;
    interface "katl-api";
}

protocol bgp cilium {
    local port 61790 as 64513;
    neighbor 127.0.0.1 as 64513 internal;
    passive;
    multihop;

    ipv4 {
        import all;
        export none;
    };
}

protocol ospf fabric {
    ipv4 {
        export filter {
            if net = 10.40.0.10/32 && source = RTS_DEVICE then accept;
            if proto = "cilium" then accept;
            reject;
        };
        import none;
    };

    area 0 {
        interface "bond0" {
            type ptp;
        };
    };
}
```

The drop-in is native systemd configuration. Katl derives
`/etc/systemd/system/bird.service.d/10-site.conf`, verifies the effective unit
mechanically and does not interpret why this topology needs that preflight
command or those capabilities. If BIRD rejects `/etc/bird.conf`,
`ExecStartPre=` fails `bird.service`; its required boot-health state prevents
the candidate generation from being promoted and its journal provides the
diagnostic. I can similarly replace `ExecStart`, add an environment file, or
change restart and ordering policy using normal systemd drop-in syntax.

The resulting lifecycle is:

```text
before kube-apiserver readiness
  katl-api exists without the API VIP
  BIRD remains running and exports no direct API VIP route

after local readiness succeeds
  Katl adds 10.40.0.10/32 to katl-api
  BIRD's direct protocol observes the address
  the operator's filter exports the route through OSPF

after local readiness fails
  Katl removes only 10.40.0.10/32
  BIRD remains running
  the direct route disappears and BIRD withdraws it according to native policy

after config or extension removal
  the next selected generation omits the BIRD sysext and its /etc files together
```

Katl proves only local VIP ownership, BIRD unit health when requested and
remote stable-endpoint reachability through the normal bootstrap or lifecycle
journey. It does not parse BIRD protocols or claim that a specific route was
advertised.

## Katl-Provided Extension Producer

Definitions for extensions published by the Katl project live in this
repository under:

```text
extensions/<name>/
```

Each directory owns the reproducible build recipe, locked package or source
inputs, supplied native systemd units and defaults, bundle metadata inputs and
focused content tests for one extension. `name` is a safe lowercase OCI
repository path segment. In-tree ownership lets a change to the KatlOS runtime
interface, common bundle tooling or an extension recipe be reviewed and tested
together.

The shared `.github/workflows/system-extensions.yml` GitHub Actions workflow
builds these definitions. Pull requests build and validate affected extensions
without publishing them. Trusted CI after merge or an explicitly authorized
release dispatch:

```text
builds the sysext and any bundled confext
verifies native extension metadata, contents, units and runtime compatibility
creates the common Katl custom manifest and OCI descriptor set
records source and build-input provenance
publishes the immutable OCI artifact
verifies the registry manifest digest against the locally produced manifest
```

The workflow invokes the common payload-bundle producer and publisher factored
from Kubernetes delivery. An extension directory supplies payload build and
metadata inputs; it does not implement its own OCI layout, digest rules or
registry client.

Official artifacts use:

```text
ghcr.io/katl-dev/katl/extensions/<name>:<artifactVersion>
ghcr.io/katl-dev/katl/extensions/<name>:<artifactVersion>@sha256:<OCI-manifest-digest>
```

For example:

```text
ghcr.io/katl-dev/katl/extensions/bird:v3.1.2-katl.1
```

The readable tag is immutable. Any source, package, recipe or bundle-metadata
change produces a new `artifactVersion`; CI must not replace an existing tag
with different bytes. CI is the only publisher to the official namespace.
Developers use the same producer entrypoint locally for build and validation
but do not need official registry credentials.

Katl-provided bundles use the same `SystemExtensionBundle` manifest, media
types, digest hierarchy and consumer path as any user-published bundle. The
official namespace provides a reviewed producer and compatibility promise; it
does not create ClusterConfig shorthand, a built-in extension catalog or
application-specific behavior in Katl. The operator-selected
`systemExtensions[].name` remains independent of the OCI repository name.

## Trust And Distribution

Every selected extension is published as OCI, including extensions produced
only for one home lab. OCI distribution supplies content-addressed transport;
the Katl custom manifest binds the actual sysext, confext and metadata
descriptors. A tag remains convenient authoring input, while the compiled
bundle and generation record only resolved immutable identities.

Registry authentication follows the same policy and implementation as
Kubernetes bundle acquisition. The first implementation accepts unauthenticated
public HTTPS registries. Private-registry credentials require a separate
redaction and credential-input contract. Credentials are never embedded in
ClusterConfig or generation metadata.

Signature or provenance verification remains optional policy for operators
with a stricter threat model. Katl always verifies the resolved OCI manifest,
custom manifest and payload digests, but does not mistake digest integrity for
producer identity.

Katl should provide:

```text
katlctl system-extension inspect <oci-reference>
katlctl system-extension validate <oci-reference>
katlctl system-extension publish --ref <oci-reference> <bundle-inputs>
```

These commands report format, architecture, compatibility, contained paths,
units, protected-path conflicts and calculated digests. `validate` is
mechanical bundle and compatibility validation; it does not execute application
configuration checks. The publishing command accepts local producer outputs
and writes the same OCI custom-manifest and layer shape used by Kubernetes
delivery; local files never become a ClusterConfig acquisition source.

Katl does not need to build arbitrary user software. Documentation may provide
the in-tree producer as an example, while users remain free to use any
compatible builder and publish to any public OCI repository they control.

## Apply, Upgrade And Rollback

The first implementation treats every system extension payload or
configuration change as next-boot-only.

Install and config apply:

```text
resolve and validate all bundle inputs
render the effective /etc tree
stage sysexts, confexts and operator configuration in one candidate generation
activate them together on the trial boot
start declared units and evaluate requested boot-health participation
promote only after the normal KatlOS health contract succeeds
```

Removal selects a generation without the extension payloads, configuration and
enablement. It does not delete files from the currently booted generation or
stop an active service during normal apply.

Rollback restores the previous root, sysext set, confext set, operator
configuration and enablement together. Katl does not claim to reverse external
state already produced by the extension.

A KatlOS host upgrade validates every preserved user extension against the
target runtime and protected-path policy before selecting the candidate. An
incompatible extension blocks the upgrade with the extension instance,
incompatibility and required rebuild or removal. Katl must not silently omit
it.

Live sysext refresh, service replacement and extension-specific rollback are
deferred. They require a separate operation contract because generic rollback
cannot undo arbitrary process or external state.

## Status And Diagnostics

Plans and node status report the generic facts Katl owns:

```text
extension instance name and desired state
submitted OCI reference and resolved OCI and custom manifest digests
selected sysext and confext payload digests
compatibility result
desired and observed generation
bundle staging plus sysext and confext activation state
configured /etc files, drop-ins, effective ownership and content digests
declared unit enablement and boot-health participation
native unit load, active, sub and result states
last native unit state change and bounded failure diagnostic
required reboot or rollback state
```

`katlctl cluster status` includes one concise row for every desired or observed
extension on every node. `katlctl system-extension status --node <node>
[<name>]` provides the detailed facts above and supports the normal
machine-readable output. Local, remote and automation callers consume the same
typed agent status; the CLI does not scrape ad hoc SSH output. An unreachable
node is reported as unavailable with its last durable desired and generation
state rather than as healthy.

Status is derived from desired ClusterConfig, immutable generation metadata,
`systemd-sysext`, `systemd-confext` and the systemd manager. It does not execute
an extension-provided status command.

An extension with no declared units can be reported as selected and active, but
Katl reports application health as unknown rather than inferring it from
payload activation. A declared unit which is not required for boot health is
still observable. A required unit affects candidate promotion as well as
status. Bounded journal output is included for a failed declared unit or
through explicit diagnostics, not continuously copied into durable status.

Routine status does not invent an application-specific health schema.
Extension-provided application diagnostics remain outside Katl unless a later
generic diagnostic attachment contract is accepted.

## Relationship To Earlier Extension Contracts

This ADR amends the user-provided extension sections of:

```text
docs/internal/node-app-sysext-contract.md
docs/internal/node-extension-bundle-format.md
```

Katl-supported node applications may continue to use those richer
application-specific contracts. User-owned `systemExtensions` do not need
Katl capability names, config schema IDs, app-specific operation kinds,
Katl-scoped status schemas or Katl application support.

Their delivery envelope nevertheless converges with Kubernetes payload
delivery: the same OCI config-object pattern, descriptor schema, digest
hierarchy, resolver, cache transaction, retention rules and generation
activation records apply to both. Artifact-specific kinds, config media types
and descriptor roles identify the different payload contracts; identical
sysext, confext and metadata content uses common media types.

The accepted generic BIRD extension contract describes a Katl-supported BIRD
capability and explicitly rejects raw operator configuration. It is not the
contract for the opaque operator-configured BIRD journey in this ADR. Publishing
a generic BIRD bundle from the in-tree producer makes Katl responsible for its
build, runtime compatibility and artifact integrity, not for the semantics of
the operator's BIRD configuration. Katl's simple routed endpoint remains
self-contained; an operator-configured BIRD sysext uses this ADR and native
BIRD configuration.

## Non-Goals

This decision does not:

```text
turn Katl into a package manager or application marketplace
resolve dependencies between independently produced extensions
select loose raw images, local bundle directories or catalogs from ClusterConfig
make custom extension behavior part of the Katl support contract
define BIRD, FRR, OSPF, BGP, storage or hardware-agent schemas
define a Katl-specific shell or lifecycle-hook language outside native unit drop-ins
make extension payloads a secret store
support live extension replacement in the first implementation
let a user extension replace Katl release-critical paths
infer semantic conflicts between independently selected software
```

## Testing Contract

Implementation requires unit and golden coverage for:

```text
typed-list validation and unique extension names
default and per-node merge by name, replacement and state: absent
rejection of loose file, local bundle and catalog source shapes
OCI resolution to exact embedded content
shared Kubernetes/system-extension manifest and descriptor verification
sysext and confext compatibility validation
in-tree extension recipe and producer input validation
official OCI namespace, immutable tag and post-publication digest verification
non-publishing pull-request workflow behavior
operator configuration override of bundled confext content
protected Katl path rejection without application-specific inspection
deterministic non-Katl confext collision reporting
unit presence and drop-in path/content validation
effective systemd unit verification
desired and observed extension, digest, generation and activation status
declared unit state, bounded failure diagnostics and boot-health failure
unreachable-node status without a false healthy result
generation metadata, removal and rollback of payload plus configuration
target-runtime incompatibility during host upgrade
```

The VM journey must use public `katlctl` interfaces to:

```text
build the in-tree generic BIRD definition through the shared producer entrypoint
publish the fixture in the common OCI bundle envelope
select the bundle only through its OCI reference
install three routed control-plane nodes with per-node /31 links
configure distinct bird0 router-identity /32 addresses
select the BIRD extension, native /etc/bird.conf and bird.service drop-in
bootstrap through a Katl-owned health-gated API VIP
prove the VIP route appears after local API readiness
prove local API failure removes only the VIP while BIRD remains active
prove API recovery restores reachability
apply the same configuration again without mutation
stage invalid BIRD configuration and observe the failed unit and bounded
  diagnostic through system extension status before safe candidate rollback
remove the extension and prove payload and configuration disappear together
exercise rollback to the previous known-good extension generation
```

The route proof may use BIRD on a VM fabric router, but success is judged from
the operator surface: CA-verified API reachability, useful progress and errors,
correct systemd state, exact VIP ownership and safe recovery.

## Consequences

Operators can extend KatlOS without waiting for Katl to support their
application. Katl retains its immutable runtime, generation, health and
rollback guarantees while being explicit that custom software is trusted and
operator-owned.

Configuration stays tied to the extension which consumes it, and a bundled
confext can make a third-party extension self-contained without forcing
site-specific values into an image.

Katl accepts build, compatibility-test and CI publication responsibility for
the finite set of project-provided extension recipes in `extensions/`. That
does not make arbitrary third-party software part of Katl's release or support
surface.

Katl must generalize the Kubernetes OCI bundle resolver, verifier, cache and
staging code for system extension payloads, then add generic configuration,
unit drop-in and activation policy. It must protect release-critical filesystem
ownership without using application-specific allowlists.

The routed API VIP becomes a reusable protocol-independent primitive. The
simple managed BGP path remains direct for common users, while complex routing
topologies compose through native Linux and systemd interfaces.
