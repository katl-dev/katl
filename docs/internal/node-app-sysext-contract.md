# Node App Sysext Contract

Status: accepted contract for optional node applications. Concrete applications
remain unsupported until their app-specific bundle, configuration, operation,
status, and VM-test Beads are completed.

Katl can deliver optional node applications as systemd sysext payloads selected
by Katl generations. This contract is for host-side node applications such as a
BIRD routing helper or a BGP API VIP helper. It is not the Kubernetes payload
bundle contract, not a package-manager interface, and not a path for arbitrary
user systemd units.

The first concrete consumer is the platform API endpoint helper described in
`docs/internal/platform-api-endpoint-routing-capability.md`. That helper still
needs its own app-specific contract before it becomes user-facing.

## Decision

An optional node application is made from three separately owned pieces:

```text
app sysext payload
  immutable executable and unit payload fetched as a Katl node extension bundle
  or supplied by a user through the same bundle contract

generated confext
  node-specific configuration rendered by katlc from a supported typed input
  domain and selected in the same generation as the app sysext

operation and status state
  live status, durable operation records, and any declared writable app state
  under Katl-owned paths
```

Users do not hand Katl a raw sysext path, a global systemd extension directory,
an arbitrary unit file, an arbitrary package name, or an arbitrary `/etc` patch.
Katl accepts only an extension bundle whose manifest, payload digest,
compatibility metadata, capabilities, unit declarations, config paths, status
paths, and provenance pass validation.

Generation spec remains authoritative. Selecting an app sysext, generated
confext, or both creates a new generation or a bounded live-apply operation that
records the resulting generation. Rollback switches the selected root, sysext
set, and confext set together. Any live side effects outside that selected
generation, such as route advertisements or daemon sessions, are recovered only
through the app's declared operation and fail-closed behavior.

## App Metadata

Every app sysext bundle manifest must declare:

```text
identity
  appID
  artifactKind: katl.node-app-sysext.v1
  artifactVersion
  payloadVersion
  architecture
  sha256
  sizeBytes

systemd extension identity
  extension-release ID
  extension-release VERSION_ID or image version
  SYSEXT_LEVEL, when used
  architecture asserted by extension-release metadata

compatibility
  supportedRuntimeInterfaces[]
  minKatlOSVersion, when needed
  maxKatlOSVersion, when needed
  requiredKernelModules[]
  requiredUnits[]
  requiredMounts[]
  requiredCapabilities[]

capabilities
  stable capability names implemented by the app
  capability versions
  supported config schema IDs
  supported operation kinds

unit contract
  providedUnits[]
  entrypointUnits[]
  readinessUnits[]
  activationPhase
  ordering requirements

configuration contract
  configHandoffPaths[]
  generatedDropInPaths[]
  secretRefKinds[], when supported

status contract
  liveStatusPath
  statusSchemaID
  redaction policy
  durable snapshot expectations

rollback contract
  failClosedActions[]
  liveRollbackSupported
  requiresRebootForRollback
  externalStateWarning

provenance
  sourceRepository
  sourceRevision
  buildInputDigest or packageLockDigest
  createdAt
  signing material or explicit unsigned-fixture marker
```

`appID` is a stable lower-case identifier such as `bird` or
`platform-api-endpoint`. It is used in Katl-owned paths and unit names. The
manifest must use safe single path segments and must not contain path traversal,
absolute user-selected paths, or names that collide with Katl core services.

`payloadVersion` describes the application payload API or upstream daemon
payload. `artifactVersion` describes the immutable Katl bundle build that
carries that payload. `supportedRuntimeInterfaces[]` is the primary compatibility
gate. KatlOS product-version matching is not enough to select an app sysext.

## Unit Discovery And Activation

Katl does not discover app behavior by scanning arbitrary units from a sysext.
The manifest must list every provided unit and identify the entrypoint units and
readiness units Katl may manage.

Unit names must be Katl-scoped:

```text
katl-app-<appID>.target
katl-app-<appID>.service
katl-app-<appID>-ready.service or target
```

App sysexts may ship base units under the normal systemd unit directories made
visible by `systemd-sysext`. Generated confext may add bounded drop-ins or
enablement symlinks for those declared units, but it must not create an
undeclared daemon, override Katl core units, or hide kubeadm, kubectl, Helm,
CNI, GitOps, package-manager, or arbitrary shell execution inside app
activation.

`activationPhase` is one of:

```text
pre-kubeadm
  app may be required before kubeadm init or join reaches a phase, for example a
  platform API endpoint helper

post-kubeadm
  app starts after local kubeadm-owned state exists

post-cilium
  app starts only after user-managed CNI handoff evidence exists

maintenance
  app runs only for explicit operation work
```

Katl validates the phase against the requested operation. A `pre-kubeadm` app
must declare the readiness target that gates the later kubeadm phase. A
`post-cilium` app must not satisfy bootstrap API reachability checks.

