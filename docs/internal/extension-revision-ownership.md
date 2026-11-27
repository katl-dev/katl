# Extension Revision Ownership

Status: decided design; implementation tracked in #915, #763, #879 and #880.

This document defines who decides whether a change to the repository changes an
independently versioned system extension, and what the pull request gate, the
merge-group gate and publication on `main` each do with that decision. BIRD is
the only such extension today. Any later definition under
`extensions/<name>/extension.env` follows the same contract without new
workflow logic.

## Requirement

1. A published extension version is immutable. Its tag resolves to the same
   bytes for as long as it exists, and CI neither replaces those bytes nor
   fails because it rebuilt them.
2. A change that alters what the extension build produces cannot merge under a
   version that `main` already declares. The verdict applies to the code that
   merges, so it is part of the required check in the merge queue.
3. A change that leaves the extension's bytes unchanged merges without a new
   version.
4. The version at the head of `main` is published exactly once; a version
   superseded before it was published is never published.
5. One implementation makes the decision. Events differ in the revisions they
   pass to it, not in their rules.

The rule exists because one failure class recurred. The pull request gate and
the push-time publication step each matched changed paths against a list, over
different ranges, and the gate's verdict did not bind the merge. A change that
the gate had rejected reached `main`, and the push then rebuilt a published
tag, or a multi-commit push published nothing because only its last commit was
examined.

## Constraints

- Extension packages resolve from live Fedora repositories. The same commit
  built on different days produces different bytes, so a fresh build cannot be
  compared with a published artifact. Two builds are comparable only when the
  same job resolves both against the same repository metadata.
