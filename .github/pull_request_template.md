## What and why

<!-- What changed and why. For a defect, give the root cause. -->

## Operator workflow

<!--
For a user-facing change, list the exact public commands an operator runs, from
their starting state to the outcome, and what they see: progress, errors, end
state. Every step must work through katlctl or another supported interface. If
a step needs an internal shortcut or a command that doesn't exist, the change
is incomplete.

For an internal-only change, write "None: internal".
-->

## Journey

- [ ] User-facing: this changes an operator workflow, so the PR has the `user-facing` label.

<!--
Record the katldev journey for this workflow here: the commands and outcomes.
Whoever runs it then adds `journey: passed`, or `journey: gap` plus a link to
the release-blocker fix. It runs alongside the merge queue and must pass before
the next release candidate.
-->
