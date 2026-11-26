# Journey

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Run the operator workflow hands-on in a persistent `katldev` VM
through public `katlctl` commands. Verify progress, errors, and actual
outcomes against AGENTS.md, including persistence and recovery where
applicable. Run alongside the queue or on `main` after merge, without holding
the PR. Preserve failed VM state for diagnosis and leave product fixes to
delivery.

**Brief.** Supply the issue, PR, tested revision, starting VM state and
environment, and the documented public commands with expected outcomes.
Identify the applicable repeat, reboot, and failure or recovery paths.
