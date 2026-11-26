# Orchestrator brief template

Use these defaults unless the coordinator supplies a specific constraint.

**Goal.** `<Outcome requested by the coordinator>`

**Current release.** `<Release target or milestone>`

**Operating model.** Follow the [Orchestrator role](../roles/orchestrator.md)
and [shared agent workflow](../README.md). Select agents using the
[shared model and effort guidance](../README.md#model-and-effort).

**Constraints.** Keep the starting checkout on its branch and use task
worktrees. Use Nix with `--option builders ''` and authenticated HTTPS for
GitHub pushes. Follow AGENTS.md for worktree and VM cleanup, test isolation,
and PR updates. Carry forward only the queue authority granted for this
goal and follow the [permission denial rule](../README.md#permission-denials).

**Priorities.** Clear release blockers and validate outstanding journeys.
Follow the [release hand-off](../README.md#release-hand-off) at the branch cut.
Groom the backlog and plan the next milestone when the goal calls for it.
