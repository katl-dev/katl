# ADR-015: Release-owned kernel extensions and external userspace builds

Status: accepted.

Date: 2026-09-22.

This ADR specifies interfaces and behavior to implement, including the `release`
selector and `--apply-config` option. Acceptance does not establish their
availability.

## Decision

Katl builds and qualifies kernel-bound extensions in `katl-dev/katl` as part of
each release. Active maintenance covers only the latest release. Older artifacts
remain available for pinned installations and rollback, but receive no routine
rebuilds or backports.

External extension repositories initially support userspace software only,
built through `katlext`. BIRD moves to `katl-dev/extensions` as the first
consumer. DRBD9 is the first release-owned kernel extension, followed by NVIDIA.
Both build paths produce optional bundles that use the same Katl configuration
and generation model.

Operators select release-owned extensions by their full OCI repository reference.
Katl resolves that repository against the runtime selected for the target
generation. A host upgrade stages the target runtime, unified kernel image
(UKI), and matching selected extensions together, then activates them in one
reboot. Compatibility validation must prevent activation of an incompatible
kernel and extension combination. Drivers are prepared before boot.

Ordinary configuration apply does not upgrade KatlOS. Host upgrade retains
the node's effective configuration unless the operator explicitly requests a
combined host-configuration change. Install, apply and upgrade share one
target-generation resolver. Every committed generation records exact artifact
digests; boot and rollback perform no version discovery.

## Context

Users need custom host extensions without contributing application-specific
support to Katl. A template repository and shared builder can provide this for
userspace software through configuration, optional scripts and native files.

DRBD9 and NVIDIA introduce kernel compatibility constraints. Exposing external
kernel builds immediately would require a public kernel SDK, cross-repository
release coordination, and a policy for rebuilding historical targets. Keeping
kernel builds in the Katl repository limits that work to combinations Katl
releases and qualifies.

Build ownership alone does not solve upgrades. Preserving an old kernel
extension while replacing the runtime can leave an unusable candidate. Applying
the new extension separately to the old runtime can also fail compatibility.
The required operation is to prepare the complete future combination while the
old generation remains active, then activate it in one reboot.

Katl's product boundary requires prebuilt software, node-local configuration
compilation, and rollback of complete generations. Cluster storage systems and
workload orchestration remain outside Katl.

## Ownership

The repositories divide build and lifecycle responsibilities as follows:

| Repository | Responsibility |
| --- | --- |
| `katl-dev/katl` | Kernel-bound recipes and build inputs, release qualification, compatibility enforcement and generation lifecycle |
| `katl-dev/katlext` | Public userspace build tool, recipe schema, shared packaging support and reusable GitHub Actions workflow |
| `katl-dev/extensions-template` | Minimal user workspace and pinned caller workflow, without copied build infrastructure |
| `katl-dev/extensions` | Common external userspace recipes and extension-specific tests, using the same workflow as users |

DRBD9 kernel modules belong in-tree. Optional `drbd-utils` can be a separate
userspace extension. The NVIDIA driver extension belongs in-tree with its
corresponding host driver libraries and firmware. The NVIDIA Container Toolkit
can be external, qualified against supported driver and container-runtime
combinations.

External userspace software may depend on a supported host driver, but does not
build or choose kernel-specific driver artifacts. Existing Kubernetes payload
ownership and its separate upgrade workflow are unchanged.

## Release-owned kernel extensions

### Build and publication

Kernel extensions consume the exact candidate runtime and prepared kernel build
inputs inside Katl's release pipeline, including symbol-version information
where required. This path does not require a public kernel-development SDK.

The release pipeline must complete these steps in order:

1. Build the candidate runtime and kernel.
2. Build matching kernel extensions from in-tree recipes.
3. Qualify the advertised combinations.
4. Publish immutable artifacts.
5. Publish the release manifest referencing their digests.

Every advertised extension must pass qualification for its supported
architecture and kernel flavor before promotion. A failed build must not
silently remove promised coverage from a candidate.

The release manifest provides an authoritative mapping:

```text
Katl release identity
    -> architecture and kernel flavor
        -> exact kernel target
            -> full OCI repository reference
                -> bundle reference pinned by digest
```

The kernel target identifies the exact build, not only an upstream version
string. A matching tag name is not compatibility evidence. Including an
extension in the manifest does not install it automatically.

The manifest and all artifacts required by the selected generation must be
available through the supported release or local-handoff path. Boot must not
depend on contacting the release service or registry.

### Maintenance policy

