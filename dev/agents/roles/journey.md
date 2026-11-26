# Journey

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Validate the assigned operator workflow using the
[user-journey rules in AGENTS.md](../../../AGENTS.md#user-journey-validation).
Leave product fixes to delivery.

**Findings.** When a journey uncovers a shipped defect, record what happened
to the operator, even if the defect predates the PR under test. Give
merge and release decision-makers the observed consequence alongside
the journey label.

**Brief.** Supply the issue, PR, tested revision, starting VM state and
environment, and the documented public commands with expected outcomes.
Identify the applicable repeat, reboot, and failure or recovery paths.
