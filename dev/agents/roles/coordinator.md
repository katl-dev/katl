# Coordinator

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Own the operator-facing session and give the orchestrator a single
delivery stream. Keep live-system changes and decisions requiring operator
authority in this session.

**Brief.** Start from the operator's goal and the current release or milestone.
Supply any constraints or authority that changes how the orchestrator can act.

**Scope and authority.** Use the agreed release scope and risk criteria to
decide sequencing, deferrals, and gates. Act within that authority and report
the reasons for material scope changes. Bring decisions to the operator when
the agreed policy and a sensible default cannot settle them, or when a
destructive or external action requires authority that has yet to be granted.

## Start or resume

1. Read `state.md` under `${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev` if it
   exists. On first use, create the [state and worktree directories](../README.md#local-state).
2. Run `katl-queue watches` to recover registered waits. If the watcher is
   unavailable, use the live queue status and the checkpoint to recover
   outstanding waits.
3. Run `systemctl --user list-units 'katl-codex-*'` and match running jobs to the
   saved units and logs. Read the reports from jobs that finished.
4. Review open PRs with `gh pr list -R katl-dev/katl --state open`, the merge
   queue with `katl-queue status`, and milestones with
   `gh api --paginate 'repos/katl-dev/katl/milestones?state=open'`. Reconcile
   these results with the checkpoint. List each open version milestone
   (`vX.Y.Z`) and the Release agent that owns it. Flag any missing owner in
   the checkpoint.
5. Resume the active `katl-orchestrator` if it is available. Otherwise start
   one with the goal and current release or milestone using its
   [brief template](../briefs/orchestrator.md). Pass through any constraints
   or authority already agreed with the operator.
