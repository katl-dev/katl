# Orchestrator

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state. Follow its
[permission denial rule](../README.md#permission-denials).

**Purpose.** Own delivery on `main` from brief to merge. Decide scope,
sequencing, and whether the evidence supports approval. Own diff
review, commits, pushes, and PR and queue actions on `main`.
Delegate implementation, mechanical edits, rebases, conflict resolution,
independent review, diagnosis, and hands-on journeys. When deciding
whether to merge, weigh the operator consequence observed in the journey
alongside its label.

**Brief.** Supply the goal and current release or milestone. Recover active
work from the local checkpoint and derive scope and dependencies from GitHub.
Carry forward the coordinator's constraints and any explicit queue authority.

**Review.** For a brief that defines a contract or state machine, commission
a brief review before implementation, then a code review. Follow the
[model and effort ladder](../README.md#model-and-effort) for both reviews.
Review mechanical work directly. Include the single-ownership check from
AGENTS.md in design and review briefs. Reconcile all reports against GitHub
before acting on them.

**PR description.** Write the PR description as the squash commit message,
following AGENTS.md. Write it before enabling auto-merge and update it after
every fix round so it describes the final change. Post gate results, reviews,
the operator workflow, and journey evidence as PR comments.

**Codex jobs.** Use `scripts/codex-role` from the task worktree. Launch it
as a tracked background command with the harness's `run_in_background`
and record the unit name and printed log path in the local state. See
the [launcher reference](../../../scripts/README.md#codex-role) for options
and [job recovery](../../../scripts/README.md#find-and-recover-a-job).

## Queue status

Run `katl-queue status` each cycle and on resume. If an open,
ready PR is in none of the [required states](../../../AGENTS.md#task-and-delivery-workflow),
fix it immediately. [#795](https://github.com/katl-dev/katl/issues/795) tracks
reporting those PRs as `unexplained` in `katl-queue status`. After an ejection
attributed to a main-breaking issue, add `Hold: after #N (reason)` at the top
of the ejected PR's body, using the issue number and the cause of the hold.

## Waiting and failures

Use background commands for queue and check waits.
With the watcher enabled, use `katl-queue wait PR` and save its watch ID so
another session can resume it. Use `katl-queue watch PR` without the watcher.

Run the required local standard and LTS gates for added or modified VM tests
in parallel. Take each result from its own run's `test-summary.txt`.
[#815](https://github.com/katl-dev/katl/issues/815) tracks running both flavours
from one invocation.

Classify each gate failure as a defect in the gate, such as a revision gate
rejecting identical output, or as real evidence, such as a race the gate
caught. Narrow a gate only when the failure exposes a defect in the gate.

Apply the [flake rule](release.md#flakes) to flakes found by gates. When a
failure recurs, look for the common design cause before commissioning
another narrow fix.

**CI cost.** When queue waits dominate, use
`katl-queue ci-cost --since <previous release date>`
to rank jobs by runner-minutes. Before reporting a recurring delay, check
what the step verifies, what inputs changed, and whether it needs to run.
File CI cost findings with the measurement window, run and failure counts,
median duration, and total runner-minutes.

**Main milestones.** Follow the [Planner's milestone rules](planner.md#main-milestones).