Kernel changes trigger matching builds. A driver update or recipe fix ships in
a new Katl release even when the kernel is unchanged. Published release
manifests and artifact identities are never replaced with different content.

Older releases receive no routine extension rebuilds or backports. Their
published artifacts remain available, and local retention protects artifacts
needed by retained valid generations. Users may need to upgrade Katl for a
driver fix.

Identical artifacts may be reused when their relevant immutable inputs match
and the combination passes qualification. No separate historical compatibility
catalog or rebuild service is required for the initial implementation.

## External userspace extensions

`katlext` accepts a workspace containing versioned YAML recipes, verified
sources, optional build scripts, and `rootfs/` overlays. Scripts install into
`DESTDIR`; the tool owns validation and packaging. Authors do not need to write
mkosi configuration or Open Container Initiative (OCI) publication logic.

Typed parameters and constrained scalar substitutions support version pins and
ordinary build matrices. Each version and its checksum remain one parameter set.
Arbitrary YAML evaluation and mkosi configuration passthrough are excluded.

Builds consume pinned userspace targets derived from Katl's runtime. Those
supply the base and dependency information needed for assembly without exposing
kernel-development inputs. Userspace compatibility remains explicit; absence
of kernel modules does not establish compatibility with every Katl release.

The initial API has no `kernel:` recipe section or kernel-build environment
variables. `katlext` rejects kernel-module payloads and unsupported kernel
fields. This validation enforces the supported build contract. It does not
sandbox third-party code.

The reusable workflow discovers recipes and builds declared targets on recipe
changes or manual invocation. Target updates are explicit repository changes.
Publishing credentials are unavailable to recipe execution; publication uploads
already-built artifacts. The caller pins the workflow and build records pin the
builder and resolved inputs.

Automatic release subscriptions, rolling historical-kernel target sets and a
multi-release rebuild service are deferred.

## Common artifact and selection contract

Both paths use the `SystemExtensionBundle` family and `systemExtensions`.
Kernel-bound bundles carry an enforced kernel-target constraint and module
inventory. An older consumer that cannot enforce required constraints must
reject the artifact contract, rather than ignore additional fields. Existing
userspace bundles remain supported under their existing contract.

### Release-relative and explicit selection

Add `release` as an alternative to `bundle` for a present extension:

```yaml
spec:
  defaults:
    systemExtensions:
      - release: ghcr.io/katl-dev/katl/extensions/drbd9

      - bundle: ghcr.io/katl-dev/extensions/bird@sha256:<digest>
```

The digest is illustrative. There is no separate `name` field. `release` is a
full OCI repository reference, including its registry and namespace, without a
tag or digest. The target KatlOS release manifest maps that exact repository to
a digest-pinned artifact in the same repository. Katl does not infer a registry,
organization, or repository prefix. The registry and organization in these
examples are not resolver restrictions. Existing configuration and unit fields
remain available.

Each entry requires exactly one of `release` or `bundle`. The full repository
identifies the entry for duplicate detection and configuration layering; a
bundle's tag or digest is not part of that identity. A node entry for the same
repository overrides its default, including when changing between `release`
and `bundle`. Repositories with the same basename in different namespaces remain
distinct.

To remove an inherited entry, use its selector with `state: absent`. Removal
uses the repository identity and does not fetch or resolve an artifact:

```yaml
systemExtensions:
  - release: ghcr.io/katl-dev/katl/extensions/drbd9
    state: absent
```

`release: ghcr.io/katl-dev/katl/extensions/drbd9` selects the DRBD9 artifact from the release used by that
generation. It does not select the newest published driver or newest Katl
release. The user pins the supported kernel/driver combination by choosing the
Katl release, without maintaining a second matching version in configuration.

`bundle` retains explicit-artifact semantics. An incompatible pin is rejected,
not replaced. Existing bundle entries are never inferred to be release-relative
from their names, registry or tags. Migration to `release` is explicit.

An untagged OCI reference does not imply release-relative selection. Katl does
not synthesize extension tags by appending the OS version. Tags may exist for
human convenience; the release manifest supplies the authoritative digest.

### Selection intent and resolved identity

Katl persists both the operator's selection intent and the resolved artifact
identity in the node's effective configuration and generation records:

```text
Intent
    release: ghcr.io/katl-dev/katl/extensions/drbd9

Resolved selection
    immutable Katl release identity
    architecture, kernel flavor and kernel target
    extension bundle digest
    payload digests
```

