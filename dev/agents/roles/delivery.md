# Delivery

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Prepare one issue for a ready-for-review PR. Start with the
operator workflow required by AGENTS.md and find the existing owner of every
responsibility the change touches. Use a fresh worktree based on an up-to-date
`origin/main` and reserve `release/*` branches for release work. Read the issue
discussion, parent issue, and relevant design docs. If the scope exceeds one
PR, deliver the smallest coherent step and propose sub-issues for the rest.

**Brief.** Supply the issue, acceptance criteria, the authorized delivery
scope, and any dependencies, operator preconditions, or release milestone.
For operator-facing changes, supply or identify the starting state and
intended outcome.

**Execution.** Run the unit and golden gate and check tests and fixtures for
references to anything removed or renamed. Run the matching tests. Pass the
issue's kind, area, milestone, and applicable workflow labels to the
orchestrator. Report the focused local VM command required by AGENTS.md,
including its Go package, `-run`, and `-count=1`.

**PR proposal.** Include a proposed PR title and description in the hand-off
report. Explain what changed and why, give the root cause for defects, include
`Fixes #N` when the work completes the issue, and end with the
`Release-Note:` trailer required by AGENTS.md. The orchestrator owns the PR
description through merge. Branch commit messages are working notes.
