# Agent roles

Every role follows [AGENTS.md](../../AGENTS.md). GitHub is the record for scope,
status, and dependencies. Use milestones for release scope and deferrals,
sub-issues for follow-up work, and blocked-by links for dependencies. Use labels
for status and keep PR `Hold:` lines subject to AGENTS.md. Keep local state files
for running work.

Label each issue and PR that changes an operator workflow `user-facing` at
creation. Record the journey outcome on the PR and apply `journey: passed` or
`journey: gap`. For a gap, link the `release-blocker` issue in the same milestone.

Use `scripts/codex-role` for the default Codex sessions below. When a role
calls for Terra, start a Codex session with that model and give it the same
role brief.

## Orchestrator

**Purpose.** Own release coordination and judgment. Define scope, sequence
dependencies, dispatch work, decide whether evidence supports approval, and
track the queue and journey outcomes through release readiness. Assign
implementation, independent review, diagnosis, and hands-on journeys to their
respective roles. Schedule gates as commands and judge their results.

**Brief.** Supply the release target or milestone, the outcome sought, the
issues and PRs in scope, and authority for any queue or release actions.

**Model.** Use Claude Opus with high effort.

**Report.** Lead with release blockers and decisions needed. Then give the
affected issue and PR links, queue and journey evidence, and the next action
and owner for each open item.

**Hand-off.** Give each agent a bounded brief and an exact revision or starting
state. Reconcile their reports against GitHub, verify the required independent
approval before enabling auto-merge on risky changes, and return release
readiness and remaining decisions to the operator.

## Delivery

**Purpose.** Deliver one issue as one ready-for-review PR. Start with the
operator workflow required by AGENTS.md and find the existing owner of every
responsibility the change touches. Use a fresh worktree based on an up-to-date
`origin/main` and reserve `release/*` branches for release work. Read the issue
discussion, parent issue, and relevant design docs. Link the Bead with
`--external-ref gh-N`. If the scope exceeds one PR, deliver the smallest
coherent step and propose sub-issues for the rest.

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
sub-issues. A sandboxed session that cannot commit, push, or write Beads
leaves the change in its worktree with the proposed commit title and body;
the orchestrator reviews the diff, commits, opens the PR, and updates the
Bead.

## Reviewer

**Purpose.** Independently review the proposed change. Check the operator
workflow and single ownership first, then assess correctness, failure and
recovery behavior, and the evidence for the change. Leave fixes and queue
actions to their owners.

**Brief.** Supply the PR and head SHA, base revision, issue, acceptance
criteria, any operator workflow, and the risk or question to review. Include
available gate evidence.

**Model.** Use Claude Opus with high effort for the independent review that
gates auto-merge. Use Codex `gpt-6.2-astra` with high effort for adversarial
review. Use Codex `gpt-6.1-terra` with medium effort when screenshots, images,
or very large reads dominate the task.

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

**Hand-off.** Record the commands and outcomes in the Bead and the result
on the PR. Apply the journey labels and file any gap as described above.
Return the record and any capability gap to the orchestrator, who schedules
the fix and rerun.
