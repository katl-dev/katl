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

**Codex jobs.** Use `scripts/codex-role` as the only way to launch a Codex job.
Run it as a tracked background command with the harness's `run_in_background`
so you can recover the systemd unit and its log after a hand-off. From the task
worktree, pass the role, task name, and brief as text or `@file`. Use
`scripts/codex-role reviewer review-contract @review.md` for review and
`scripts/codex-role rebase rebase-pr @rebase.md` for rebases and conflict
resolution. The rebase role uses Terra at medium effort by default and accepts
`--effort low` before the task name.

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
Go caches, Codex auth and session state, and any external log directory. If the
brief needs authenticated GitHub reads, use the
[dedicated GitHub credential](../README.md#provision-github-read-access).

To share evidence outside the repository, pass `--ro-bind PATH` once per file
or directory before the task name. Refer to the resolved paths in the brief
when the inputs are symlinks. Use individual paths for evidence in a session
scratchpad because the unit has a private `/tmp`. Explicit `--bind` and
`--ro-bind` reject the entire `/tmp` directory, the home directory, its
ancestors, and paths that resolve inside it.

To recover jobs after a hand-off, find running units with
`systemctl --user list-units 'katl-codex-*'`. Use
`journalctl --user -u katl-codex-<role>-<task>.service` to inspect the unit's
lifecycle, including after systemd collects the unit. Open the recorded log
for the Codex report and exit status. If the log has no exit line, check the
journal for a startup failure or interruption.

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
