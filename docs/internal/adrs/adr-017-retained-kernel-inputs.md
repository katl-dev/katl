# ADR-017: Retained kernel RPM inputs

Status: accepted.

Date: 2026-10-16.

## Decision

Select one reviewed kernel RPM set for each Fedora release, architecture, and
kernel flavour. Retain the actual RPM bytes in Katl's dedicated
`ghcr.io/katl-dev/katl/kernel-inputs` package. A checked-in lock binds the
selection to an OCI manifest digest and every RPM's identity, size, and SHA-256.

This decision concerns the kernel ABI. Keep userspace repository resolution
unchanged, including generic `kernel-headers`, `kernel-srpm-macros`, compiler
packages, and dependencies of the selected RPMs. Retain `core`, `modules`,
`modules-core`, and the matching `devel` package for each flavour.

Normal builds must consume the selected retained inputs. Only the update
workflow discovers kernels from moving Fedora and Copr repositories. Missing,
incomplete, or corrupt retained inputs must fail before image-cache reuse or
package installation.

## Context and alternatives

Floating kernel package names let CI and development select different kernel
APIs from the same source revision. Binding extensions to a runtime after it
has built prevents mixing build inputs, but does not control that selection.

Pinning only a version or NEVRA still depends on upstream package retention and
does not establish byte identity. Freezing complete repositories would change
userspace resolution and make Katl responsible for a broader snapshot. Building
Katl's own kernel adds maintenance ownership without satisfying an additional
product requirement. A retained kernel-only RPM set is the smallest boundary
that makes kernel selection reproducible.

## Capture and consumption

`katl-kernel-release candidate` resolves a kernel release once, obtains all four
matching RPMs, verifies their upstream signatures and embedded RPM identities,
and exports their complete OCI closure. Fedora origins use canonical repository
URLs so mirror selection cannot create duplicate proposals. Capture timestamps
are normalized so repeating capture does not change the retained identity.

Reuse the existing payload-bundle transport and immutable digest-derived
retention tags. Do not use Actions cache entries or expiring workflow artifacts
as durable RPM storage. Workflow artifacts only carry captured inputs between
jobs. Keep tagged retained inputs while any committed lock references them.

Before building, verify the selected lock's Fedora release and flavour, then
verify every materialized RPM's bytes. Supply those files through native mkosi
`PackageDirectories=` and explicit `/repository/<filename>.rpm` specifications.
This prevents another repository's package with the same NEVRA from winning
resolution. Install `devel` only in the runtime build overlay.

Preserve the existing runtime-owned prepared header archive and its runtime
digest, kernel release, configuration, and `Module.symvers` checks. Carry the
retained set identity through runtime, header, and extension provenance. Verify
all discovered release-owned recipes and their complete module composition.

## Cache identity

Include the selected RPM dependency identity in wrapper artifact fingerprints,
mkosi incremental cache keys, CI cache keys, and build provenance. The identity
includes package digests; a version string alone is insufficient.

Mkosi 26 fingerprints local package names and modification times. It does not
establish package content identity, so a digest-addressed RPM directory alone
does not isolate cached images. Use a kernel-set-specific native `CacheKey=`
as well as verification before wrapper cache reuse.

## Updates and promotion

The scheduled or manually invoked Kernel Updates workflow captures both
flavours and creates a lock proposal commit. Trusted workflow-source jobs retain
the signed bytes, verify anonymous retrieval of every RPM, and confirm the
proposal branch still points to the captured commit before opening a
ready-for-review PR. Input retention does not constitute qualification or
promotion. Retry incomplete publication; skip only when main selects the same
dependency identities. Advance proposals by merging main without rewriting
history.

PR CI is the qualification owner. For each changed flavour, build both boot
artifacts and every release extension, verify their complete composition, and
run VM regression and production DRBD replication. Shared ABI inputs require
both flavours. Do not duplicate this qualification before opening the PR or
carry custom qualification state between runs.

A repository-scoped GitHub App opens proposals so native PR checks run. A
privileged workflow triggered by successful CI may enable native auto-merge
only for the exact unchanged App-authored, same-repository, kernel-lock-only
revision. It never executes proposal code. Branch protection remains the merge
authority; failed, skipped, or cancelled selected gates cannot permit promotion.

Build and composition qualification does not prove functional driver behavior.
Run the applicable capable-host VM gates and persistent public `katlctl`
journeys before landing, including DRBD replication when affected. Record
NVIDIA hardware qualification only when actual hardware evidence exists.

## Bootstrap

Land the capture, retention, and explicit candidate qualification tooling before
activating mandatory locks. Publish the initial qualified RPM sets, make the
dedicated package publicly readable, and verify anonymous complete retrieval.
Then land the locks and mandatory consumer integration together. The completed
integration has no fallback to floating kernel packages.

No KatlOS release tag or product release is required to bootstrap input storage.
