# Release State Machine

Status: decided, not yet implemented. Until the release action
replaces it, [Release KatlOS](../development/releases.md) describes the
procedure in force.

One GitHub workflow, **Release** (`release.yml`), is dispatched on a release
branch and takes the next step of that version's release. It tags and
publishes candidates without approval, and it publishes the final version
only from a job that waits for approval in the `release` environment. Nobody
pushes a release tag by hand.

This document defines how the action chooses its step. One Go function owns
the choice. `katl-release next` runs it in the workflow's decide job and
`katl-release status VERSION` runs it read-only from a checkout, so a dispatch
never does anything `status` did not predict from the same state.

## State is derived

The action stores nothing. Every dispatch reads the state below from Git and
GitHub and decides from it, so an interrupted run leaves nothing to reconcile.
A read that fails is an error: the command exits non-zero and reports no
state, because a missing answer must never be read as "absent".

The decision takes these inputs for a version `V`.

| Input | Definition |
| --- | --- |
| Branch | `release/V`. `V` is a complete final version in the [ADR-009](adrs/adr-009-katlos-calendar-versioning.md) grammar, `YYYY.M.PATCH`, with no suffix. `scripts/katl-release-artifacts version` owns the grammar. |
| `H` | The commit at the head of `release/V`, read when the decision is made. |
| `D` | The commit the run was dispatched at (`GITHUB_SHA`). Only the workflow supplies it. |
| Retry reason | The dispatch's `retry` input with surrounding whitespace removed. Empty means no retry. |
| Candidate tags | Every tag named `vV-rc.<suffix>`, with the commit it peels to. A valid suffix is a positive integer without a leading zero. `N` is the highest candidate number and `C` is the commit of `vV-rc.N`. |
| Final tag | `vV` and its commit `X`, when it exists. |
| Records | For each candidate tag, the record under `katl/release/vV-rc.<n>` on that tag's commit. For the final, the record under `katl/release/vV` on `C`. [Records](#records) defines them. `p` is the phase of `rc.N`'s record and `q` the phase of the final's. |
| Pre-release | The published GitHub release for `vV-rc.N`, with its pre-release flag and assets. |
| Final release | The published GitHub release for `vV`, with its pre-release flag and assets. |
| Active run | A run of `release.yml` on `release/V` whose status is `queued`, `in_progress`, `waiting` or `requested`, other than the run making the decision. A run that only waits for the concurrency group has status `pending` and does not count. If there are several, the oldest. |
| Approval pending | The active run has a pending deployment to the `release` environment. |

Tags of `V` with any other suffix are not inputs. The action neither reads nor
refuses them.

A draft release is not a publication and counts as absent. The repository has
immutable releases enabled, so a published release's assets and tag never
change afterwards. Publication therefore uploads to a draft and publishes it
last, and an interrupted publication leaves at most a draft, which the next
publication of that tag replaces.

A published release **matches** a record when all of these hold:

- its pre-release flag is set for a candidate and clear for the final;
- it has a `SHA256SUMS` asset whose GitHub asset digest equals the record's
  digest;
- its assets are exactly `SHA256SUMS` and the files that `SHA256SUMS` lists,
  each uploaded and with the asset digest that `SHA256SUMS` gives.

## Records

A record is a commit status. It is the only place the action writes an
outcome, and the state machine reads exactly this format.

| Field | Value |
| --- | --- |
| Commit | The commit the candidate or final is built from. |
| Context | `katl/release/vV-rc.<n>` for a candidate, `katl/release/vV` for the final. |
| Target URL | `<server>/<owner>/<repo>/actions/runs/<run id>/attempts/<attempt>`: the run attempt that built and qualified the bytes. A later run that only publishes those bytes writes the same URL, so the record always leads to the artifacts. |
| State and description | One of the four pairs below. |

| Phase | State | Description | Written |
| --- | --- | --- | --- |
| `building` | `pending` | `building` | Before the candidate's tag is created, or before the final build starts. |
| `failed` | `failure` | `failed` | When the build attempt ends without a successful **Release validation**. |
| `publishing` | `pending` | `publishing sha256:<digest>` | After **Release validation** succeeds and the bundle is verified, before anything is pushed or published. For the final, before the approval wait. |
| `published` | `success` | `published sha256:<digest>` | After the release is published and its `SHA256SUMS` has been read back and compared. |