At boot, `katl-generation-activate.service` exposes only the selected
generation's sysexts and confexts under `/run/extensions` and `/run/confexts`
before `systemd-sysext.service` and `systemd-confext.service` run. App units
start only after the selected extension set is active and after their declared
native dependencies are satisfied.

## Configuration Handoff

Node-specific app configuration is generated confext rendered by `katlc`.
The default config handoff path is:

```text
/etc/katl/apps/<appID>/config.yaml
```

An app may declare additional Katl-owned paths under:

```text
/etc/katl/apps/<appID>/
/etc/systemd/system/katl-app-<appID>*.d/
```

Domain-specific renderers may also write supported native config such as
systemd-networkd files when that domain owns the path. All config paths must be
declared in the app manifest and validated by the corresponding Katl input
domain. The app reads normalized generated config; it does not parse arbitrary
operator files from random paths.

Secrets are not inline app config. If an app needs credentials, the manifest
must declare supported `secretRefKinds[]`, and a separate secret-materialization
design must define storage, redaction, rotation, and status behavior before the
app becomes user-facing.

## Status And Health

Each app exposes one bounded live status document:

```text
/run/katl/apps/<appID>/status.json
```

The status document is runtime state. It is not the trust root for generation
selection and must not contain persistent identity or secret material. When an
operation needs durable evidence, `katlc` copies a redacted status snapshot into
the operation record:

```text
/var/lib/katl/operations/<operation-id>/apps/<appID>/status.json
```

Normal app status must include:

```text
appID
artifactVersion
payloadVersion
selectedGenerationID
healthState
readinessUnit
lastTransitionTime
failureReason
redactionVersion
```

App-specific schemas may add bounded fields, such as routing peer summaries or
advertisement state. They must not require operators to inspect raw daemon
state for routine status. Debug bundles may include daemon logs if the app
declares redaction rules.

Health is declared by the app manifest and verified through systemd readiness
units plus status content. For fail-closed apps, health failure must trigger the
declared fail-closed action, such as withdrawing route advertisement, before the
status reports the app unhealthy.

## Rollback And Live Apply

Generation rollback restores the previously selected app sysext payload and
generated config because both are part of generation spec. It does not
automatically roll back external fabric state, Kubernetes objects, Cilium
objects, remote routes, or daemon sessions unless the app contract declares a
tested operation that performs that recovery.

An app may support:

```text
next-boot apply
  render and select a new generation; the new app state becomes active after a
  boot trial

live apply
  render a new generation and run a declared operation that validates, applies,
  checks health, records status, and either commits or rolls back live state

operation-only apply
  reject normal config apply and require a named operation
```

`live apply` is unsupported by default. It becomes available only when the app
has tests for preflight, resource locking, status snapshots, failure handling,
and rollback or fail-closed behavior. If safe live restoration is not proven,
the app must require next boot or explicit operator recovery.

## Signing And Provenance

Katl-provided and user-provided app sysexts use the same bundle validation
contract. A user-provided app is not special-cased into raw sysext activation.
It must be fetched or supplied as a node extension bundle whose manifest and
payload digest validate.

Before signing lands, local fixtures may be checksum-only if the bundle manifest
contains the same digest, runtime compatibility, unit, config, status, and
provenance fields the signed path will use, plus an explicit unsigned-fixture
marker. Published or generally user-facing app bundles require a signing and
trust-root decision before they are treated as stable distribution artifacts.

The provenance record must identify source repository, source revision, build
input digest or package lock digest, creation time, and bundle digest. Katl
status may display provenance summaries, but compatibility and activation are
decided by validated metadata and digests, not by repository names.

## Test And Fixture Requirements

An optional node application is not user-facing until it has:

```text
manifest validation tests for metadata, capabilities, units, config paths,
  status path, runtime compatibility, digests, signing hooks, and provenance

artifact tests proving the sysext contains only declared units, executables,
  extension-release metadata, and expected package or build inputs

golden tests for generated confext config, drop-ins, and supported native files

negative tests for raw sysext paths, undeclared units, unsupported capability
  names, incompatible runtime interfaces, missing status path, unsafe config
  paths, inline secrets, and ambiguous rollback behavior

systemd-analyze verify for app units and generated drop-ins where practical

status parser tests with redaction and schema-version coverage

VM tests for activation ordering, health success, health failure, fail-closed
  behavior, rollback or next-boot recovery, retained diagnostics, and declared
  host capability skips
```

Fixtures must use the same manifest and digest shape as published bundles. A
fixture may be served from local HTTPS or VM-test infrastructure, but its
metadata must not require later `katlc` fetch and staging code to change
semantics when the artifact moves to GHCR or a GitHub Releases static layout.

## Non-Goals

This contract does not make BIRD, API VIP advertisement, CNI installation,
storage add-ons, GPU enablement, ingress, or arbitrary routing behavior part of
the KatlOS base runtime.

The reusable node extension bundle OCI/static manifest is defined in
`docs/internal/node-extension-bundle-format.md`.

This contract does not define the BIRD app, the BGP API VIP app, their typed
input schemas, or their operation records. Those remain app-specific follow-up
work.
