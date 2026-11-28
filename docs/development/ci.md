# Continuous integration

**CI** is the required check for pull requests and the merge queue, and it also
runs fast checks on main. A short **Plan** job selects VM coverage from changed
paths and [driver qualification](driver-qualification.md) from the candidate's
input digests, so image builds and driver qualification start with the run
rather than after **Fast checks**. CI fails if any selected job fails, is
cancelled, or is skipped. CI expects unselected jobs to be skipped.

## Production images

Merge groups and pull requests into a `release/[0-9]*` series branch select both
Production flavours through `production-build.yml` and retain the assembled
artifacts and extension publication inputs for one day.
A merge group skips the Production build, Production VM, and bundle jobs when
every changed path is prose: `README.md`, `AGENTS.md`, or Markdown under
`docs/` other than `docs/support.md` and `docs/releases/`, which feed release
staging. Any other path requires both flavours. A prose-only merge group still resolves the production
identity and renders release notes so CI catches invalid commit trailers.

The build runs the production
checks (`check-runtime-root`, `check-runtime-boot-asset`, `check-installer-image`,
and the KatlOS image checks) with `KATL_VMTEST_IMAGE_SUPPORT=0` on production
bytes, in parallel with fast checks and VM regression. A following bundle job
combines the two flavours with `katl-release-artifacts combine`, as the release
workflow does. Production VM then installs each flavour's assembled bytes, reboots,
upgrades from the previous stable release, and rolls back through `TestReleaseLifecycle`
with no test agent, as the release does. It starts when the builds end and runs
alongside the VM regression lanes, which remain the longer path. CI requires all of
these jobs whenever the plan selects them. A change
that breaks a release check or the production lifecycle therefore fails before merge
rather than at tag time.
The assembled bytes remain available for one day as `katl-assembled-<flavour>-<sha>`.
The `vmtest` label on other pull requests selects regression VMs. Production
builds on pull requests are reserved for release series branches.

The build carries a development version above its upgrade source. The merge
queue uses `scripts/production-upgrade-baseline.txt`, which the release process
updates after publication. A pull request into a release series branch uses the
newest stable tag reachable from its base. The notes renderer uses the earlier
upgrade baseline pinned at the tested commit when available. Otherwise it starts
at the highest published stable version below the build version, including
releases tagged on sibling branches.

## VM coverage

GitHub-hosted Ubuntu runners execute the existing libvirt suite with KVM. Each
selected kernel flavour builds immutable test inputs once and runs five lanes in
parallel, balanced by measured test duration so that none exceeds about 15
minutes of tests (`laneBudget` in `cmd/katl-vmtest-plan`):

- **Readdress:** worker re-addressing.
- **Two-node:** two-node bootstrap, ISO/PXE boot, storage authority, and
  installer disk layouts.
- **Cluster:** three-node installation, external worker join, volume
  binding, and moving a trusted-network node to mTLS management.
- **Config:** configuration apply modes and service security.
- **Lifecycle:** installation and local handoff, management rotation, and
  update/rollback.

VM suites run in the merge queue, on a pull request into a `release/[0-9]*` series
branch, and on any other pull request only while it carries the `vmtest` label.
Pushes to `release/[0-9]*` branches run fast checks. A pull request without
either runs fast checks, the Nix build, and any selected driver qualification.
Adding `vmtest` reruns the head commit's CI with the VM suites, cancelling a run
still in progress; other labels do not affect CI. Pull requests from forks
cannot be restarted this way, so push a commit instead. Add `vmtest` early in
the life of a branch. Pushes to main run driver qualification for any flavour
that still needs evidence for its committed inputs. The merge queue covers
general VM regression.

The required check names the base CI tested. A pull request into a release
series branch publishes `CI (release)`; every other event publishes `CI`, and the
release branch ruleset waits for `CI (release)`
([release rulesets](release-rulesets.md)). A rerun replays the original
event, so it keeps the name of the base it tested. After a pull request is
retargeted onto a release branch it therefore has no `CI (release)` result:
open release pull requests with `gh pr create --base release/...`, or close and
reopen the pull request, or push a commit, after retargeting.

Every selected test must emit a Go JSON pass event. Labelled pull requests into
main run the affected suites on both standard and LTS. Merge groups and release
series pull requests run the full hosted suite on both flavours. For a
documentation-only diff, both the flavour and scenario matrices are empty, and
CI expects the hosted VM job to report skipped. Scheduled and manually
dispatched VM runs, including the nightly candidate, run the full hosted suite.

The planner compares the event's tested base SHA with the checked-out merge
commit for a pull request or the group head for a merge group. It includes both
paths of a rename, so moving a file into documentation still accounts for the
removed input. Only explicitly mapped scenario fixtures can narrow the suite.
Shared image, harness, and packaging inputs take precedence over fixture and
documentation rules. The planner resolves Go build and test dependencies,
embedded files, and VM input symlink targets in both trees. A missing base,
an unmapped non-documentation path, or an inventory failure selects the full
suite.