`<digest>` is the SHA-256 of the bundle's `SHA256SUMS` file as 64 lowercase
hexadecimal digits. It equals the asset digest GitHub reports for the
published `SHA256SUMS`.

One build moves `building` to `failed`, or `building` to `publishing` to
`published`. A retry starts a new build at `building`. Nothing follows
`publishing` except `published` with the same digest. A job that cannot write
`failed`, for example because the run was cancelled, leaves `building`; the
state machine treats `building` with no active run exactly as `failed`.

### Trust

A status is a record only when it is trusted and well formed:

1. Its creator is the GitHub Actions bot: type `Bot`, login `github-actions`
   (`github-actions[bot]` in REST), database ID 41898282.
2. Its target URL has exactly the form above, on this server and repository.
3. The run attempt it names has that run ID and attempt number, belongs to
   this repository with this repository as its head, has `head_sha` equal to
   the status's commit, runs `.github/workflows/release.yml`, was started by
   `workflow_dispatch`, and has `head_branch` equal to `release/V`.
4. Its state and description form one of the four pairs.

The record for a context is the trusted, well-formed status with the highest
status ID. Statuses that fail the rule are ignored and `status` lists them as
warnings. Statuses cannot be deleted, so a status the action did not write
must not be able to change a decision or retire a version.

Steps 1 to 3 are the rule driver qualification already applies to its records
in `cmd/katl-vmtest-plan`. Both callers use one implementation, which takes
the expected workflow path, events and branch from its caller. Driver
qualification keeps its own check of the producing job. Records are written
with the workflow token, including in the approved job, because only that
token writes as the GitHub Actions bot.

## States

The rows are evaluated in order and the first whose guard holds is the state.
Each guard below is written for a reader who has already ruled out every
earlier row. The last row catches what is left, so the rows are exclusive and
exhaustive.

In messages, `<V>` is the version, `<N>` the highest candidate number, `<K>`
is `N + 1`, and `<H>`, `<C>`, `<X>` and `<D>` are the first 12 characters of
those commits. `<tag>`, `<url>` and `<reason>` are defined with the row.
`<outcome>` describes the record's build attempt: the names of its jobs that
concluded `failure` or `timed_out`, in listing order and joined by `, `,
followed by ` failed`; or `the run was cancelled` when no job failed and the
attempt concluded `cancelled`; otherwise `the run ended without qualifying`.

### The inputs are not a release the action made

These rows refuse and never act.

