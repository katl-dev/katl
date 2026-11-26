# Agent roles

Every role follows [AGENTS.md](../../AGENTS.md). GitHub is the record for scope,
status, and dependencies. Use milestones for release scope and deferrals,
sub-issues for follow-up work, and blocked-by links for dependencies. Use labels
for status and keep PR `Hold:` lines subject to AGENTS.md. Keep local state files
for running work.

Label each issue and PR that changes an operator workflow `user-facing` at
creation. Record the journey outcome on the PR and apply `journey: passed` or
`journey: gap`. For a gap, link the `release-blocker` issue in the same milestone.

## Orchestrator

**Purpose.** Own release coordination and judgment. Define scope, sequence
dependencies, dispatch work, decide whether evidence supports approval, and
track the queue and journey outcomes through release readiness. Assign
implementation, independent review, diagnosis, and hands-on journeys to their
respective roles. Schedule gates as commands and judge their results.

**Release cut.** Run `katl-queue ci-cost --since <previous release date>`.
For the jobs that used the most runner-minutes, ask what each verifies, what
inputs it actually depends on, and whether it needs to run at all. File an
issue for each finding with the date window and the job's attempt count,
median duration, and total runner-minutes.

**Brief.** Supply the release target or milestone, the outcome sought, the
issues and PRs in scope, and authority for any queue or release actions.

**Model.** Use Claude Opus with high effort.

**Report.** Lead with release blockers and decisions needed. Then give the
affected issue and PR links, queue and journey evidence, and the next action
and owner for each open item.

**Hand-off.** Give each agent a bounded brief and an exact revision or starting
state. Before dispatching delivery for a brief that defines a contract or state
machine, have `gpt-6.2-astra` review the brief at low effort. `gpt-6.2-sol`
implements the brief literally, so a contradiction becomes a defect. Skip this
review for mechanical briefs.

**Codex jobs.** Launch every Codex job as a tracked background command using
the harness's `run_in_background`. Never launch a job with `&` or `nohup`.
For the default role models, pass a role, a task name, and the brief as text or
`@file` to `scripts/codex-role`. For example, use
`scripts/codex-role reviewer review-413 @review.md` from the task worktree.
When a role calls for Terra, give that Codex session the same role brief.

`codex-role` uses `systemd-run --user` to launch `katl-codex-<role>-<task>.service`,
waits for the unit to finish, and returns its exit status. Choose a task name
that is unique among running jobs for that role and uses 1 to 232 characters
from `[A-Za-z0-9:_.-]`. Record the unit name and printed log path in the local
state for the job. Without `--log`, the log is
`_build/codex/katl-codex-<role>-<task>.log` in the worktree. To choose a log path,
pass `--log PATH` before the task name and use an existing parent directory.
The unit overwrites the log with the Codex report and ends it with `exit=N`.

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

Reconcile agent reports against GitHub, verify the required independent
approval before enabling auto-merge on risky changes, and return release
readiness and remaining decisions to the operator.

## Delivery

**Purpose.** Deliver one issue as one ready-for-review PR. Start with the
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
references to anything removed or renamed. Run the matching tests. On the PR,
use the issue's kind and area labels and assigned milestone, apply any
workflow labels described at the top of this guide, and include
`Closes #N` for work that completes the issue. Follow AGENTS.md for auto-merge
and holds. The merge-queue VM run is the regression gate. Give the
orchestrator any focused local VM command required by AGENTS.md, including
its Go package, `-run` and `-count=1`. The journey role owns hands-on
validation.

**Report.** Lead with the outcome, root cause for a defect, and any remaining
gap. Then give the PR number and head SHA, design and single owner, material
alternatives considered, tests and the incorrect behavior each distinguishes,
`go test` exit status, the exact applicable VM gate command, and the public
journey commands with their starting state and expected results.

**Hand-off.** Return the report to the orchestrator once the scoped PR is
ready. Identify outstanding review, gates, journeys, and any proposed
sub-issues. A sandboxed session that cannot commit or push leaves the change
in its worktree with the proposed commit title and body. The orchestrator
reviews the diff, commits, and opens the PR. Branch commit messages are
working notes; propose the PR description (what changed and why, root cause,
`Fixes #N`, any `Release-Note:` trailer) in the report, and the orchestrator
writes and updates the PR description before enabling auto-merge.

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
