# Agent roles

Every role follows the delivery, testing, and user-journey rules in
[AGENTS.md](../../AGENTS.md). GitHub is the shared record for scope, release
milestones, holds, dependencies, review, and journey results. Local state
tracks work that a new session needs to recover.

## Local state

Use `${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev` for coordination state.
Create the directory and its worktree root on first use. The coordinator reads
the state on startup. The orchestrator and Release agent update it as their
jobs start, finish, or change hands.

| Path | Contents |
| --- | --- |
| `state.md` | Active orchestrator and agent identifiers, open version milestones and their Release agent identifiers, issue and PR links, worktree paths, running unit names and log paths, queue watch IDs, and the next action and owner. |
| `logs/` | Codex job reports, retained across session and worktree cleanup. |
| `worktrees/` | Linked worktrees for scoped tasks and a trusted `origin/main` checkout for cleanup. |

Reconcile the checkpoint with running jobs and GitHub before acting on it.

## Coordinator

**Purpose.** Own the operator-facing session and give the orchestrator a single
delivery stream. Keep live-system changes and decisions requiring operator
authority in this session.

**Brief.** Start from the operator's goal and the current release or milestone.
Supply any constraints or authority that changes how the orchestrator can act.

**Model.** Use Claude Opus.

**Report.** Give the operator a brief update when a blocker resolves, a release
candidate is tagged or published, the final release is published, a permission
is denied, a failure remains unexplained after investigation, or related issues
require a redesign. For each queue ejection, give the PR, the reason, whether
the PR caused it or a flake did, and the fix issue when one is needed. Mark the
cause as unresolved while diagnosis is in progress. Send updates at these
events and when a decision needs the operator's authority.

**Hand-off.** From the Katl checkout, run `claude --agent katl-coordinator`.
Reconcile the local checkpoint and the live queue, then resume the active
Orchestrator or start one from the template in
[its agent definition](../../.claude/agents/katl-orchestrator.md). Route related
follow-ups to that Orchestrator as one scoped item.

**Scope and authority.** Use the agreed release scope and risk criteria to
decide sequencing, deferrals, and gates. Act within that authority and report
the reasons for material scope changes. Bring decisions to the operator when
the agreed policy and a sensible default cannot settle them, or when a
destructive or external action requires authority that has yet to be granted.