| # | State | Guard | Refusal |
| --- | --- | --- | --- |
| 1 | `not-release-branch` | The ref, or the argument to `status`, is not `release/<version>` with a complete final version. A series branch such as `release/2026.12.x` and a prerelease version are both refused. | `<ref> does not name one release. The release action runs only on release/<version>, where <version> is a complete final version such as 2026.12.1. Dispatch it on the branch of the version to release.` |
| 2 | `branch-missing` | `release/V` does not exist. | `release/<V> does not exist. Cut it from the selected main commit, then dispatch the release action on it.` |
| 3 | `branch-moved` | `D` is supplied and differs from `H`. | `release/<V> moved from <D> to <H> after this run was dispatched. Dispatch the release action again.` |
| 4 | `candidate-tag-invalid` | A tag `vV-rc.<suffix>` has an invalid suffix. `rc.0` is invalid. `<tag>` is the first such tag in name order. | `<tag> is not a candidate tag: candidates are v<V>-rc.1, v<V>-rc.2 and so on. Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 5 | `candidate-gap` | The candidate numbers are not exactly 1 to `N`. `<k>` is the lowest missing number and `<tag>` the lowest candidate tag above it. | `v<V>-rc.<k> is missing below <tag>: candidates are numbered from rc.1 without a gap. Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 6 | `tag-off-branch` | The commit of a candidate tag or of the final tag is neither `H` nor an ancestor of `H`. `<tag>` is the first such tag, candidates in number order and then the final, and `<commit>` its commit. | `<tag> is at <commit>, which is not on release/<V> (head <H>). Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 7 | `candidates-misordered` | For some `n`, the commit of `rc.<n>` is not a strict ancestor of the commit of `rc.<n+1>`. Two candidates on one commit are this case. `<a>` and `<b>` are the first such pair and `<ca>` and `<cb>` their commits. | `<a> at <ca> and <b> at <cb> are out of order: each candidate is on a later commit than the one before it, and no commit carries two. Nothing was changed. Have a repository administrator remove <b> if it was created by mistake, or abandon <V> and cut the next version.` |
| 8 | `candidate-foreign` | A candidate tag has no record. `<tag>` is the lowest such candidate and `<commit>` its commit. | `<tag> has no status katl/release/<tag> from a Release run on release/<V> at <commit>, so the release action did not create it. Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 9 | `final-foreign` | The final tag exists and one of these holds, taken in order. There is no candidate, or `X` is not `C`: `<reason>` is `the final goes on the commit of the latest candidate, and that is not <X>`. `p` is not `published`: `v<V>-rc.<N> at that commit was never published`. The final has no record on `X`: `it has no status katl/release/v<V> from a Release run`. `q` is `building` or `failed`: `its Release run did not qualify`. | `v<V> at <X> was not created by the release action: <reason>. Nothing was changed. Have a repository administrator remove v<V> if it was created by mistake, or abandon <V> and cut the next version.` |
| 10 | `release-contradicts-record` | A published release disagrees with its record. The cases are taken in order, first for `vV-rc.N` and then for `vV`; `<tag>` is the tag concerned. The pre-release exists and `p` is `building` or `failed`: `<reason>` is `it is published, and the Release run for <tag> never reached publication`. `p` is `published` and the pre-release is absent: `the record says it is published, and it does not exist`. `p` is `publishing` or `published` and the pre-release does not match the record: `it does not match the recorded sha256:<digest>` with the record's digest. The final tag is absent and `q` is `published`: `the record says it is published, and the tag v<V> does not exist`. The final tag exists, `q` is `published` and the final release is absent: `the record says it is published, and it does not exist`. The final tag exists and the final release is published and does not match the final's record: `it does not match the recorded sha256:<digest>`. | `The GitHub release <tag> disagrees with what the release action recorded: <reason>. Published bytes are never replaced. Nothing was changed. If the release was edited or deleted by mistake, undo that and dispatch again; otherwise abandon <V> and cut the next version.` |

### A run is already working

| # | State | Guard | Refusal |
| --- | --- | --- | --- |
| 11 | `approval-superseded` | Approval is pending, there is no final tag, there is a candidate and `H` is not `C`. `<url>` is the active run. | `v<V> is waiting for approval at <C>, and release/<V> has moved to <H>: <url>. Reject the approval; the next dispatch then tags v<V>-rc.<K> at the head. Approving it publishes nothing, because the publish job refuses a branch that moved.` |
| 12 | `approval-pending` | Approval is pending. | `v<V> is waiting for approval: <url>. Approve or reject it there.` |
| 13 | `run-active` | There is an active run. | `A Release run for <V> is in progress: <url>. Wait for it to finish, then run scripts/katl-release status <V>.` |

### The final tag exists

Row 9 has established that `X` is `C`, that `rc.N` is published and that the
final's record is `publishing` or `published`.

| # | State | Guard | Action or refusal |
| --- | --- | --- | --- |
| 14 | `final-incomplete` | `q` is `publishing` and the retry reason is empty. The final release is absent, or it is published and matches. `H` is not consulted: a tagged final is completed wherever the branch is. | `resume-final`: behind the same approval, publish the bytes of the recorded build attempt, then write `published`. |
| 15 | `released` | `q` is `published` and `H` is `X`. `<url>` is the final release. | `v<V> is released: <url>. There is nothing left to do for <V>.` |
| 16 | `released-branch-ahead` | `q` is `published` and `H` is not `X`. | `v<V> is released at <X>, and release/<V> has moved on to <H>. A released version never changes; cut the next version for the later commits.` |

### No final tag

