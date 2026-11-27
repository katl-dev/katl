# Delivery

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Prepare one issue for a ready-for-review PR. Start with the
operator workflow required by AGENTS.md and find the existing owner of every
responsibility the change touches. Use a fresh worktree at the revision in
the brief. Base new work on up-to-date `origin/main` and follow-up work on the
current head of the PR. Choose a PR source branch outside the
`release/[0-9]*` ruleset, including for cherry-picks targeting a release
branch. Read the issue discussion, parent issue, and relevant design docs.
If the scope exceeds one PR, deliver the smallest coherent step and propose
sub-issues for the rest.

**Brief.** Supply the issue, acceptance criteria, the authorized delivery
scope, and any dependencies, maintainer preconditions, or release milestone.
For operator-facing changes, supply or identify the starting state and
intended outcome.

**Execution.** Run the unit and golden gate and check tests and fixtures for
references to anything removed or renamed. Run the matching tests. Pass the
issue's kind, area, milestone, and applicable workflow labels to the
orchestrator. Report the focused local VM command required by AGENTS.md,
including its Go package, `-run`, and `-count=1`. For added or modified VM
tests, use `scripts/vmtest-run --flavour standard,lts` to cover both kernel
flavours from the delivery worktree.

Keep the scratchpad for small, transient files. Use the
[evidence store](../README.md#retain-investigation-evidence) for evidence over
about 100 MB and copy VM evidence there only if it must outlive its run
directory.

**PR proposal.** Include a proposed PR title and description in the hand-off
report. Explain what changed and why, give the root cause for defects, include
`Fixes #N` when the work completes the issue, and end with the
`Release-Note:` trailer required by AGENTS.md. The orchestrator owns the PR
description through merge. Branch commit messages are working notes.