The `release` selector remains intent; Katl generates the resolved metadata.
Users do not maintain those fields. An upgrade retains intent while
updating the resolved selection. Status and subsequent apply operations must
observe the same identities as the booted generation.

## Target-generation resolution

The node owns extension resolution and acquisition. For configuration apply and
host upgrade, `katlc` prepares the generation from operator intent and the
operation's target runtime. Installation uses the same selection and compatibility
rules with the selected installation image. Workstation install defaults and the
running kernel are insufficient when preparing a different generation.

`ClusterConfig` does not declare a KatlOS version. Ordinary apply uses the node's
current release. The explicit upgrade command selects a published release or
local image; the image's verified release manifest identifies the target runtime
and qualified extension digests. Selecting that target does not change the scope
of ordinary apply.

The workstation expands local configuration files and sends extension selectors,
configuration, and unit settings. It does not need to resolve extension tags,
download extension payloads, or receive artifact bytes from the node. Configuration
building and operation preparation may use the network; neither requires an
air-gapped workflow. A self-contained installation bundle can also supply verified
payloads.

Each operation supplies the following inputs:

| Operation | Runtime and extension resolution | Configuration input |
| --- | --- | --- |
| Install | Selected install release | Proposed installation configuration |
| Ordinary `cluster apply` | Each selected node's current runtime release | Proposed supported configuration |
| `node upgrade` | Requested upgrade release | Node's current effective configuration |
| `node upgrade --apply-config` | Requested upgrade release | Proposed supported host configuration |
| Rollback | Recorded generation; no new resolution | Recorded generation configuration |

Resolution is per node. During a rolling upgrade, the same `release` repository
can resolve to different digests on nodes running different releases or kernel
flavors. `katlc` selects the digest from the target release manifest or resolves
an explicit bundle tag during operation preparation. An explicit digest remains
fixed.

Preparation reuses matching verified generation payloads or fetches the selected
artifact. An upgrade image can supply its advertised artifacts directly. Reuse
must enforce the same target-compatibility checks as acquisition. Missing or
corrupt retained payloads fail verification; retained metadata alone does not
prove that a payload is available.

Before accepting a mutating operation, `katlc` freezes the resolved identities and
verified payloads in its operation inputs. Execution verifies that the base
generation and target still match before mutation, without resolving mutable tags
again. A changed base or target requires replanning. A preview does not authorize
mutation; acceptance prepares and freezes the operation's inputs.

The node does not need a separate persistent OCI graph cache. Generations retain
the verified native payloads and resolved identities needed for activation and
rollback. Release images retain their self-contained artifact closure for
installation and upgrade acquisition. Boot and rollback never fetch artifacts or
re-resolve selectors.

### Ordinary configuration apply

Apply does not upgrade KatlOS. Adding a release-relative extension resolves it
for the node's current release and stages a next-boot generation when payloads
change. Existing supported live configuration behavior remains unchanged when
no payload change requires a reboot. Applying configuration does not itself
reboot the node.

After an upgrade, unchanged release-relative configuration must be a no-op.
It must not restore the old digest from stale resolved metadata or a
workstation's original install release.

A request for an extension unavailable or incompatible with the current release
fails with upgrade guidance. Apply must not implicitly change the runtime to
satisfy that request.

### Normal host upgrade

For a routine upgrade, set `TARGET_RELEASE` to the desired KatlOS release, review
the plan, and then execute the upgrade:

```sh
katlctl node upgrade worker-1 --version "$TARGET_RELEASE" --config cluster.yaml --plan
katlctl node upgrade worker-1 --version "$TARGET_RELEASE" --config cluster.yaml
```

`--config` retains its targeting role unless `--apply-config` is explicitly
requested. Normal upgrade preserves the node's effective configuration and
selected Kubernetes payload. It re-resolves release-relative extensions against
the target release and preserves explicit bundles only when compatible.

The plan must show runtime, kernel, and extension changes before execution. This
illustrative summary shows the required information, not a fixed output format:

```text
KatlOS       N-1 -> N
Kernel       K1  -> K2
DRBD9        previous digest -> release-N digest
BIRD         unchanged
Kubernetes   unchanged
Activation   one reboot
```

The operation must:

1. Resolve and validate the complete proposed combination. Acquire and verify
   every selected artifact before staging boot state. Missing or incompatible
   extensions fail preflight before inactive-slot mutation.
2. Prepare the target module set and dependency indexes without loading its
   modules or merging its extensions into the running generation.
