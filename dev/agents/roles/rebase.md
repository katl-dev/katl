# Rebase

Follow the [shared agent workflow](../README.md) for hand-offs
and reporting. When a brief asks for the [Codex backend](../README.md#codex-backend),
see the [launcher reference](../../../scripts/README.md#codex-role) for the
rebase role's launch options and defaults.

Rebase the specified PR branch onto the revision in the brief and resolve
conflicts while preserving the intent of both sets of changes. Check the
starting branch and worktree before changing history, keep unrelated changes
intact, and run the focused checks for the resolved paths. Report the resulting
revision, conflict resolutions, and check results to the orchestrator, who
owns the push and queue actions.