- Cached and fresh runtime roots are not yet identical (#841), so even two
  builds in one job cannot yet be compared by digest.
- No list of paths can decide whether bytes change. Shared build wiring such as
  `scripts/mkosi` and the runtime profile shapes the extension only through
  the base tree and the resolved transaction.
- The gate runs on pull requests from forks and in the merge queue, so its
  decision should not depend on registry availability or credentials.

## Decision

Two facts are kept apart, each with one source.

**The input fingerprint** is a digest of the extension's declared inputs in a
committed tree: paths, modes and content. It is a pure function of a commit,
so it is cheap, reproducible and comparable across time. Equal fingerprints
mean that no declared input changed. The declared set is not yet complete, so
until it is widened an undeclared input can still change the bytes; different
fingerprints mean only that the bytes may have changed.

**The artifact identity** is the digest of the sysext payload blob. It settles
whether a possible change is a real one: one job builds the base and the
candidate with the same `SOURCE_DATE_EPOCH`, `KATL_BUILD_COMMIT` and
repository metadata, and compares the two digests. Provenance annotations on
the manifest (the input fingerprint, the source revision and the creation
time) are not part of the identity. This is the comparison #763 prototypes,
and it depends on #841.

The gate compares a candidate with the commit it is applied to rather than
with the registry. A version is reserved when it reaches `main`, not when its
publication completes, so a later change cannot reuse a version whose
publication is still in progress or has failed. Because every merge passes the
gate, the inputs behind a declared version are constant from the commit that
introduced it, and `main` needs no stored record of what was accepted.

That argument holds only while every merge-group entry is tested against its
predecessor and merges on its own result. It depends on the `main` ruleset's
`ALLGREEN` grouping strategy; under a strategy that requires only the head of
a group to pass, an entry the gate rejected could merge.

Publication derives its intent from the registry instead of from a diff: a
version is published when its tag does not exist. The pushed range is
therefore irrelevant, a published tag is never rebuilt, and a version whose
publication failed is published by the next push.

The decision applies to `main` only. For a pull request into any other base,
including `release/*`, and for `ci.yml` push and schedule events, the planner
emits no extension action.

### Alternatives considered

- **Path patterns over a diff.** This is the current mechanism. It needs a
  correct range from every caller, which is what failed on multi-commit
  pushes, and it requires a new version for wiring changes that leave the
  bytes unchanged. It also invites one list per caller: the workflow `paths`
  filter, the gate pattern and the publication pattern are three today.
- **Fingerprint committed beside the version.** The Kubernetes bundle's
  `recipeDigest` is this model. It makes the check a unit test, but every pull
  request that touches shared build wiring must regenerate the value, and
  concurrent ones conflict in the merge queue. It still cannot tell an
  equivalent change from a real one.
- **Fingerprint compared with the published fingerprint.** This makes the range
  irrelevant for the gate as well, but only a commit-pure fingerprint can be
  compared across time, so it rejects equivalent wiring changes. Accepting them
  needs a second record of proven-equivalent fingerprints, in the registry or
  in commit statuses, that must be reconciled with the tag. It also ties a
  required check to registry availability, and a version on `main` that is not
  yet published would look free.
- **Content-addressed artifact tags.** Tagging by artifact digest makes
  republication idempotent only if rebuilding reproduces the bytes, which the
  live repositories rule out. Tagging by input fingerprint removes the
  operator-facing version, which cluster configuration pins and which carries
  the payload version.
- **The effective-input fingerprint of #763 alone.** Hashing an allowlist of
  resolved mkosi settings, the builder image and the package transaction adds
  a third list that can drift from what the build reads. Once builds are
  reproducible the payload digest is the necessary and sufficient comparison,
  and those components are useful as the explanation printed when it differs.

## Owner

`internal/extensionrevision` owns the decision. Workflows and scripts call it
through the CI planner and act on its answer; none of them matches paths,
compares versions or queries the registry for intent.

```go
// State is one extension at one commit.
type State struct {
    Name       string
    Present    bool   // extensions/<name>/extension.env exists
    Version    string // KATL_EXTENSION_ARTIFACT_VERSION
    Inputs     string // fingerprint of the build inputs
    Validation string // fingerprint of the validation inputs
}

func Read(repo, revision, name string) (State, error)

// Decide compares a candidate with the commit it is applied to.
func Decide(base, head State) Decision

// Publication reports whether head's version still has to be published.
func Publication(ctx context.Context, registry Registry, head State) (Intent, error)
```

`Read` accepts only plain `KEY=value` lines in `extension.env` and fails on
anything else, so the file means the same to it as to a shell that sources it.
A definition that does not parse is a `Read` error, which the caller rejects.
Workflows take the name, versions and runtime interface from the planner's
output instead of sourcing the file again.

`Decide` is total. It returns the first answer whose condition holds.

| Order | Condition | Answer | Meaning |
| --- | --- | --- | --- |
| 1 | Head has no definition | `None` | No action. |
| 2 | Base has no definition | `NewVersion` | Build and check the candidate once. |
| 3 | The version changes without advancing | `Invalid` | Reject with the reason. |
| 4 | The version advances | `NewVersion` | Build and check the candidate once. |
| 5 | Same version, equal `Inputs`, equal `Validation` | `Unchanged` | Nothing to build. |
| 6 | Same version, equal `Inputs`, different `Validation` | `Revalidate` | Build and check the candidate once. No version is required. |
| 7 | Same version, different `Inputs` | `Unproven` | Bytes may have changed. The caller must prove they did not, or the change needs a new version. |

A version advances when its payload version increases, or its `katl.N`
revision increases under the same payload version. This ordering keeps a
change from returning to a tag an earlier version already used, without a
registry query. It is the revision-advance check, and it has no other
implementation.

Reverts roll forward for the same reason. Restoring an earlier version is
`Invalid`, and restoring earlier inputs under the current version is
`Unproven`, so a revert of an extension change carries a new, higher version.
Both errors say so.

### Declared inputs

The owner declares two classes of input for each extension.

**Build inputs** can change the payload. Until `Unproven` can be proven, the
set is an enumerated superset of today's gate pattern: `Containerfile.mkosi`,
`mkosi.conf`, `extensions/<name>/`, `mkosi.profiles/system-extension-<name>/`,
`mkosi.profiles/runtime/`, `internal/systemdunit/units/`,
`scripts/build-system-extension`, `scripts/fedora-release` and `scripts/mkosi`,
excluding Go tests. Once the proof exists the set should widen to everything
that can reach the payload, because a wider set then costs a build rather than
a version.

**Validation inputs** cannot change the payload but decide whether it is
checked and packed correctly: `scripts/check-system-extension`,
`internal/sysextimage/`, `internal/installer/payloadbundle/`,
`internal/installer/systemextensionbundle/`,
`cmd/katl-mkosi-artifacts/extract_sysext.go`,
`cmd/katlctl/system_extension.go` and the gate's own workflow job. A change
to them yields a build and check with no version requirement.

Both fingerprints come from the shared tree digest that #915 extracts from
`cmd/katl-vmtest-plan/driver_inputs.go`. That issue first pins the current
driver digests with a golden test, then moves the mechanism, parameterised by
domain label, digest version, flavour salt, the per-flavour lock appended to
the shared set, the Markdown exclusion and the revision to read. Driver
qualification's digest is the same mechanism and keeps its values. It is not
the same decision. Driver qualification asks whether test evidence exists for
a digest, keeps its records as commit statuses, and its consequence is
skipping a job. Its input list, record lookup and selection stay where they
are.

## Callers

The planner resolves the base for each event, as it already does for path
selection, and emits one action per extension. Workflows run the action.

| Caller | Revisions | Action on the answer |
| --- | --- | --- |
| Pull request gate | Pull request base and merge commit, for pull requests into `main` | As the merge-group gate. Runs inside `ci.yml`. |
| Merge-group gate | `merge_group.base_sha` and the group head | `None` and `Unchanged`: no job. `NewVersion` and `Revalidate`: build, run `scripts/check-system-extension`, pack. `Unproven`: prove or reject. `Invalid`: reject. The job is needed by the `CI` result, so its failure blocks the merge. |
| Publication on `main` | Pushed head and the registry | `Publication` says publish when the tag is absent: build once, check, publish. When the tag exists it verifies the published artifact and builds nothing. |
| Manual publication | Selected commit and the registry | The same `Publication` call. It refuses a commit that is not an ancestor of `origin/main`. An existing tag is reported, never replaced. |

An `Unproven` candidate is rejected until #841 lands, with an error that names
the version to advance. Afterwards the gate builds both revisions in one job
and compares their artifact identities. Equal digests pass. Different digests
reject with the differing resolved settings and package transactions as the
explanation.

### Publication

Publication runs on every push to `main` without a `paths` filter. The registry
lookup costs seconds, and a filter would be a second statement of the inputs.
`Publication` reuses `payloadbundle`'s registry client and tag resolution
rather than adding a second one. The lookup retries through `internal/retry`
and fails the job when the registry cannot answer; the next push converges.

Pushes and manual runs share one concurrency group per extension with
`cancel-in-progress: false`, and call `Publication` only after entering it, so
two runs never build the same absent tag. GitHub keeps only the newest waiting
run in a group, which is sufficient because intent derives from the head. A
publish that finds the tag created since its lookup verifies that tag and
succeeds instead of failing on a differing digest.

When the tag exists, `Publication` verifies the manifest and its blobs, reads
the published revision from the manifest, recomputes `Read` for that revision
with the current code and requires `Decide(published, head)` to be
`Unchanged` or `Revalidate`. Anything else fails with "inputs changed since
`<revision>` without a new version", without building. Once `Unproven` can be
proven it is an expected answer here, and the check reports it as a notice.

## Published record

The published manifest carries two annotations beside the existing version
annotation: `org.opencontainers.image.revision`, the commit it was built from,
and `dev.katl.system-extension.inputs`, that commit's input fingerprint.
`systemextensionbundle` writes them because it owns the bundle's annotations.

The tag's existence is what prevents a rebuild, and the revision is what the
publication check reads. The stored fingerprint is informational: comparing it
with a fingerprint computed by later code would fail whenever the declared set
or the digest version changes. Tags published before the annotations exist are
reported as unrecorded.

## Coverage

| Extension | Version on `main` | Contract |
| --- | --- | --- |
| BIRD and later `extension.env` definitions | `KATL_EXTENSION_ARTIFACT_VERSION` | This document. |
| DRBD9 and NVIDIA | None; released under the KatlOS version | No revision decision. Driver qualification uses the shared tree digest for its evidence. |
| Kubernetes sysext | One immutable artifact per upstream version | No revision decision. `ci.yml` selects its validation build with `kubernetesrelease.RecipeChanged`, a path diff, which #916 replaces with a fingerprint comparison. |

## Implementation

Each step merges on its own and leaves `main` working. Only the equivalence
proof waits for #841.

| Issue | Blocked by | Scope |
| --- | --- | --- |
| #915 | | Pin the current driver digests with a golden test, then move the tree digest to a shared package with the parameters above. |
| #763, first part | #915 | Add `internal/extensionrevision` with `Read`, `Decide`, the version ordering and both declared classes, and the planner output. Replace the shell revision step in `system-extensions.yml` with a call to the owner in the same advisory position. The push step and `KATL_BIRD_RECIPE_PATTERN` stay. `Unproven` rejects. |
| #879 | #763, first part | Move the gate into `ci.yml` as a job needed by the `CI` result, on pull requests into `main` and merge groups. Add `Publication` with verification of an existing tag, the concurrency group, the ancestry check for manual runs, and the annotations. |
| #880 | #879 | Make `Publication` the only source of publish intent. Remove the `paths` filters, the head-commit diff, `KATL_BIRD_RECIPE_PATTERN` and its `scriptstest`. Test a push of several commits in which only an earlier one advances the version. |
| #763, second part | #841 | Add the proof for `Unproven`, reusing the same-metadata base build from the prototype branch with the payload digest as the comparison and the resolved settings and packages as diagnostics. Widen the build inputs and turn the publication check's `Unproven` into a notice. |
| #916 | #915 | Replace `RecipeChanged` with a fingerprint comparison. Not required by the issues above. |

## Open questions

- Fedora updates never reach a published version, by requirement 1. Whether a
  scheduled build should compare `main`'s resolved packages with the published
  transaction and prompt a new version is undecided.
- After the proof exists, nothing re-examines `main` against the published
  artifact except the gate's own history. A scheduled proof from the published
  revision to `main` would audit it at the cost of two builds per run.
- Whether the Kubernetes bundle's unused `verify-recipe` and
  `refresh-rebuilds` commands are removed with #916 or kept for the legacy
  manifest.
- Whether an extension's overlay contains package database state from the base
  tree. If it does, kernel and runtime package changes alter its bytes, and
  the widened build inputs must include the kernel locks and `packages/`.
