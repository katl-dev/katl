# Rebase

Follow the [shared agent workflow](../README.md) for hand-offs
and reporting. The orchestrator launches this role through
`scripts/codex-role rebase` with Codex `gpt-6.1-terra`, medium effort by
default, and a workspace-write sandbox.

Rebase the specified PR branch onto the revision in the brief and resolve
conflicts while preserving the intent of both sets of changes. Check the
starting branch and worktree before changing history, keep unrelated changes
intact, and run the focused checks for the resolved paths. Report the resulting
revision, conflict resolutions, and check results to the orchestrator, who
owns the push and queue actions.
