# Extension Revision Ownership

Status: decided design; implementation tracked in #763, #879 and #880.

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
4. Every version that reaches `main` is published once, whether one push lands
   one pull request or five, and whether or not an earlier publication attempt
   failed.
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
committed tree: paths, modes and content, with projections that drop settings
that cannot affect output. It is a pure function of a commit, so it is cheap,
reproducible and comparable across time. It over-approximates: equal
fingerprints mean the extension cannot have changed, and different
fingerprints mean it may have.

**The artifact identity** is the packed manifest digest of a build. It settles
whether a possible change is a real one, by building the base and the
candidate in one job against the same repository metadata and comparing the
two digests. This is the comparison #763 prototypes, and it depends on #841.

The gate compares a candidate with the commit it is applied to rather than
with the registry. A version is reserved when it reaches `main`, not when its
publication completes, so a later change cannot reuse a version whose
publication is still in progress or has failed. Because every merge passes the
gate, the inputs behind a declared version are constant from the commit that
introduced it, and `main` needs no stored record of what was accepted.

Publication derives its intent from the registry instead of from a diff: a
version is published when its tag does not exist. The pushed range is
therefore irrelevant, a published tag is never rebuilt, and a version whose
publication failed is published by the next push.

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
  reproducible the packed digest is the necessary and sufficient comparison,
  and those components are useful as the explanation printed when it differs.

## Owner

`internal/extensionrevision` owns the decision. Workflows and scripts call it
through the CI planner and act on its answer; none of them matches paths,
compares versions or queries the registry for intent.

```go
// State is one extension at one commit.
type State struct {
    Name        string // extensions/<name>
    Version     string // KATL_EXTENSION_ARTIFACT_VERSION
    Fingerprint string // input fingerprint, versioned like driver digests
}

func Read(repo, revision, name string) (State, error)

// Decide compares a candidate with the commit it is applied to.
func Decide(base, head State) Decision

// Publication reports whether head's version still has to be published.
func Publication(ctx context.Context, registry Registry, head State) (Intent, error)
```

`Decide` returns one of four answers.

| Answer | Condition | Meaning |
| --- | --- | --- |
| `Unchanged` | Fingerprints are equal | Nothing to build. |
| `NewVersion` | Fingerprints differ and the version advances | Build and check the candidate once. |
| `Unproven` | Fingerprints differ under the same version | Bytes may have changed. The caller must prove they did not, or the change needs a new version. |
| `Invalid` | The version changes without advancing, or the definition does not parse | Reject with the reason. |

A version advances when its payload version increases, or its `katl.N`
revision increases under the same payload version. This ordering keeps a
change from returning to a tag an earlier version already used, without a
registry query. It is the revision-advance check, and it has no other
implementation.

The fingerprint is computed by one shared mechanism, `internal/buildinputs`,
extracted from `cmd/katl-vmtest-plan/driver_inputs.go`: a versioned digest over
a declared set of committed tree entries, following in-tree symlinks and
applying per-path projections. Driver qualification's digest is the same
mechanism and moves onto it with its values unchanged. It is not the same
decision. Driver qualification asks whether test evidence exists for a digest,
keeps its records as commit statuses, and its consequence is skipping a job.
Its input list, record lookup and selection stay where they are.

The declared inputs of a system extension are its definition
(`extensions/<name>/extension.env`), its profile
(`mkosi.profiles/system-extension-<name>/`), the runtime profile that supplies
its base tree, the root mkosi configuration, the builder container, and
`scripts/mkosi`, `scripts/fedora-release` and `scripts/build-system-extension`.
Documentation and tests are excluded. While `Unproven` is a rejection the set
lists only paths with a build role, as the gate pattern does today. Once
`Unproven` can be proven it should widen to everything that can reach the
packed bytes, including the bundle packer, because a wider set then costs a
build rather than a version.

## Callers

The planner resolves the base for each event, as it already does for path
selection, and emits one action per extension. Workflows run the action.

| Caller | Revisions | Action on the answer |
| --- | --- | --- |
| Pull request gate | Pull request base and merge commit | As the merge-group gate. Runs inside `ci.yml`. |
| Merge-group gate | `merge_group.base_sha` and the group head | `Unchanged`: no job. `NewVersion`: build, run `scripts/check-system-extension`, pack. `Unproven`: prove or reject. `Invalid`: reject. The job is needed by the `CI` result, so its failure blocks the merge. |
| Publication on `main` | Pushed head and the registry | `Publication` says publish when the tag is absent: build once, check, publish. When the tag exists it verifies the published manifest and its blobs and builds nothing. |
| Manual publication | Selected ref and the registry | The same `Publication` call. An existing tag is reported, never replaced. |

