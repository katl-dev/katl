# Release State Machine

The **Release** workflow implements this design. The legacy tag-triggered
procedure is documented in [Release KatlOS](../development/releases.md).

One GitHub workflow, **Release** (`release.yml`), is dispatched on a release
branch and takes the next step of that version's release. It tags and
publishes candidates without approval, and it publishes the final version
only from a job that waits for approval in the `release` environment. Nobody
pushes a release tag by hand.

This document defines how the action chooses its step. One Go function owns
the choice. `katl-release next` runs it in the workflow's decide job and
`katl-release status VERSION` runs it read-only from a checkout. The decide
job runs the release branch's own `katl-release`, so `status` predicts a
dispatch only when it runs from a checkout of that branch's head.

## State is derived

The action stores no state of its own. Every dispatch reads the state below
from Git and GitHub and decides from it, so an interrupted run leaves nothing
to reconcile. A read that fails is an error: the command exits non-zero and
reports no state, because a missing answer must never be read as "absent".

The decision takes these inputs for a version `V`.

| Input | Definition |
| --- | --- |
| Branch | `refs/heads/release/V`, where `V` is a complete final version: a version of the [ADR-009](adrs/adr-009-katlos-calendar-versioning.md) grammar with no prerelease suffix. One Go package, `internal/releaseversion`, owns that grammar. The state machine, `cmd/katl-release` and `scripts/katl-release-artifacts` all call it, the script through a Go command, and none of them keeps a pattern of its own. |
| `H` | The commit at the head of `release/V`, read when the decision is made. |
| `D` | The commit the run was dispatched at (`GITHUB_SHA`). Only the workflow supplies it. |
| Retry reason | The dispatch's `retry` input with surrounding whitespace removed. Empty means no retry. |
| Candidate tags | Every tag named `vV-rc.<suffix>`, with the commit it peels to. A tag is valid when its name without the `v` is a candidate version of the grammar numbered 1 or higher. `N` is the highest candidate number and `C` is the commit of `vV-rc.N`. |
| Final tag | `vV` and its commit `X`, when it exists. |
| Records | For each candidate tag, the record under `katl/release/vV-rc.<n>` on that tag's commit. For the final, the record under `katl/release/vV` on `C`. [Records](#records) defines them. `p` is the phase of `rc.N`'s record and `q` the phase of the final's. |
| Pre-release | The published GitHub release for `vV-rc.N`, with its pre-release flag and assets. |
| Final release | The published GitHub release for `vV`, with its pre-release flag and assets. |
| Active run | A run of `release.yml` on `release/V` whose status is `queued`, `in_progress`, `waiting` or `requested`, other than the run making the decision. A run that only waits for the concurrency group has status `pending` and does not count. If there are several, the oldest. |
| Approval pending | The active run has a pending deployment to the `release` environment. |
| Extension references | The extension bundle references that exist for version `V` in the registry: for each release extension recipe (`extensions/<name>/recipe.json`) at `H`, the tags of the recipe's repository that name the final `V` (not a candidate), each with the manifest digest it resolves to. Release publication pushes to the same repositories. The tags are read anonymously through the registry API, not the GitHub Packages API. A repository the registry does not know, one whose anonymous pull token the registry refuses, or one without such a tag, holds no reference; a refusal from the registry API after a token was issued, or a failed read, is a read failure. |

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

A record is a commit status. It says what one build attempt reached, and the
state machine reads exactly this format.

| Field | Value |
| --- | --- |
| Commit | The commit the candidate or final is built from. |
| Context | `katl/release/vV-rc.<n>` for a candidate, `katl/release/vV` for the final. |
| Target URL | `<server>/<owner>/<repo>/actions/runs/<run id>/attempts/<attempt>`: the run attempt that builds the bytes and writes the record. |
| State and description | One of the three pairs below. |

| Phase | State | Description | Written |
| --- | --- | --- | --- |
| `building` | `pending` | `building` | Before the candidate's tag is created, or before the final build starts. |
| `failed` | `failure` | `failed` | When the build attempt ends before `qualified` is written. |
| `qualified` | `success` | `qualified sha256:<digest>` | After **Release validation** succeeds and the bundle is verified, before anything is pushed or published. For the final, before the approval wait. |