| # | State | Guard | Action or refusal |
| --- | --- | --- | --- |
| 17 | `first-candidate` | There is no candidate tag and the retry reason is empty. | `candidate`: tag `vV-rc.1` at `H`, build, qualify and publish it as a pre-release. |
| 18 | `superseded` | `H` is not `C` and the retry reason is empty. The candidate's phase is not consulted: a new commit supersedes a green candidate, a failed one and one whose publication never finished. | `candidate`: tag `vV-rc.<K>` at `H`, build, qualify and publish it as a pre-release. |
| 19 | `candidate-failed` | `H` is `C`, `p` is `building` or `failed`, and the retry reason is empty. `<url>` is the record's target. | `v<V>-rc.<N> did not qualify at <C>: <outcome> (<url>). Push a fix to release/<V>; the next dispatch then tags v<V>-rc.<K>. If the source is not at fault, dispatch again with a retry reason to rebuild v<V>-rc.<N>.` |
| 20 | `candidate-retry` | `H` is `C`, `p` is `building` or `failed`, and there is a retry reason. | `retry-candidate`: rebuild and requalify `vV-rc.N` at `C` under the existing tag, then publish it. |
| 21 | `candidate-incomplete` | `H` is `C`, `p` is `publishing` and the retry reason is empty. The pre-release is absent, or it is published and matches. | `resume-candidate`: publish the bytes of the recorded build attempt, then write `published`. Never rebuild. |
| 22 | `final-start` | `H` is `C`, `rc.N` is green, the final has no record on `C`, and the retry reason is empty. | `final`: build and qualify `V` at `C`, wait for approval, tag `vV` at `C` and publish. |
| 23 | `final-failed` | `H` is `C`, `rc.N` is green, `q` is `building` or `failed`, and the retry reason is empty. `<url>` is the final record's target. | `v<V> did not qualify at <C>: <outcome> (<url>). v<V>-rc.<N> is still green. If the source is at fault, push a fix to release/<V>; the next dispatch then tags v<V>-rc.<K>. Otherwise dispatch again with a retry reason to rebuild v<V>.` |
| 24 | `final-retry` | `H` is `C`, `rc.N` is green, `q` is `building` or `failed`, and there is a retry reason. | `retry-final`: as `final`, recording the reason. |
| 25 | `final-unapproved` | `H` is `C`, `rc.N` is green, `q` is `publishing` and the retry reason is empty. The earlier final qualified and published nothing: its approval was rejected or timed out, or its run ended before the tag. | `final`: as row 22. |
| 26 | `retry-not-applicable` | Everything else, which is a retry reason in the state of row 14, 17, 18, 21, 22 or 25. `<step>` is `finish publishing v<V>` for row 14, `tag v<V>-rc.1 at <H>` for 17, `tag v<V>-rc.<K> at <H>` for 18, `finish publishing v<V>-rc.<N>` for 21, and `build v<V> at <C> for approval` for 22 and 25. | `There is nothing to retry for <V>: a dispatch without a retry reason would <step>. Dispatch again without one.` |

Rows 19 and 23 do not judge whether a failure was external. That judgement
follows the repository's flake rule, and whoever dispatches the retry records
it as the reason.

## Actions

`katl-release next` exits zero whenever it reaches a decision, including a
refusal, and prints the decision for the workflow. The decide job fails the
run on a refusal and shows the message in the run summary.

| Action | Parameters | What the workflow does |
| --- | --- | --- |
| `candidate` | candidate version, commit `H` | Write `building`, create the tag, build and qualify, write `publishing`, publish, write `published`. |
| `retry-candidate` | candidate version, commit `C`, reason | The same without creating the tag. The reason goes in the run summary. |
| `resume-candidate` | candidate version, commit `C`, build run and attempt, digest | Download that attempt's bundle, verify it against the digest, publish, write `published`. |
| `final` | version, commit `C`, candidate version | Write `building`, build and qualify, write `publishing`, then run the publish job. |
| `retry-final` | the same and the reason | As `final`. The reason goes in the run summary. |
| `resume-final` | version, commit `X`, build run and attempt, digest | Run the publish job on that attempt's bundle. |
| `refuse` | state, message | Fail the run. |

`status` prints the state's name, the action or the refusal, the candidate's
record and digest, the final's record, and a pending approval with its URL.
From a checkout it can observe a state between two steps of an active run;
row 13 covers that, and the decide job's reading is the authoritative one.

## Invariants

**Never two candidate tags on one commit.** Only rows 17 and 18 create a tag.
Row 17 has no candidate to collide with, row 18 requires `H` to differ from
`C`, and rows 6 and 7 have established that every earlier candidate is a
strict ancestor of `C`. A retry reuses its tag.

**The final tag goes only on a commit that carries a green candidate.** Rows
22, 24 and 25 are the only ones that lead to a new final tag, and each
requires `rc.N` green with `C` equal to `H`. The publish job checks it again
after approval.