**Release hand-off.** Confirm the planned cut with the orchestrator. After the
cut, route release decisions and any explicit operator authority to the
[Release agent](#release).

## Orchestrator

**Purpose.** Own delivery on `main` from brief to merge. Decide scope,
sequencing, and whether the evidence supports approval. Own diff
review, commits, pushes, and PR and queue actions on `main`.
Delegate implementation, mechanical edits, rebases, conflict resolution,
independent review, diagnosis, and hands-on journeys.

**Brief.** Supply the goal and current release or milestone. Recover active
work from the local checkpoint and derive scope and dependencies from GitHub.
Carry forward the coordinator's constraints and any explicit queue authority.

**Model.** Use Claude Opus with high effort.

**Report.** Report to the coordinator if a blocker resolves, a permission is
denied, a failure cannot be root-caused, a redesign is needed, or a decision
requires operator authority. For each queue ejection, give the PR and reason
from `katl-queue why PR`, whether the PR caused it or a flake did, and the fix
issue when one is needed. Give the current evidence and next action if the
cause is still unknown.

**Hand-off.** Give each agent one task with a self-contained brief, worktree,
exact revision or starting state, and relevant GitHub links. Scope each
delivery task to one PR and its focused gate set. Start from a merged base
where possible. Start independent work while the queue runs, and prepare
dependent work up to the point where it needs the merge. Check the report and
GitHub state before ending the agent's task, and use a fresh brief and agent
for follow-up work.

**Review.** For a brief that defines a contract or state machine, have
`gpt-6.2-astra` check the brief at low effort before Sol implements it, then
commission one code review at the effort described in [Reviewer](#reviewer).
Review mechanical work directly. Include the single-ownership check from
AGENTS.md in design and review briefs. Reconcile all reports against GitHub
before acting on them.

**Codex jobs.** Launch every Codex job as a tracked background command using
the harness's `run_in_background`. Never launch a job with `&` or `nohup`.
For the default role models, pass a role, a task name, and the brief as text or
`@file` to `scripts/codex-role`. For example, use
`scripts/codex-role reviewer review-contract @review.md` from the task worktree.
When a role calls for Terra, give that Codex session the same role brief.

`codex-role` uses `systemd-run --user` to launch `katl-codex-<role>-<task>.service`,
waits for the unit to finish, and returns its exit status. Choose a task name
of 1 to 232 characters from `[A-Za-z0-9:_.-]` that identifies this attempt.
Record the unit name and printed log path in the local state. The launcher
creates the default log directory and writes to
`${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev/logs/katl-codex-<role>-<task>.log`.
To choose a log path, pass `--log PATH` before the task name and use an
existing parent directory. The unit overwrites the log with the Codex report
and ends it with `exit=N`.

The unit masks the home directory and mounts the worktree, linked Git metadata,
Go caches, Codex auth and session state, and any external log directory. When
GitHub CLI configuration is present, the unit mounts it read-only so roles can
use `gh` to read issues and pull requests.

To recover jobs after a hand-off, find running units with
`systemctl --user list-units 'katl-codex-*'`. Use
`journalctl --user -u katl-codex-<role>-<task>.service` to inspect the unit's
lifecycle, including after systemd collects the unit. Open the recorded log
for the Codex report and exit status. If the log has no exit line, check the
journal for a startup failure or interruption.

**PR description.** The orchestrator owns each PR's description, which becomes
its squash commit message. Write it before enabling auto-merge and rewrite it
after every fix round so it describes the final change, following AGENTS.md.
Post gates, reviews and journey evidence as PR comments.

**Waiting and failures.** Use background commands for queue and check waits.
With the watcher enabled, use `katl-queue wait PR` and save its watch ID so
another session can resume it. Use `katl-queue watch PR` without the watcher.
Apply the flake rule in AGENTS.md when a check fails independently of the PR.
Use a separate fix issue for the flake, and on recurrence look for the common
design cause before commissioning another narrow fix.

**CI cost.** When queue waits dominate, use
`katl-queue ci-cost --since <previous release date>`
to rank jobs by runner-minutes. Before reporting a recurring delay, check
what the step verifies, what inputs changed, and whether it needs to run.
File CI cost findings with the measurement window, run and failure counts,
median duration, and total runner-minutes.

**Main milestones.** Use **Next release** for committed work and **Backlog**
for uncommitted work. After the cut, send findings relevant to the cut
release to its Release agent for a scope decision. Deliver admitted fixes on
`main` first and hand the merged revision back to the Release agent.

**Release hand-off.** At the branch cut, start one Release agent for the
release. Use its brief template to pass the version, branch and cut revision,
planned scope and open blockers, and any explicit operator authority for
that release. Record the agent and version milestone in the checkpoint and
continue delivery on `main`. [Release agent brief](../../.claude/agents/katl-release.md#brief-template)

## Release

**Purpose.** Own one release from branch cut through publication and milestone
closure. Start at the cut and work alongside the main-branch Orchestrator.

**Brief.** Take the release version, branch and cut revision, planned scope
and open blockers, and any explicit operator authority from the Orchestrator.

**Model.** Use Claude Opus.

**Report.** Tell the coordinator when a blocker resolves, a candidate is
tagged or published, the final release or its security advisory is published,
or a decision needs operator authority. Include the affected revision, the
evidence, and the next action when a gate or a permission blocks progress.

**Hand-off.** Take ownership at freeze, including the branch cut. Route
admitted fixes through the Orchestrator on `main` first, then carry the merged
fixes to the release branch. Keep the milestone and checkpoint current so the
coordinator can recover the release and its owner.

**Qualification and publication.** At the cut, use the CI cost check described
under [Orchestrator](#orchestrator). Run the outstanding-journey query in
AGENTS.md and confirm that no journey release blockers remain before cutting
a candidate. Qualify the candidate from the release branch. Review the
generated `RELEASE_NOTES.md` for the exact version before each tag that
triggers publication. Tag the release candidate and verify its published
run, then promote the passing candidate by tagging the final version at the
same commit. Verify final publication and publish the security advisory when
one applies. [Release procedure](releases.md)

**Authority.** Obtain the operator's explicit authority for each release
before tagging or triggering publication, including candidate and final
publication and any security advisory. Confirm which actions the authority
covers and ask the coordinator for any missing authority. Never infer it
from a previous release, the hand-off, or a passing gate.

### Release milestones

At the branch cut, the Release agent creates a milestone named for the
release (`vX.Y.Z`) and decides its scope. Reserve it for work shipping from
that branch and move the release's open blockers and their fix PRs for both
`main` and the release branch into it. Close the milestone when the final
release is published.

The Release agent applies this rule to every new finding after the cut. Add
it only if it causes data loss, an unreachable node, a wrong-target operation,
or a misleading security outcome in shipped code, or if it is a release
artifact defect involving the release notes, candidate, or tag run. Put all
other findings in **Next release**. Include the admission rule in briefs for
fixes going into the release.

## Delivery

**Purpose.** Prepare one issue for a ready-for-review PR. Start with the
operator workflow required by AGENTS.md and find the existing owner of every
responsibility the change touches. Use a fresh worktree based on an up-to-date
`origin/main` and reserve `release/*` branches for release work. Read the issue
discussion, parent issue, and relevant design docs. If the scope exceeds one
PR, deliver the smallest coherent step and propose sub-issues for the rest.

**Brief.** Supply the issue, acceptance criteria, the authorized delivery
scope, and any dependencies, operator preconditions, or release milestone.
For operator-facing changes, supply or identify the starting state and
intended outcome.

**Model.** Use Codex `gpt-6.2-sol` with medium effort.

**Execution.** Run the unit and golden gate and check tests and fixtures for
references to anything removed or renamed. Run the matching tests. Pass the
issue's kind, area, milestone, and applicable workflow labels to the
orchestrator with the proposed PR text. Include `Closes #N` when the work
completes the issue. Report the focused local VM command required by
AGENTS.md, including its Go package, `-run`, and `-count=1`.

**Report.** Lead with the outcome, root cause for a defect, and any remaining
gap. Then give the worktree and base revision, any existing PR link, the
design and single owner, material alternatives considered, tests and the
incorrect behavior each distinguishes, `go test` exit status, the applicable
VM gate command, and the public journey commands with their starting state
and expected results.

**Hand-off.** Return the report to the orchestrator when the scoped change
is ready or blocked. Identify outstanding review, gates, journeys, and any
proposed sub-issues. Leave the change in its worktree for the orchestrator
to review, commit, and publish. In the report, propose the PR title and
description, including what changed and why, the root cause for defects, the
issue-closing reference when applicable, and the `Release-Note:` trailer.
Branch commit messages are working notes.

## Reviewer

**Purpose.** Independently review the proposed change. Check the operator
workflow and single ownership first, then assess correctness, failure and
recovery behavior, and the evidence for the change. Leave fixes and queue
actions to their owners.

**Brief.** Supply the PR and head SHA, base revision, issue, acceptance
criteria, any operator workflow, and the risk or question to review. Include
available gate evidence.

**Model.** Use Claude Opus with high effort for the independent review that
gates auto-merge. For Codex `gpt-6.2-astra`, use low effort for brief or contract
reviews and narrow checks, and medium effort for ordinary code reviews. Use
high effort only for risky boot, disk, update, security, or trust changes, or
when the review gates a hard-to-reverse merge. Astra costs several times as
much as Sol, so run one code review per change and file follow-up issues for
edge-case findings. Use Codex `gpt-6.1-terra` with medium effort when
screenshots, images, or very large reads dominate the task.

**Report.** Lead with findings in severity order. For each, give a file and
line, the condition that triggers the defect, and its consequence. Then give
the approve or request-changes verdict, the reviewed SHA, and any evidence
gaps.

**Hand-off.** Return the verdict to the orchestrator for that revision. The
orchestrator routes fixes and obtains approval for the updated change.

## Debugger

**Purpose.** Diagnose a failure from code and retained evidence. Trace the
violated invariant to its owner, check other implementations of that
responsibility, and narrow the cause with read-only checks. Leave changes,
queue actions, and VM mutations to the roles that own them.

**Brief.** Supply the symptom, expected and observed behavior, exact
revision and failing command or job, and available logs, run IDs, or other
artifacts. Include prior attempts and any reproduction constraints.

**Model.** Use Codex `gpt-6.2-astra` with high effort for hard debugging. Use
`gpt-6.1-terra` with medium effort when screenshots, images, or very large
reads dominate the task.

**Report.** Lead with the root cause or the best-supported hypothesis and
its uncertainty. Then give the evidence and run IDs, the violated invariant
and its owner, explanations the evidence rules out, and the narrowest
command or observation that would distinguish any remaining hypotheses.

**Hand-off.** Give the orchestrator the proposed fix scope and the behavior a
regression test should verify. Identify any evidence or execution capability
needed to finish the diagnosis.

## Journey

**Purpose.** Run the operator workflow hands-on in a persistent `katldev` VM
through public `katlctl` commands. Verify progress, errors, and actual
outcomes against AGENTS.md, including persistence and recovery where
applicable. Run alongside the queue or on `main` after merge, without holding
the PR. Preserve failed VM state for diagnosis and leave product fixes to
delivery.

**Brief.** Supply the issue, PR, tested revision, starting VM state and
environment, and the documented public commands with expected outcomes.
Identify the applicable repeat, reboot, and failure or recovery paths.

**Model.** Use Claude Sonnet with medium effort.

**Report.** Lead with passed, gap, or blocked and the reason. Then give the
tested revision, VM starting state, exact commands and observed outcomes,
independent state evidence, and links to any gap issue or outstanding check.

**Hand-off.** Record the exact commands and observed outcomes on the PR.
Apply the journey labels and file any gap as described above.
Return the record and any capability gap to the orchestrator, who schedules
the fix and rerun.