`<digest>` is the SHA-256 of the bundle's `SHA256SUMS` file as 64 lowercase
hexadecimal digits. It equals the asset digest GitHub reports for a published
`SHA256SUMS`.

Publication is not recorded. A candidate or a final is **published** when its
record is `qualified` and its published release matches the record, so the
release itself is the evidence and no later write can claim it. A release
matches its record when it has the record's pre-release flag, its
`SHA256SUMS` asset has the recorded digest and verifies with
`gh attestation verify` against `release-artifacts.yml` as called by the
record's run attempt, at the record's commit on `refs/heads/release/V`, and
its other assets are exactly the files that `SHA256SUMS` lists, each uploaded
with the digest it gives. A verification that `gh attestation verify` could not
complete, or that it completed and refused, fails the read: neither is
taken as a release that contradicts its record.

One attempt moves from `building` to `failed` or to `qualified`. A job that
cannot write `failed`, for example because the run was cancelled, leaves
`building`; the state machine treats `building` with no active run exactly as
`failed`. For a candidate, nothing follows `qualified`: no retry is allowed
and the only action left is to publish those bytes. For the final the same
holds once `vV` exists. Without the tag, a `qualified` final is an unapproved
build that published nothing, and the next final starts again at `building`.

One subcommand, `katl-release record`, renders and writes records from the Go
type the state machine parses. Workflow steps call it and never compose a
context, a description or a target URL themselves.

### Trust

A status is a record only when all of these hold:

1. Its creator is the GitHub Actions bot: type `Bot`, login `github-actions`
   (`github-actions[bot]` in REST), database ID 41898282.
2. Its target URL has exactly the form above, on this server and repository.
3. The run attempt it names has that run ID and attempt number, belongs to
   this repository with this repository as its head, has `head_sha` equal to
   the status's commit, runs `.github/workflows/release.yml`, was started by
   `workflow_dispatch`, and has `head_branch` equal to `release/V`.
4. Its state and description form one of the three pairs.
5. The named attempt did what the status says. A `qualified` status is a
   record only when that attempt's **Release validation** job concluded
   `success` and that attempt built the version the context names: it
   uploaded the bundle artifact
   `katl-<version>-x86_64-<first 12 of the commit>`. GitHub lists a run's
   artifacts without their attempt, so an attempt's artifacts are those
   created from its start until the next attempt of the run started; an
   attempt whose start, or whose next attempt's start, GitHub does not
   report fails the read rather than guessing the bounds. A
   `failed` status is a record only when that attempt completed without that
   success.

The trust rule does not verify the attestation of a `qualified` record's
`SHA256SUMS`, because `next` has those bytes only once they are published.
`next` verifies it where a release is published whose `SHA256SUMS` asset has
the digest of a qualified record of that tag on the tag's commit: it
downloads that asset and verifies it as the definition of a matching release
above requires. A verification that fails stops the decision, like any read
that fails. For a qualified record with no published release, rows 21 and 26,
`next` does not require the attestation; the acting job verifies the bytes
and their attestation before it publishes anything.

The lookup lists every status of the commit, following pagination, and not
only the latest status of each context. The record for a context is that of
the most recently started attempt that has any record, at the furthest phase
that attempt reached. A `qualified` record gives way to a later attempt only
for the final, and only while `vV` does not exist. A candidate's `qualified`
record and a tagged final's stand whatever is written afterwards; if several
attempts qualified, the most recently started of them holds the record.

Statuses that fail the rule are ignored, and `status` lists them as warnings.
Step 5 and the attestation checks are what keep a status from standing in
for work: bytes become publishable only through an attempt that validated and
attested them, whoever wrote the status. A `qualified` status that passes the
rule without an attestation behind it can make row 21 apply on its commit;
the acting job then refuses to publish, and the status holds row 21 on that
commit until a new commit clears it through row 17. Steps 1 to 4 alone do not establish that the named attempt
wrote a `building` status. The most such a status can do is make the action
refuse with row 19 or row 24, which a retry or a new commit clears, or make
row 19 apply in place of row 8 to a tag the action did not create, where a
retry builds and qualifies it like any candidate. No status can cause a
publication, replace published bytes, or stop a version for good.

