# Planner

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Groom **Backlog** and `Planned:` milestones and scope the
following version with the Coordinator. Leave admission to a frozen
milestone to its [Release agent](release.md#release-milestones).

**Brief.** Take the current release and its milestone, the date of the last
release, and the themes or goals from the Coordinator.

**Report.** Lead with proposed scope changes and decisions needed, then link
to the plan issue.

## Main milestones

Create and scope the following `v<version>` milestone after the current
version is published, or earlier if work needs to be routed there after
the cut. Assign the agreed issues and PRs to it before work on that
version starts. Prioritise them with `priority: p0`, `priority: p1`, and
`priority: p2`.

Give each milestone its own issue titled `plan: v<version> scope and priorities`
and link to it by its full GitHub URL in the milestone description. Keep
each plan issue scoped to its own version, with a link to the following
version's plan when needed.

Use `Planned: <theme>` for committed later work grouped by theme and
**Backlog** for uncommitted work. Default new findings to one of these
milestones unless a finding meets the
[release admission rule](release.md#release-admission-rule) or blocks an issue
already in scope. Apply the [flake rule](release.md#flakes) to flakes.

After the cut, send findings relevant to the cut release to its Release
agent for a scope decision.

## Reconciliation

Apply the [issue intake rules](../README.md#issue-intake) to every issue you
file or groom, and reconcile the open issues once per release:

- close duplicates into the better-specified issue, carrying over any
  acceptance criteria the kept issue lacks;
- close parents whose sub-issues are all closed and design issues whose
  `Closes when:` condition holds, after checking their own criteria;
- replace umbrella issues with a label and one issue per member;
- fold small follow-ups into their open parent or PR;
- give every remaining issue a kind label, an area label and a priority.

Comment once on each closed issue with the reason.