3. Stage the runtime, UKI, extensions and configuration as one candidate. Persist
   consistent generation records and effective-manifest metadata.
4. Arm the bounded trial only after the candidate is complete and verified, then
   perform the existing reboot.
5. Activate the recorded candidate and qualify boot health, including required
   module loading, before promoting it. On failure, retain or restore the
   previous complete known-good selection through the existing recovery model.

```text
Running       runtime N-1 + kernel K1 + driver for K1
Staged        runtime N   + kernel K2 + driver for K2
                         |
                         | one reboot
                         v
Running       runtime N   + kernel K2 + driver for K2
```

Preparing N's artifacts while N-1 runs is supported. Activating those artifacts
on N-1 is not part of the upgrade. Atomicity here means activation of a complete
selected generation; it does not imply that every filesystem write is one
atomic operation. Interrupted staging must leave the current known-good
generation bootable and must not select an incomplete candidate.

### Combined host upgrade and configuration apply

The `--apply-config` option allows a host configuration change to accompany the
runtime upgrade, such as enabling an extension first available in the target
release. The specified interface is:

```sh
katlctl node upgrade worker-1 --version "$TARGET_RELEASE" \
  --config cluster.yaml \
  --apply-config \
  --plan
```

Omitting `--plan` executes the combined operation. The proposed host
configuration is compiled against the target release and staged with it for one
reboot. No subset of that combined change is applied live to the old generation.

The option covers supported host changes that can participate in that candidate.
It does not include Kubernetes version or cluster-wide kubeadm changes,
cluster-membership operations, destructive storage actions or workload
orchestration. Unsupported changes fail planning with the appropriate workflow.

Without this option, editing local configuration must not silently expand a
host upgrade's scope. This path is unnecessary for routine upgrades of already
selected release-relative extensions.

## Failure, concurrency and rollback

A missing release entry or unsupported architecture or kernel flavor blocks the
operation. Katl must not keep an incompatible old driver, silently remove an
extension or substitute one from a different release. An incompatible explicit
pin requires an explicit selection change, potentially through the combined
upgrade operation.

Node-side enforcement remains mandatory. Source agents unable to understand the
required bundle or operation contract must reject it before mutation; a newer
workstation CLI must not bypass this check.

Only one mutating node operation may proceed at a time. In the initial
implementation, an unbooted candidate or pending trial blocks another apply or
upgrade. The operator must complete or explicitly discard it first.
`--apply-config` does not implicitly adopt an
existing pending candidate. Katl must revalidate the base generation while
holding the mutation lock.

An acquisition or validation failure does not change the active generation or
arm a reboot. Failure after staging starts must leave no incomplete candidate
selected. All artifacts required for the trial and supported rollback remain
local and protected from garbage collection.

Rollback restores the recorded runtime, UKI, kernel arguments, extension set
and configuration. It does not consult current release metadata. It also does
not undo storage data, on-disk metadata, Kubernetes state or external changes.

## Runtime responsibilities and limits

Katl composes dependency indexes for the complete selected module set. It rejects
conflicting providers and validates deliberate replacement of a base module.
Individual extensions must not overwrite global indexes independently.

Native boot ordering makes the selected files and indexes available before
required modules load and dependent workloads start. A required module failing
to load prevents a healthy-generation result and produces useful diagnostics.
Boot health checks do not establish application-level replication or GPU
workload health.

Kernel-extension changes are next-boot operations. No forced unloading, live
driver replacement, driver-specific supervisor or arbitrary node installation
hook is introduced.

Initial support covers workloads that start after the root filesystem is
available, on targets whose policy permits unsigned modules. Katl never disables
signature enforcement to make an extension work. Signing, trusted-key
provisioning and drivers required to mount
the root filesystem remain separate workstreams.

systemd 262 is not a prerequisite. Existing update mechanisms remain in place;
expanding sysupdate integration is outside this decision. Background acquisition
must never independently refresh the active extension set or select a boot.

DRBD resources, replication policy and LINSTOR/CSI remain user-owned. GPU device
plugins and GPU Operator remain in the cluster layer. Host container-runtime
configuration uses Katl's supported configuration path. Storage-aware
maintenance and workload availability remain operator responsibilities.

## Delivery and acceptance

### Selection and target-generation lifecycle

Implement release-relative selection, release manifest mappings, and persistence
of selection intent and resolved identities. Generalize upgrade replacement to
release-owned extensions rather than maintaining application-specific
replacement branches. Update
candidate generation records and effective configuration together.

Tests must establish all of the following:

| Scenario | Required result |
| --- | --- |
| Existing release-owned DRBD9, N-1 to N | Matching runtime and driver staged together; one reboot |
| Required driver missing, wrong kernel or unsupported contract | Preflight failure before inactive-slot mutation or reboot |
| Explicit bundle pin | Never silently replaced; incompatible target rejected |
| Reapply unchanged configuration after upgrade | No-op; no stale driver digest restored |
| Apply to nodes on mixed releases or flavors | Each node resolves its own compatible artifact |
| Change kernel flavor at the same Katl version | Resolve the new target's driver and indexes together |
| Extension first introduced in N | Ordinary apply on N-1 fails; combined upgrade stages it with N |
| Unrelated local edits without `--apply-config` | Normal upgrade preserves effective node configuration |
| Pending candidate or concurrent base change | Reject or replan; never silently combine operations |
| Interrupted acquisition/staging or failed trial | No incomplete generation boots; known-good recovery remains available |
| Rollback with registry unavailable | Restore recorded kernel and driver without discovery |

### External builder and BIRD

Extract the generic producer/build functionality and migrate BIRD. A new template
repository must build multiple userspace recipes without a Katl checkout or
workflow edits. BIRD retains its native configuration interface and passes
install, reboot and rollback tests. Preserve published BIRD references and test
external rejection of kernel-module payloads.

### Release-owned DRBD9

Implement generic module composition and the in-tree DRBD9 recipe. Two VMs with
disposable disks must verify the intended module provider, replication, reboot
and recovery after peer interruption. Exercise the complete N-1 to N upgrade,
unchanged-config reapply and rollback loop. Removal after resources are safely
stopped must leave no stale module indexes.

Keep the DRBD software version fixed for the first kernel-upgrade test. Qualify
DRBD-version transitions separately; software rollback is not data rollback.
A release advertising this target cannot promote without its qualification.

### NVIDIA follow-on

Use the same mechanism for NVIDIA. Qualify a declared GPU target with a
Kubernetes GPU workload and the supported upgrade/rollback loop. Validate toolkit
integration and node-generated device configuration without NVIDIA-specific
branches in generic generation activation.

## Consequences

The public builder remains small. Katl owns a bounded set of qualified
kernel/driver combinations, and operators do not coordinate separate driver
version bumps for routine OS upgrades. One configuration can support a rolling
upgrade while each node retains explicit resolved identities.

Adding a kernel module requires a Katl contribution. Driver fixes follow Katl
releases, and historical releases do not receive routine backports. Qualification
adds release workload. Release manifests, source-agent compatibility and the
shared target-generation resolver become correctness-critical interfaces.

Reuse shared packaging and validation without requiring a public kernel SDK for
the internal build path. Public kernel builds can be reconsidered after the
in-tree lifecycle establishes stable requirements.

## Alternatives considered

### External builds for all extensions

Deferred because this requires a public kernel SDK and coordination of
historical targets before the kernel lifecycle has been validated.

### Keep all extensions in the Katl repository

Rejected because userspace customization must not require Katl contributions.

### Separate driver installer

Rejected because both extension classes must share generation enforcement and
rollback.

### Untagged references or matching OS and extension tags

Rejected as the selection contract because tags do not explicitly encode the
target-release relationship or its architecture and kernel-flavor mapping.
Named release entries resolve to authoritative digests.

### Activate the OS and driver separately

Installing the driver after upgrading the OS, or activating the target driver
before upgrading, can expose an incompatible running combination. Katl stages
the complete candidate before reboot.

### Implicitly combine configuration apply and OS upgrade

Making every apply upgrade the OS, or every upgrade apply local configuration,
would silently expand the operation's scope. The explicit `--apply-config`
option provides the combined operation when needed.

## Relationship to other decisions

This ADR extends the selection and lifecycle contract in
[ADR-012: User-owned system extension bundles](adr-012-user-owned-system-extension-bundles.md).
It moves common external userspace recipes, starting with BIRD, out of the
in-tree producer described there. Configuration, generation activation, and
rollback remain shared responsibilities.

[ADR-013: Firmware and CPU microcode](adr-013-firmware-and-cpu-microcode-support.md)
defines the base firmware and in-tree driver policy. This ADR adds optional
kernel-bound extensions without moving cluster storage or workload orchestration
into Katl.

Detailed recipe schemas and release-manifest serialization remain implementation
designs within these boundaries. The selection semantics, operation scopes and
failure guarantees in this ADR are part of the decision, not deferred questions.