Steps 1 to 3 are the rule driver qualification already applies to its records
in `cmd/katl-vmtest-plan`. Both callers use one implementation, which takes
the expected workflow path, events and branch from its caller. Driver
qualification keeps its own check of the producing job. Records are written
with the workflow token, because only that token writes as the GitHub Actions
bot.

## States

The rows are evaluated in order and the first whose guard holds is the state.
Each guard is written for a reader who has already ruled out every earlier
row, and repeats the earlier conditions it depends on. The last row catches
what is left, so the rows are exclusive and exhaustive.

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
| 1 | `not-release-branch` | The ref is not `refs/heads/release/<version>` with a complete final version, or the argument to `status` is not such a version. A series branch such as `release/2026.12.x`, a prerelease version, and a tag named `release/<version>` are all refused. | `<ref> does not name one release. The release action runs only on the branch release/<version>, where <version> is a complete final version such as 2026.12.1. Dispatch it on the branch of the version to release.` |
| 2 | `branch-missing` | `release/V` does not exist. | `release/<V> does not exist. Cut it from the selected main commit, then dispatch the release action on it.` |
| 3 | `branch-moved` | `D` is supplied and differs from `H`. | `release/<V> moved from <D> to <H> after this run was dispatched. Dispatch the release action again.` |
| 4 | `candidate-tag-invalid` | A tag `vV-rc.<suffix>` is not valid. `rc.0` is not valid. `<tag>` is the first such tag in name order. | `<tag> is not a candidate tag: candidates are v<V>-rc.1, v<V>-rc.2 and so on. Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 5 | `candidate-gap` | The candidate numbers are not exactly 1 to `N`. `<k>` is the lowest missing number and `<tag>` the lowest candidate tag above it. | `v<V>-rc.<k> is missing below <tag>: candidates are numbered from rc.1 without a gap. Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 6 | `tag-off-branch` | The commit of a candidate tag or of the final tag is neither `H` nor an ancestor of `H`. `<tag>` is the first such tag, candidates in number order and then the final, and `<commit>` its commit. | `<tag> is at <commit>, which is not on release/<V> (head <H>). Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 7 | `candidates-misordered` | For some `n`, the commit of `rc.<n>` is not a strict ancestor of the commit of `rc.<n+1>`. Two candidates on one commit are this case. `<a>` and `<b>` are the first such pair and `<ca>` and `<cb>` their commits. | `<a> at <ca> and <b> at <cb> are out of order: each candidate is on a later commit than the one before it, and no commit carries two. Nothing was changed. Have a repository administrator remove <b> if it was created by mistake, or abandon <V> and cut the next version.` |
| 8 | `candidate-foreign` | A candidate tag has no record. `<tag>` is the lowest such candidate and `<commit>` its commit. | `<tag> has no status katl/release/<tag> from a Release run on release/<V> at <commit>, so the release action did not create it. Nothing was changed. Have a repository administrator remove <tag> if it was created by mistake, or abandon <V> and cut the next version.` |
| 9 | `final-foreign` | The final tag exists and one of these holds, taken in order. There is no candidate, or `X` is not `C`: `<reason>` is `the final goes on the commit of the latest candidate, and that is not <X>`. `p` is not `qualified`: `v<V>-rc.<N> at that commit never qualified`. The final has no record on `X`, or `q` is not `qualified`: `no Release run on release/<V> qualified v<V> at that commit`. | `v<V> at <X> was not created by the release action: <reason>. Nothing was changed. Have a repository administrator remove v<V> if it was created by mistake, or abandon <V> and cut the next version.` |
| 10 | `final-contradicts-record` | The final tag exists and one of these holds, taken in order. No release is published for it: `<reason>` is `the tag exists and no release is published for it`. The published final release does not carry the final's recorded bytes: `it does not match the recorded sha256:<digest>`, with the record's digest. The published final release carries those bytes, but its `SHA256SUMS` is not attested by the record's run attempt: `the SHA256SUMS is not attested by the recorded run attempt <url>`. | `The GitHub release v<V> disagrees with what the release action recorded: <reason>. Published bytes are never replaced. Nothing was changed. If the release was edited or deleted by mistake, undo that and dispatch again; otherwise abandon <V> and cut the next version.` |

The action publishes the final tag and its release in one step, so row 10's
first case arises only when a published release was deleted afterwards. Its
tag cannot be used again, which is why the exit is the next version.

### A run is already working

| # | State | Guard | Refusal |
| --- | --- | --- | --- |
| 11 | `approval-superseded` | Approval is pending, there is no final tag, there is a candidate and `H` is not `C`. `<url>` is the active run. | `v<V> is waiting for approval at <C>, and release/<V> has moved to <H>: <url>. Reject the approval; the next dispatch then tags v<V>-rc.<K> at the head. Approving it publishes nothing, because the publish job refuses a branch that moved.` |
| 12 | `approval-pending` | Approval is pending. | `v<V> is waiting for approval: <url>. Approve or reject it there.` |
| 13 | `run-active` | There is an active run. | `A Release run for <V> is in progress: <url>. Wait for it to finish, then run scripts/katl-release status <V>.` |

### The final tag exists

Rows 9 and 10 have established that `X` is `C`, that both records are
`qualified` and that the final release is published and matches. `V` is
published.

| # | State | Guard | Refusal |
| --- | --- | --- | --- |
| 14 | `released` | The final tag exists and `H` is `X`. `<url>` is the final release. | `v<V> is released: <url>. There is nothing left to do for <V>.` |
| 15 | `released-branch-ahead` | The final tag exists and `H` is not `X`. | `v<V> is released at <X>, and release/<V> has moved on to <H>. A released version never changes; cut the next version for the later commits.` |

### No final tag

Every row below requires that no final tag exists. Rows 17 to 26 also require
a candidate: `C` is the commit of the highest, `vV-rc.N`, and `p` the phase
of its record. Rows 22 to 26 require `rc.N` to be published and take `q` from
the final's record on `C`.

| # | State | Guard | Action or refusal |
| --- | --- | --- | --- |
| 16 | `first-candidate` | No final tag, no candidate tag, and the retry reason is empty. | `candidate`: tag `vV-rc.1` at `H`, build, qualify and publish it as a pre-release. |
| 17 | `superseded` | No final tag, a candidate exists, `H` is not `C`, and the retry reason is empty. Neither the candidate's record nor its pre-release is consulted: a new commit supersedes a published candidate, a failed one, one whose publication never finished and one whose pre-release disagrees with its record. | `candidate`: tag `vV-rc.<K>` at `H`, build, qualify and publish it as a pre-release. |
| 18 | `prerelease-contradicts-record` | No final tag, a candidate exists, `H` is `C`, and one of these holds, taken in order. The pre-release is published and `p` is `building` or `failed`: `<reason>` is `it is published, and no Release run qualified v<V>-rc.<N>`. `p` is `qualified` and the published pre-release does not carry the recorded bytes: `it does not match the recorded sha256:<digest>`, with the record's digest. `p` is `qualified` and the published pre-release carries those bytes, but its `SHA256SUMS` is not attested by the record's run attempt: `the SHA256SUMS is not attested by the recorded run attempt <url>`. | `The GitHub pre-release v<V>-rc.<N> disagrees with what the release action recorded: <reason>. Published bytes are never replaced. Nothing was changed. Restore the pre-release flag if it was edited; otherwise push a commit to release/<V>, and the next dispatch tags v<V>-rc.<K>.` |
| 19 | `candidate-failed` | No final tag, a candidate exists, `H` is `C`, `p` is `building` or `failed`, and the retry reason is empty. `<url>` is the record's target. | `v<V>-rc.<N> did not qualify at <C>: <outcome> (<url>). Push a fix to release/<V>; the next dispatch then tags v<V>-rc.<K>. If the source is not at fault, dispatch again with a retry reason to rebuild v<V>-rc.<N>.` |
| 20 | `candidate-retry` | No final tag, a candidate exists, `H` is `C`, `p` is `building` or `failed`, and there is a retry reason. | `retry-candidate`: rebuild and requalify `vV-rc.N` at `C` under the existing tag, then publish it. |
| 21 | `candidate-incomplete` | No final tag, a candidate exists, `H` is `C`, `p` is `qualified`, no pre-release is published, and the retry reason is empty. | `resume-candidate`: publish the bytes of the recorded build attempt. Never rebuild. |
| 22 | `extensions-already-published` | No final tag, `H` is `C`, `rc.N` is published, and an extension reference for `V` exists. A final build would produce different bytes and could not publish them, so the action refuses before building, whatever the final's record and the retry reason. | `Extension bundles for <V> are already published with different contents. Published bytes are never replaced. Have a repository administrator delete those package versions if no release uses them and dispatch again; otherwise abandon <V> and cut the next version.` |
| 23 | `final-start` | No final tag, `H` is `C`, `rc.N` is published, the final has no record on `C`, and the retry reason is empty. | `final`: build and qualify `V` at `C`, wait for approval, then publish the tag and the release together. |
| 24 | `final-failed` | No final tag, `H` is `C`, `rc.N` is published, `q` is `building` or `failed`, and the retry reason is empty. `<url>` is the final record's target. | `v<V> did not qualify at <C>: <outcome> (<url>). v<V>-rc.<N> is still green. If the source is at fault, push a fix to release/<V>; the next dispatch then tags v<V>-rc.<K>. Otherwise dispatch again with a retry reason to rebuild v<V>.` |
| 25 | `final-retry` | No final tag, `H` is `C`, `rc.N` is published, `q` is `building` or `failed`, and there is a retry reason. | `retry-final`: as `final`, recording the reason. |
| 26 | `final-unapproved` | No final tag, `H` is `C`, `rc.N` is published, `q` is `qualified`, and the retry reason is empty. The earlier final qualified and published nothing: its approval was rejected or timed out, or its run ended before publication. | `final`: as row 23. |
| 27 | `retry-not-applicable` | Everything else, which is a retry reason in the state of row 16, 17, 21, 23 or 26. `<step>` is `tag v<V>-rc.1 at <H>` for row 16, `tag v<V>-rc.<K> at <H>` for 17, `finish publishing v<V>-rc.<N>` for 21, and `build v<V> at <C> for approval` for 23 and 26. | `There is nothing to retry for <V>: a dispatch without a retry reason would <step>. Dispatch again without one.` |

Rows 19 and 24 do not judge whether a failure was external. That judgement
follows the repository's flake rule, and whoever dispatches the retry records
it as the reason.

## Actions

`katl-release next` exits zero whenever it reaches a decision, including a
refusal, and prints the decision for the workflow. The decide job fails the
run on a refusal and shows the message in the run summary.

| Action | Parameters | What the workflow does |
| --- | --- | --- |
| `candidate` | candidate version, commit `H` | Write `building`, create the tag, build and qualify, write `qualified`, push the extension bundles, publish the pre-release. |
| `retry-candidate` | candidate version, commit `C`, reason | The same without creating the tag. The reason goes in the run summary. |
| `resume-candidate` | candidate version, commit `C`, build run and attempt, digest | Download that attempt's bundle, verify it against the digest, push the extension bundles, publish the pre-release. It writes no record. |
| `final` | version, commit `C`, candidate version | Write `building`, build and qualify, write `qualified`, then run the publish job. |
| `retry-final` | the same and the reason | As `final`. The reason goes in the run summary. |
| `refuse` | state, message | Fail the run. |

When `resume-candidate` finds that the recorded attempt's bundle has expired,
it fails with: `The bundle of v<V>-rc.<N> from <url> has expired, so its
publication cannot be finished. Push a commit to release/<V>; the next
dispatch then tags v<V>-rc.<K>.`

`status` prints the state's name, the action or the refusal, the candidate's
record and digest, the final's record, and a pending approval with its URL.
From a checkout it can observe a state between two steps of an active run;
row 13 covers that, and the decide job's reading is the authoritative one.

## Invariants

**Never two candidate tags on one commit.** Only rows 16 and 17 create a tag.
Row 16 has no candidate to collide with, row 17 requires `H` to differ from
`C`, and rows 6 and 7 have established that every earlier candidate is a
strict ancestor of `C`. A retry reuses its tag.

**The final tag goes only on a commit that carries a green candidate.** Rows
23, 25 and 26 are the only ones that lead to a final tag, and each requires
`rc.N` published with `C` equal to `H`. The publish job checks it again after
approval.

**A new commit always moves the release forward.** With no final tag, a head
ahead of `C` leads to the next candidate whatever `rc.N`'s record and
pre-release say, so nothing about a candidate can block a version that has a
fix. Only rows 1 to 10 can, and those describe tags and a final release that
the action did not produce.

**The record comes before the act.** `building` is written before the
candidate's tag, so a tag the action created always has a record. `qualified`
is written before the first push or publication, so a record in `building` or
`failed` proves nothing of that build is public.

**The final tag and its release appear together.** The publish job creates a
draft release for `vV` that targets `C`, uploads the bundle to it, pushes the
extension bundles, and publishes the draft last, which creates the tag. A
missing final tag therefore proves no final release is public, and an
existing one proves the release was published whole. No state has a final tag
waiting for its release, so the final is never resumed from an earlier run.
The legacy tag-triggered jobs skip tags made by the `katl-release` App
because Release has already qualified and published those versions.

**Retry never touches published bytes.** Retry exists only in rows 20 and 25,
where the record is `building` or `failed`. A `qualified` candidate is only
ever published from its recorded attempt, which is idempotent. A `qualified`
final without its tag is rebuilt, and check 7 of
[Releasable final](#releasable-final) keeps the rebuild from replacing
extension bundles an earlier attempt pushed.

**A run acts only on its own attempt's decision.** Re-running failed jobs
keeps the decide job's earlier output and would rebuild without a decision or
a recorded reason. An acting job whose decision comes from an earlier attempt
therefore fails with `This attempt has no decision of its own. Dispatch the
release action again.` Re-running all jobs decides again and is equivalent to
a new dispatch with the same inputs, including the same retry reason.

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
Artifact retention for the final build must exceed the environment's approval
timeout.

## Green candidate

`rc.N` at `C` is **green** when it is published:

1. its record is `qualified`, and
2. its pre-release is published and matches the record.

`katl-release next` checks both. Check 1 includes the trust rule, so `next`
itself confirms that **Release validation** succeeded in the recorded attempt,
and check 2 includes verifying that the published `SHA256SUMS` is attested by
that attempt's build. The
candidate workflow establishes the rest before it writes `qualified`:

- **Release validation** covers unit checks, the bundle, Production VM on
  both flavours, the qualified-bytes comparison, and VM regression either
  successful or skipped on the existing rule that the merge queue produced
  this exact commit and ran every VM job of both flavours;
- every file of the bundle matches its `SHA256SUMS` and has an attestation
  from the release build for `C` on `release/V`.

A green candidate is **promotable** when also `C` is `H` and no final tag
exists. Green alone does not imply that: row 17 supersedes a green candidate.

## Releasable final

After cutting `release/<version>`,
[check the release credential](../../cmd/katl-release/README.md#check-the-release-credential)
before dispatching a candidate or final release.

The final rebuilds the bytes at the candidate's commit `C` because the
bytes embed the version. The called build repeats the VM regression lanes
for both flavours on every candidate and final.

The final **Publish** job waits for approval in the `release`
environment. The run summary shows the candidate, commit, checksums and
rendered notes before review. After approval, the job uses the
`katl-release` App to draft and publish the final release and its tag.

The final may be published when all of these hold. The publish job checks
them after approval and before it creates anything, on the bytes it is about
to publish.

1. Deciding again, with an empty retry reason, still gives `final` for `C`.
   This re-reads the branch head, the tags and the candidate, so a branch
   that moved during the approval wait fails here with row 3's message and
   nothing is published.
2. The final's record is `qualified` and its target is this run attempt, so
   the job publishes only what its own run built and validated.
3. Every file of the bundle `katl-<V>-x86_64-<first 12 of C>` matches the
   bundle's `SHA256SUMS`, and the digest of that `SHA256SUMS` equals the
   record's digest.
4. The per-flavour checksums that Production VM qualified equal the
   bundle's, with only `RELEASE_NOTES.md` exempt.
5. Every file has an attestation from the release build for `C` on
   `release/V`.
6. The extension publication inputs of both flavours match the bundle.
7. No extension reference for `V` exists in the registry. If one does, the
   job fails with:
   `Extension bundles for <V> are already published with different contents.
   Published bytes are never replaced. Have a repository administrator delete
   those package versions if no release uses them and dispatch again;
   otherwise abandon <V> and cut the next version.`

The job then creates the draft release with the `katl-release` App's token,
uploads the bundle, pushes the extension bundles, confirms that the draft's
assets still have the digests `SHA256SUMS` gives, and publishes it with one
request that sets the tag `vV` and the target `C`; it then reads back the
tag's commit and the published `SHA256SUMS`.

Check 7 also runs before the final is built, as row 22: `next` refuses to
start a final while any extension reference for `V` exists. Both fire when a
reference exists before this attempt pushes one, which happens after an
earlier final attempt failed between pushing the extension bundles and
publishing the draft, or when a reference was created outside the action.

The environment requires approval and accepts only release branches.
A reviewer approves the workflow at the dispatched commit, so release
branch protection is part of the gate. Repository-wide enforcement also
requires the tag ruleset to reserve final tag creation to the
`katl-release` App.

`katl-release next` chooses the final from the green and promotable checks
and row 22. It cannot check 2 to 6 because the final's bytes do not exist
when it decides.

## Decisions

These are the choices the design makes where another reading was available.

- **A retry is an input, not a judgement.** The state machine never decides
  whether a failure was external. A failed or cancelled build at the head is
  one state, split only by whether a retry reason was given, and a retry
  reason where a plain dispatch would act is refused instead of ignored.
- **Records carry a phase and bind to what the attempt did.** A plain
  pending, success or failure could not tell a build that failed from one
  that qualified and was interrupted while publishing, and the extension
  bundles are pushed before the release exists. `qualified` is written
  before anything is public, and it counts only when the attempt it names
  validated and attested those bytes.
- **Publication is derived from the release.** The published release is
  immutable, so comparing it with the record is stronger than a status
  claiming it, and there is no record to forge or to lose.
- **The final has a record of its own.** Without one, a failed or unapproved
  final build could not be told from no final attempt.
- **A new commit supersedes every candidate state**, including a
  half-published candidate and one whose pre-release disagrees with its
  record. The alternative, finishing or repairing the old candidate first,
  could block a version that already has its fix.
- **The final is never resumed.** Tag and release appear in one step, so
  there is no tagged final with an incomplete release to finish, and no
  approved resume on a branch that has moved.
  The cost is that a failure between pushing the extension bundles and
  publishing the draft needs the cleanup that check 7 names.
- **An unapproved final is rebuilt without a retry reason.** A rejection is
  a decision, not a failure; nothing is public and the next attempt waits
  for approval again.
- **Green does not include "at the head".** A candidate stays green when the
  branch moves; whether it may be promoted is a separate condition.
- **The dispatch commit must still be the head**, at the decision and again
  after approval. Otherwise a push during the run or the approval wait would
  release a commit without the newer ones.
- **The record comes before the tag.** Writing it afterwards leaves a moment
  in which the action's own tag reads as foreign.
- **Immutable releases narrow "incomplete".** A published release cannot be
  completed later, so an unfinished publication is an absent or draft
  release, and a published release that does not match its record is refused.
- **Tags are checked as a set.** A tag off the branch, a branch behind its
  tags and a head that is not a descendant are one condition, and candidates
  must also be in ancestry order.
- **`next` verifies one attestation, not the release.** For a published
  release that carries a qualified record's `SHA256SUMS`, it verifies the
  attestation of that file and compares the release's asset digests with it.
  Verifying every asset's attestation needs the bytes and stays with the
  workflow, which also verifies before publishing a qualified build that
  `next` resumes or replaces without one.
- **Re-running failed jobs does not act.** It would rebuild on a stale
  decision with no recorded reason.
- **Candidates start at `rc.1`.** The grammar still accepts `rc.0`, and the
  action refuses a version that has one.

## To verify during implementation

The design assumes these facts about GitHub. The state machine and the
release workflow confirm each against the service before relying on it, and
a fact that does not hold reopens the part of the design that names it.

- Publishing a draft release with the App's token creates the reserved tag
  `vV` at the draft's target commit in the same request.
- An attestation's certificate identifies the run attempt that called
  `release-artifacts.yml`, which `next` compares with the record of a
  published release. `gh attestation verify --format json` reports it as
  `runInvocationURI`.
- Whether a published immutable release can be deleted, which decides
  whether the first case of row 10 can arise at all.
- The registry can list the extension references of a version with their
  digests, which row 22 and check 7 read. `ghcr.io` serves the tags of a
  public repository to an anonymous pull token, but its token endpoint
  refuses a repository that does not exist with the same refusal as a
  private one. The read counts that refusal as "not published", so a
  release extension that was never published does not block release
  decisions, and a private one is indistinguishable from it; publication
  still refuses to overwrite a tag with different content.
