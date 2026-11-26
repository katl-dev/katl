# Orchestrator

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state. Follow its
[permission denial rule](../README.md#permission-denials).

**Purpose.** Own delivery on `main` from brief to merge. Decide scope,
sequencing, and whether the evidence supports approval. Own diff
review, commits, pushes, and PR and queue actions on `main`.
Delegate implementation, mechanical edits, rebases, conflict resolution,
independent review, diagnosis, and hands-on journeys.

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

**Main milestones.** Use **Next release** for the next release's committed
scope, `Planned: <theme>` for committed later work grouped by theme, and
**Backlog** for uncommitted work. The [Release role](release.md#release-milestones)
owns version milestones. After the cut, send findings relevant to the cut
release to its Release agent for a scope decision.