An `Unproven` candidate is rejected until #841 lands, with an error that names
the version to advance. Afterwards the gate builds both revisions in one job
against the same repository metadata and compares the packed digests. Equal
digests pass. Different digests reject with the differing resolved settings
and package transactions as the explanation.

Publication runs on every push to `main` without a `paths` filter. The registry
lookup costs seconds, and a filter would be a second statement of the inputs.
The lookup retries through `internal/retry` and fails the job when the
registry cannot answer; the next push converges. When one push lands two
versions, only the head's version is published. The skipped version was never
visible, so nothing depends on it.

## Published record

The published manifest carries two annotations beside the existing version
annotation: `org.opencontainers.image.revision`, the commit it was built from,
and `dev.katl.system-extension.inputs`, that commit's input fingerprint.
`systemextensionbundle` writes them because it owns the bundle's annotations.

The tag's existence is what prevents a rebuild. The annotations make the
artifact traceable to its source and let publication check the invariant the
gate maintains. While `Unproven` is a rejection, a push whose head fingerprint
differs from the published one under the same version fails with "inputs
changed since `<revision>` without a new version", without building. Once
equivalence can be proven a difference is expected, and the check reports it
as a notice. Tags published before the annotations exist are reported as
unrecorded.

## Coverage

| Extension | Version on `main` | Contract |
| --- | --- | --- |
| BIRD and later `extension.env` definitions | `KATL_EXTENSION_ARTIFACT_VERSION` | This document. |
| DRBD9 and NVIDIA | None; released under the KatlOS version | No revision decision. Driver qualification uses the shared fingerprint mechanism for its evidence. |
| Kubernetes sysext | One immutable artifact per upstream version | No revision decision. `ci.yml` selects its validation build with `kubernetesrelease.RecipeChanged`, a path diff, which should become a fingerprint comparison through `internal/buildinputs`. |

## Implementation

The order is: the shared mechanism, then the owner and gate, then publication.
Only the equivalence proof waits for #841.

| Issue | Scope |
| --- | --- |
| New: extract the input digest | Move the tree digest from `driver_inputs.go` to `internal/buildinputs`. Driver digests and their version stay byte-identical, shown by a test on a fixed tree. |
| #763, first part | Add `internal/extensionrevision` with `Read`, `Decide`, the version ordering and the declared inputs. The planner emits the extension actions. Remove `KATL_BIRD_RECIPE_PATTERN` and the shell revision step. `Unproven` rejects. |
| #879 | Run the gate as a `ci.yml` job needed by the `CI` result, on pull requests and merge groups, and remove the `pull_request` trigger from `system-extensions.yml`. Publication verifies an existing tag instead of rebuilding it, and writes and checks the annotations. |
| #880 | Add `Publication` and make it the only source of publish intent for pushes and manual runs. Remove the push `paths` filter and the head-commit diff. Test a push of several commits in which only an earlier one advances the version. |
| #763, second part, after #841 | Add the proof for `Unproven`, reusing the same-metadata base build from the prototype branch with the packed digest as the comparison and the resolved settings and packages as diagnostics. Widen the declared inputs and turn the publication check into a notice. |
| New: Kubernetes validation selection | Replace `RecipeChanged` with a fingerprint comparison. Not required by the issues above. |

## Open questions

- Fedora updates never reach a published version, by requirement 1. Whether a
  scheduled build should compare `main`'s resolved packages with the published
  transaction and prompt a new version is undecided.
- After the proof exists, nothing re-examines `main` against the published
  artifact except the gate's own history. A scheduled proof from the published
  revision to `main` would audit it at the cost of two builds per run.
- Whether the Kubernetes bundle's unused `verify-recipe` and
  `refresh-rebuilds` commands are removed with the validation-selection change
  or kept for the legacy manifest.
- Whether an extension's overlay contains package database state from the base
  tree. If it does, kernel and runtime package changes alter its bytes, and
  the widened input set must include the kernel locks and `packages/`.
