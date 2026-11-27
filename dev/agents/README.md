# Katl agent workflow

Every role follows the delivery, testing, and user-journey rules in
[AGENTS.md](../../AGENTS.md). GitHub is the shared record for scope, release
milestones, holds, dependencies, review, and journey results. The local
checkpoint lets a new session recover the work.

Find tool usage and setup in the [development tool index](../README.md).

## Model and effort

Each role belongs to a tier, and the tier table names the model for that
tier. To change a tier's model, edit its row here and the `model:` line of the
matching [agent definitions](../../.claude/agents/).

| Tier | Model | Work |
| --- | --- | --- |
| Coordinate | Claude Opus | Orchestration, release ownership, scope and approval decisions |
| Implement | Codex `gpt-6.2-sol` through [`scripts/codex-role`](../../scripts/README.md#codex-role), medium effort, high for multi-file or subtle work; Claude Opus when Codex is not available | Delivery, fix rounds and debugging |
| Design and review | Claude Fable | Design-heavy briefs, contract reviews and independent code review |
| Mechanical | Claude Sonnet | Rebases, gates, watching, journeys, planning and washup evidence |

| Role | Tier |
| --- | --- |
| [Coordinator](roles/coordinator.md) | Coordinate |
| [Orchestrator](roles/orchestrator.md) | Coordinate, high effort |
| [Release](roles/release.md) | Coordinate |
| [Planner](roles/planner.md) | Mechanical |
| [Delivery](roles/delivery.md) | Implement |
| [Reviewer](roles/reviewer.md) | Design and review, high effort for the independent review that gates auto-merge |
| [Debugger](roles/debugger.md) | Implement |
| [Rebase](roles/rebase.md) | Mechanical |
| [Journey](roles/journey.md) | Mechanical, medium effort |
| [Washup](roles/washup.md) | Mechanical |

Commission one independent code review per change, and record edge-case
findings under the [issue intake rules](#issue-intake). Reserve high effort for risky boot, disk, update,
security or trust changes and for reviews that gate a hard-to-reverse merge.

Send fix rounds on one PR to the same implementer job or session while its
context is modest. Start a fresh agent for a new PR, or when the context has
grown large.

## Codex jobs

The implement tier runs through `scripts/codex-role`. Use `scripts/codex-role`
from the task worktree; the repository's Claude permissions deny direct Codex
launches. See the [launcher reference](../../scripts/README.md#codex-role) for
job arguments, report recovery, and
[GitHub credential setup](../../scripts/README.md#provision-github-read-access).

## Local state

Use `${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev` for coordination state.
Create the directory and its worktree root on first use. The coordinator reads
the state on startup. The orchestrator and Release agent update it as their
jobs start, finish, or change hands.

| Path | Contents |
| --- | --- |
| `state.md` | Active orchestrator, Planner, and other agent identifiers, open version milestones and their Release agent identifiers, pending washups and their Release agent identifiers, issue and PR links, worktree paths, running unit names and log paths, queue watch IDs, and the next action and owner. |
| `logs/` | Codex job reports, retained across session and worktree cleanup. |
| `worktrees/` | Linked worktrees for scoped tasks and a trusted `origin/main` checkout for cleanup. |
| `evidence/` | Retained investigation evidence, managed by the [evidence tool](../../scripts/README.md#katl-dev-evidence) under `~/.local/state/katl-dev` in the passwd home. |

Reconcile the checkpoint with running jobs and GitHub before acting on it.

Read `AGENTS.md` and `dev/agents`, and run `katl-queue`, `katl-release`,
`katl-dev-evidence` and the cleanup scripts, from a worktree at the current
`origin/main`. A checkout that a session happens to start in may be many
commits behind; refresh it or use a fresh detached worktree under
`worktrees/` before acting.

Create every worktree under `worktrees/`. Never keep a worktree, or anything
else needed for more than a day, under `/tmp`, including the session
scratchpad. Hosts age out files under `/tmp`: a typical systemd-tmpfiles rule
deletes every entry not modified for two days. A long-lived worktree there loses
its untouched tracked files and then its `.git` link, while recently edited
files survive, so the damage is easy to miss.

### Retain investigation evidence

Keep only small, same-day files in the session scratchpad. Store retained
evidence larger than about 100 MB, such as harness state or downloaded CI
logs, in an entry allocated by `katl-dev-evidence new <owner>/<name>`.
Use the issue number as the owner for issue-scoped work and include the
entry path in the hand-off.

Keep VM runs in the run directories managed by `scripts/vmtest-run` and
`scripts/vmtest-clean`. If evidence must outlive its run directory, allocate
an entry with `katl-dev-evidence new` and copy the needed files there before
cleaning the run. Use the [evidence tool](../../scripts/README.md#katl-dev-evidence)
to list, retain, and release stored entries.

## Hand-offs

The coordinator routes related delivery follow-ups to the active orchestrator
as one scoped item. When starting an orchestrator, use the
[Orchestrator brief template](briefs/orchestrator.md).

The orchestrator gives each agent one task with a self-contained brief, a
worktree, the exact revision or starting state, and relevant GitHub links.
Scope each delivery task to one PR and its focused gates. Start from a merged
base where possible. Start independent work while the queue runs, and prepare
dependent work up to the point where it needs the merge. Check the report and
GitHub state before ending the agent's task. Use a fresh brief and agent for
follow-up work.

- **Delivery** returns the scoped work in its worktree with proposed PR text
  when ready or blocked. Identify outstanding review, gates, journeys, and
  proposed sub-issues. The orchestrator reviews, commits, and publishes.
- **Reviewer** reports to the orchestrator when ready or blocked. Give a
  verdict for the reviewed brief or revision when available. The orchestrator
  routes fixes and obtains approval for the updated change.
- **Debugger** reports to the orchestrator when ready or blocked. Give the
  proposed fix scope, the behavior a regression test should verify, and any
  evidence or capability still needed.
- **Journey** records exact commands and observed outcomes on the PR, applies
  the journey labels, and files gaps under the rules in AGENTS.md. Return the
  record and any capability gap to the orchestrator, who schedules the fix
  and rerun.

### Release lifecycle

The coordinator confirms the planned cut with the orchestrator. At freeze,
start one Release agent to own the release from the branch cut. The
orchestrator passes the version, branch and cut revision, scope and open
blockers, and any explicit maintainer authority with the
[Release brief template](briefs/release.md). The orchestrator does not take
the Release role; it keeps delivery and its queue cycle running on `main`.

1. Merge every release blocker into `main`. While the last blockers are in
   the queue, dispatch a non-publishing candidate from `main` and fix what
   it finds, so that the release-only jobs have run on current inputs before
   the cut.
2. Fetch `origin/main` at the cut and create a branch for this version from
   that exact SHA, for example `release/2026.12.1`. Push `release/<version>`
   and start candidate qualification from the branch, never from `main`.
3. Create milestone `v<version>` and move into it every **Next release**
   issue whose fix is on the branch. Open issues and open PRs stay in
   **Next release** unless they block this release. Use the
   [flake rule](roles/release.md#flakes) for flakes found around the cut.
   Record the milestone and its Release agent in the checkpoint.
4. Open two issues in the milestone, `Tag v<version>-rc.1` and `Tag v<version>`.
5. From then on, the Release agent owns admission to the milestone as release
   coordinator and admits almost nothing under the
   [release admission rule](roles/release.md#release-admission-rule).
6. When the final release is published, record the pending washup and its
   Release agent in the checkpoint, then close the milestone.
7. The Coordinator runs the [washup](roles/washup.md) with the Release agent
   and decides how its findings change the plans and the next release before
   the usual hand-off. Keep the washup in the checkpoint until the hand-off
   is complete.

[#777](https://github.com/katl-dev/katl/issues/777) tracks `katl-release cut`
for the currently manual steps 1–4.

Existing series branches such as `release/2026.11.x` remain in use only for
releases already cut on them.

After the cut, delivery continues on `main`. The coordinator routes release
decisions and any explicit maintainer authority to the Release agent. The
Release agent keeps the milestone and checkpoint current and routes admitted
fixes through the orchestrator on `main` first. The orchestrator hands back
merged revisions for the Release agent to cherry-pick through a PR targeting
`release/<version>`. Keep a PR's source branch outside the
`release/[0-9]*` ruleset, for example `backport/2026.12.1-fix`. Cut the next
version from the then-current `origin/main` SHA because reusing a branch
with cherry-picks would require a force-push or a merge from `main`, both
of which are forbidden.

After the cut, only process changes that block the release land on its
critical path. Route other process changes to the tooling stream.

## Issue intake

GitHub issues are the record of scope, so each one must be distinct, owned
and closable. Every role that files or triages issues follows these rules.

- Search the open issues before filing. When an existing issue covers the
  finding, comment on it instead of opening a near-duplicate.
  [#979](https://github.com/katl-dev/katl/issues/979) tracks a tool that
  enforces this check.
- File a follow-up as its own issue only when it has acceptance criteria and
  a priority and lies outside the current PR's scope. Otherwise fix it in the
  PR, or comment it onto the parent issue.
- Give a small design one issue with a short plan, not a parent with
  sub-issues.
- State the closing condition of every design or parent issue in its body,
  as a `Closes when:` line, and close the issue when the condition holds.
- Group a category, such as recurring flakes, with a label rather than an
  umbrella issue. File each member as its own issue.
- Plan every `priority: p1` issue into the next or the following version
  milestone. `Planned:` milestones and Backlog hold p2 and below, so lower a
  p1 that neither version will take.

## Reporting

The orchestrator and Release agent update the coordinator, who gives the
maintainer a brief update, when a blocker resolves, a permission is denied,
a failure remains unexplained after investigation, related issues require a
redesign, or a decision needs the maintainer's authority. Lead with the finding
or decision and link to the evidence. For every queue ejection, give the PR
and the reason from `katl-queue why PR`, the cause if known, and the fix issue
when one is needed. If the cause remains unresolved, give the current
evidence and next action.

The Release agent also tells the coordinator when a candidate is tagged or
published or when the final release or its security advisory is published.
Include the affected revision, evidence, and next action when a gate or a
permission blocks progress. The coordinator relays release events to the
maintainer.

- **Delivery** leads with the outcome, a defect's root cause, and any gap.
  Give the worktree and base revision, any existing PR link, the design and
  single owner, material alternatives considered, tests and the incorrect
  behavior each distinguishes, the `go test` exit status, the applicable VM
  gate command, and the public journey commands with their starting state
  and expected results.
- **Reviewer** leads with findings in severity order. Give the file and line,
  trigger, and consequence for each finding, followed by the approve or
  request-changes verdict when available, reviewed brief or SHA, and any
  evidence gaps.
- **Debugger** leads with the root cause or best-supported hypothesis and its
  uncertainty. Give the evidence and run IDs, the violated invariant and its
  owner, explanations the evidence rules out, and the narrowest command or
  observation that would distinguish the remaining hypotheses.
- **Journey** leads with passed, gap, or blocked and the reason. Give the
  tested revision, VM starting state, exact commands and observed outcomes,
  independent state evidence, and links to any gap issue or outstanding check.

### Permission denials

When the permission system denies an action, stop. Do not retry it another
way or perform a denied action for another agent, which is permission
laundering. Surface the denial to the maintainer and take further action on it
only on the maintainer's explicit instruction.
