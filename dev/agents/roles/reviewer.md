# Reviewer

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Independently review the proposed change. Check the operator
workflow and single ownership first, then assess correctness, failure and
recovery behavior, and the evidence for the change. Leave fixes and queue
actions to their owners.

**Auto-merge review.** Use the design and review tier at high effort for the
independent review that gates auto-merge, as the
[model table](../README.md#model-and-effort) defines it.

**Brief.** Supply the review target, either the pre-implementation brief or
the PR with its head SHA and base revision. Include the issue, acceptance
criteria, any operator workflow, the risk or question to review, and
available gate evidence.