**A new commit always moves the release forward.** With no final tag, a head
ahead of `C` leads to the next candidate whatever phase `rc.N`'s record is
in, so nothing the action itself leaves behind can block a version that has a
fix. Only the refusals of rows 1 to 10 can, and those describe state the
action did not produce.

**The record comes before the act.** `building` is written before the
candidate's tag, so a tag the action created always has a record. `publishing`
is written before the first push or publication, so a record in `building` or
`failed` proves nothing of that build is public. In the publish job the final
tag is created before anything else is made public, so a missing final tag
proves nothing of the final is public.

**Retry never touches published bytes.** Retry exists only in rows 20 and 24,
where the record is `building` or `failed`. From `publishing` onwards the only
action is to publish the recorded build attempt's bytes again, which is
idempotent. If that attempt's artifacts have expired, resuming fails and says
so; a candidate is then superseded by a new commit, and a tagged final has to
be abandoned for the next version. Artifact retention for the final build
must therefore exceed the environment's approval timeout.

**A run acts only on its own attempt's decision.** Re-running failed jobs
keeps the decide job's earlier output and would rebuild without a decision or
a recorded reason. The acting jobs therefore run only when the decide job ran
in the same run attempt. Re-running all jobs decides again and is equivalent
to a new dispatch.

**One run at a time per branch.** The workflow's concurrency group is
`release-<branch ref>` with `cancel-in-progress: false`, and the decide job
runs inside it, so a queued dispatch decides from the state the earlier run
left. GitHub keeps only the newest waiting run of a group and cancels older
waiting ones before they decide, so they change nothing. The group's name
must differ from the group `release-artifacts.yml` computes when called, or
the caller would wait on itself.

**A run waiting for approval holds the group.** A dispatch made meanwhile
waits behind it until the approval is granted, rejected or times out. Row 11
tells the reader to reject an approval that a new commit has superseded.

## Green candidate

`rc.N` at `C` is **green** when:

1. its record is `published`, and
2. its pre-release is published and matches the record.

`katl-release next` checks both and nothing more. They are sufficient because
the candidate workflow writes `published` only after it has established the
rest on the bytes in hand:

- **Release validation** succeeded in the build attempt: unit checks, the
  bundle, Production VM on both flavours, the qualified-bytes comparison, and
  VM regression either successful or skipped on the existing rule that the
  merge queue produced this exact commit and ran every VM job of both
  flavours;
- every file of the bundle matches its `SHA256SUMS` and has an attestation
  from the release build for `C` on `release/V`;
- the published `SHA256SUMS`, read back, equals the bundle's.

A green candidate is **promotable** when also `C` is `H` and no final tag
exists. Green alone does not imply that: row 18 supersedes a green candidate.

## Releasable final

The final build at `C` rebuilds the bytes, because they embed the version,
and skips VM regression on the evidence of the green candidate at the same
commit. It may be published when all of these hold. The publish job checks
them after approval and before it creates the tag, on the bytes it is about
to publish.

1. Deciding again, with an empty retry reason, still gives `final` for `C`,
   or `resume-final` for `X` when resuming. This re-reads the branch head,
   the tags and the candidate, so a branch that moved during the approval
   wait fails here with row 3's message and nothing is published.
2. **Release validation** succeeded in the final's build attempt.
3. Every file of the bundle `katl-<V>-x86_64-<first 12 of C>` matches the
   bundle's `SHA256SUMS`, and the digest of that `SHA256SUMS` equals the
   digest in the final's `publishing` record.
4. The per-flavour checksums that Production VM qualified equal the
   bundle's, with only `RELEASE_NOTES.md` exempt.
5. Every file has an attestation from the release build for `C` on
   `release/V`.
6. The extension publication inputs of both flavours match the bundle.

The job then creates `vV` at `C` with the `katl-release` App's token,
publishes the extension bundles and the release, reads the published
`SHA256SUMS` back, compares it, and writes `published`. Final tags, `v*`
without a suffix, can be created only by that App, and its key is a secret of
the `release` environment, so no other job can create a final tag or its
release.

`katl-release next` chooses the final from the green and promotable checks
alone. It cannot check 2 to 6 because the final's bytes do not exist when it
decides.
