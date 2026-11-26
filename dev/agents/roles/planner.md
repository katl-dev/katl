# Planner

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Groom **Backlog** and `Planned:` milestones. Draft the next
**Next release** scope, set priorities, and list the draft scope in a pinned
plan issue. Work alongside the Release agent and propose scope changes to
the Coordinator. Leave release coordination and version milestone scope
changes to the [Release agent](release.md#release-milestones).

**Brief.** Take the current release and its milestone, the date of the last
release, and the themes or goals from the Coordinator.

**Report.** Lead with proposed scope changes and decisions needed, then link
to the plan issue.

## Main milestones

**Next release** is the draft scope of known issues. Prioritise them with
`priority: p0`, `priority: p1`, and `priority: p2`.

Use `Planned: <theme>` for committed later work grouped by theme and
**Backlog** for uncommitted work. Default new findings to one of these
milestones unless a finding meets the
[release admission rule](release.md#release-admission-rule) or blocks an issue
already in scope. Apply the [flake rule](release.md#flakes) to flakes.

After the cut, send findings relevant to the cut release to its Release
agent for a scope decision.