The **Plan** summary records the suites and the selection reason. While
`selectAffectedMergeGroupVMs` in `cmd/katl-vmtest-plan/vm_selection.go` is
false, full merge-group runs also publish `vm-shadow` with the affected suites
and estimated runner-minutes saved. The estimate sums the measured durations
of omitted tests across both flavours.

Before enabling affected selection on merge groups, collect at least 20 full
merge-group runs targeting main over at least three days, each with published
`vm-shadow` output. Every VM suite failure must occur in a suite selected by
the shadow plan. If an omitted suite fails, fix the mapping and restart the
count.

Also collect shadow runs for representative changes to every narrow rule.
For each rule, seed a regression in its mapped input on a test branch and
confirm that a suite in the shadow plan catches it in the full run. Include
a group with changes to two different rules and confirm its shadow lists
both sets of suites. Record the run links, selected suites, failure evidence
and estimated savings in the change that enables the switch.

The weekly schedule and manual dispatch both force driver qualification for
both flavours.

Kubernetes producer changes select the newest supported Kubernetes candidate.
The shared image job verifies and stages that bundle, and the two-node lane
exercises it. The separate scheduled **Kubernetes Bundle Artifacts** workflow
owns discovery and publication, rather than repeating the PR image build.

Use **Hosted VM scenarios** to dispatch the complete suite for a source commit
and kernel flavour. The default runner is `ubuntu-24.04`.
`KATL_VMTEST_RUNNER` can select a supported hosted provider. Run output is deleted
on success; small diagnostic artifacts expire after three days. Every image
build runs the builder that `mkosi-builder.lock` pins, described under
[Builder image](#builder-image); build input identities still control artifact
and package-cache reuse.

## Builder image

Image builds run mkosi inside the image built from `Containerfile.mkosi`.
**Builder Image** publishes that image as
`ghcr.io/katl-dev/katl/mkosi-builder` and proposes its digest in
`mkosi-builder.lock`; CI and `scripts/mkosi` pull exactly that digest.
Builder Image is the only workflow that publishes this package; other
workflows, actions and scripts may read or pull it.
`scripts/mkosi-builder` owns the rule and the lock format.

The content tag is `fedora-RELEASE-HASH`, where `RELEASE` is the Fedora
release from `mkosi.conf` and `HASH` is the first 16 hex digits of the SHA-256
of `Containerfile.mkosi`. The builder is built with an empty context, so those
two values are its only inputs. The committed digest is current when the lock's
`tag` equals the checkout's content tag. A change to either input therefore
makes the lock stale until a new image is published, and while it is stale
every job builds `Containerfile.mkosi` itself, without a layer cache. When the
lock is current, a CI job must run the pinned image, so that every job of a run
uses the same builder: it makes three pull attempts of at most ten minutes
each, with jittered exponential backoff, and then fails. Only a local
`scripts/mkosi` run falls back to building the file, with a warning. Each job's
summary records the reference it ran, whether it was pulled or built, and its
digest; the shared VM image job also keeps the record in its evidence.

On a push to `main` that leaves the lock stale, **Builder Image** builds and
pushes the content tag once, or reuses it when it already exists. The weekly
schedule and manual dispatch rebuild on the current Fedora packages and push
under a dated tag `fedora-RELEASE-HASH-YYYYMMDD`, leaving the content tag in
place; a second refresh on one day takes the next free `-2`, `-3` suffix, so no
tag is overwritten. That holds because the workflow's concurrency group runs
one publication at a time. Publication reads the registry through `skopeo`,
which reports the registry's error code: only `MANIFEST_UNKNOWN` or
`NAME_UNKNOWN` means a tag is missing, and any other error stops the run. GHCR
answers `MANIFEST_UNKNOWN` to the logged-in job for a package that does not
exist yet, so the first publication creates it. In both cases a changed digest
is proposed as an `automation/mkosi-builder-*` pull request, so the merge queue
tests the new builder with the code that runs it. Only the newest open
proposal stays open: the automation app's builder proposals are ordered by the
workflow run that described them, and each older one gets a comment naming the
newest, loses its branch and is closed. A rerun keeps its run's ID, so a rerun
of an old run closes its own proposal rather than a newer one. A change to
`Containerfile.mkosi` lands first with locally built builders, and the proposal
follows its merge.

GitHub creates a new container package as private. After the first
publication, an organization owner must make the `katl/mkosi-builder` package
public in its package settings, so pull requests and local builds can pull it
without credentials. Until then, CI fails on the first proposal pull request,
because its jobs cannot pull the pin it introduces; that failure is the gate
that keeps an unreachable pin out of `main`. Local builds fall back to building
the image.

## Retries

Network-dependent steps retry through one owner per language. Go code uses
`internal/retry`. Shell scripts use `scripts/retry`, which runs each attempt
under a time limit, makes a bounded number of attempts with jittered
exponential backoff, announces each retry as a workflow warning and the final
failure, with its reason, as a workflow error, and reports a malformed attempt,
timeout, kill-after or backoff override as a usage error before the first
attempt.
`.github/scripts/hosted-download`, `scripts/mkosi-builder` and the
`.github/actions/setup-tools` action, which realises the flake's ci shell,
call it.

The shell owner exists because those callers cannot rely on a Go module. When
a VM workflow tests an explicit source ref, it runs its scripts from a sparse
checkout of the workflow's own revision that holds only its actions and
scripts and none of the repository's Go packages, so a script there cannot
call `internal/retry`. That checkout must therefore include `scripts/`
wherever it runs a caller. The setup-tools action runs before the ci shell
provides Go, so it cannot call `internal/retry` either; its callers use full
checkouts.

## Build caches

Only runs on `refs/heads/main` save GitHub Actions caches, whatever the
event. Every other ref (pull requests, merge groups, release branches and other
branches) only restores, because no other ref can restore what those runs would
save. On a cache miss they rebuild the missing inputs.

[`.github/cache-policy.json`](../../.github/cache-policy.json) declares every
cache family: its key prefix, the jobs that save it, its size budget and how
many generations it keeps. `TestActionsCachesFollowPolicy` in
`internal/scriptstest` fails when a workflow or composite action uses a cache
the policy does not declare, keys a cache by commit, saves off `main`, or saves
a family from a job the policy does not name. The policy lists the existing
exceptions as known violations, and an exception that no longer occurs fails
the test, so the list only shrinks.

The daily **CI health** workflow runs `katl-queue cache --apply` from `main`.
It deletes entries saved off `main`, entries matching no family, and keys
beyond a family's generation limit, writes usage per family to the job
summary, and fails when what remains exceeds a family's budget or the
policy's alert threshold. Run `katl-queue cache` locally to see the same
report without deleting anything.

## Kernel updates

**Kernel Updates** captures signature-verified RPM sets, retains the immutable
inputs, verifies anonymous retrieval, and opens a ready-for-review PR. CI
qualifies each changed kernel lock on the pull request and in the merge queue.
For the affected flavour, it builds both boot artifacts and every release
extension, verifies module composition, and exercises production DRBD upgrade,
replication, and removal. The merge queue also runs the regression lanes.

After pull-request CI succeeds, **Merge Qualified Automation** enables native auto-merge only
for the unchanged, same-repository, App-authored, kernel-lock-only revision. It
does the same for a builder proposal that changes only `mkosi-builder.lock`.
Branch protection controls the merge. These virtual machines do not qualify
NVIDIA hardware.

Install an automation GitHub App on this repository with **Contents** and
**Pull requests** write access, and **Checks** and **Actions** read access. Set
the repository variable `KATL_AUTOMATION_CLIENT_ID` to its client ID and the
repository secret `KATL_AUTOMATION_PRIVATE_KEY` to its private key. Keep these
credentials in the trusted proposal and promotion jobs; PR test jobs use the
read-only built-in token. See [Creating a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).

The optional `katl-vmtest` ARC pool is restricted to trusted main revisions by
workflow routing, which today means manual dispatches and the nightly candidate
on main. Before enabling it, enforce trusted workflow/ref admission
vsock CIDs share the host kernel. Qualify the deployed pool manually before
setting `KATL_VMTEST_MAIN_RUNNER=katl-vmtest`.

## Release promotion

**Katl Release Artifacts** runs on tags, manual dispatch, and nightly on main.
The nightly candidate is a dispatch-equivalent run at the development version
above the pinned upgrade baseline, with its own concurrency group so it never
queues behind or cancels a tag run or a dispatch. It publishes nothing and
retains its bundle, assembled artifacts, and extension publication inputs for
7 days. Other release runs retain those artifacts for 30 days. A failed night
appears as a failed scheduled run.
Merged revisions are qualified by the merge queue without a second release-artifact build.
Release builds use the same notes renderer as CI. A candidate and its final
release share the same version boundary when the renderer selects the baseline.
Each flavour is built by the reusable `production-build.yml` workflow (inputs `flavour`,
`version`, `retention-days`), and its Production VM qualification starts
when that build ends. Releases run regression for both flavours and qualify each
flavour's assembled production bytes,
including installation, reboot, upgrade from the pinned baseline, and rollback
without a test agent. Requalifying an existing bundle reads the baseline pin
from the commit that produced the bundle. Release validation checks that the
publication bundle contains the same qualified bytes. See
[Release KatlOS](releases.md) for the promotion contract and failure handling.
