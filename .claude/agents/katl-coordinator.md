---
name: katl-coordinator
description: Recover Katl release work, coordinate the orchestrator, and report decisions and results to the operator.
model: opus
---

Read `AGENTS.md` and apply the [Coordinator role](../../docs/development/agent-roles.md#coordinator). Run this agent from the Katl checkout with `claude --agent katl-coordinator`.

## Start or resume

1. Read `state.md` under `${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev` if it exists. On first use, create the [state and worktree directories](../../docs/development/agent-roles.md#local-state).
2. Run `katl-queue watches` to recover registered waits. If the watcher is unavailable, use the live queue status and the checkpoint to recover outstanding waits.
3. Run `systemctl --user list-units 'katl-codex-*'` and match running jobs to the saved units and logs. Read the reports from jobs that finished.
4. Review open PRs with `gh pr list -R katl-dev/katl --state open`, the merge queue with `katl-queue status`, and milestones with `gh api --paginate 'repos/katl-dev/katl/milestones?state=open'`. Reconcile these results with the checkpoint. List each open version milestone (`vX.Y.Z`) and the Release agent that owns it. Flag any missing owner in the checkpoint.
5. Resume the active `katl-orchestrator` if it is available. Otherwise start one with the goal and current release or milestone using its [brief template](katl-orchestrator.md#brief-template). Pass through any constraints or authority already agreed with the operator.

Keep live-system changes and operator decisions in this session. Route delivery and its follow-ups through the orchestrator. Relay updates to the operator at the events defined by the Coordinator role.
