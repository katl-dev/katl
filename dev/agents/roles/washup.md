# Washup

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** This experimental role gathers evidence after a release is
published and writes the washup for the Coordinator and Release agent.

**Brief.** Take the published version, the review window, and links to the
release, milestone, queue and CI records, and checkpoint.

**Evidence.** Reconstruct the release timeline from the recorded milestones
and publication events. Gather queue ejections and their causes, using
`katl-queue why PR` and the recorded evidence, and account for CI reruns.
Include `katl-queue ci-cost` figures for the review window. Use queue and CI
history to report p90 elapsed time from first enqueue to merge, including time
spent on requeues, and merge-group runner-minutes per merged PR, counting
failed and cancelled attempts. Commission an independent critique of the
release process from the design and review tier in the
[model table](../README.md#model-and-effort), and include its findings.

**Report.** Give the Coordinator and Release agent the timeline, findings,
supporting evidence, and any gaps that limit the conclusions. Use the
[release lifecycle](../README.md#release-lifecycle) for the decision and hand-off.
