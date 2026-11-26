# Katl agent workflow

Every role follows the delivery, testing, and user-journey rules in
[AGENTS.md](../../AGENTS.md). GitHub is the shared record for scope, release
milestones, holds, dependencies, review, and journey results. The local
checkpoint lets a new session recover the work.

Find tool usage and setup in the [development tool index](../README.md).

## Model and effort

| Role | Default |
| --- | --- |
| [Coordinator](roles/coordinator.md) | Claude Opus |
| [Orchestrator](roles/orchestrator.md) | Claude Opus, high effort |
| [Release](roles/release.md) | Claude Opus |
| [Planner](roles/planner.md) | Claude Sonnet |
| [Delivery](roles/delivery.md) | Codex `gpt-6.2-sol`, medium effort |
| [Reviewer](roles/reviewer.md) | Codex `gpt-6.2-astra`, medium effort for ordinary code reviews and high effort for the independent review that gates auto-merge. |
| [Debugger](roles/debugger.md) | Codex `gpt-6.2-astra`, high effort for hard debugging |
| [Rebase](roles/rebase.md) | Codex `gpt-6.1-terra`, medium effort |
| [Journey](roles/journey.md) | Claude Sonnet, medium effort |
| [Washup](roles/washup.md) | Claude Sonnet |

Claude usage is scarce. Use Claude Opus for review only when Codex cannot do
the job.

Use Astra at low effort for brief or contract reviews and narrow checks.
For code review, reserve high effort for risky boot, disk, update, security,
or trust changes and reviews that gate a hard-to-reverse merge. Astra costs
several times as much as Sol, so commission one code review per change and
file follow-up issues for edge-case findings. Use Codex `gpt-6.1-terra` at
medium effort for review or diagnosis dominated by screenshots, images, or
very large reads.

Send rebases and conflict resolution to Codex `gpt-6.1-terra` at low or medium
effort, or to a Claude Sonnet subagent, not to Sol. Keep Sol for
implementation.

## Launch Codex jobs

Use `scripts/codex-role` from the task worktree so jobs and their logs survive
the launching session. The repository's Claude permissions deny direct Codex
launches. See the [orchestrator role](roles/orchestrator.md) for launch options.

### Provision GitHub read access

Jobs can launch without GitHub authentication. For jobs that need authenticated
reads, configure a dedicated token from the maintainer's session.

1. In GitHub's **Fine-grained personal access tokens** settings, create a token
   with resource owner `katl-dev` and repository access limited to `katl`.
2. Under **Repository permissions**, grant **Read-only** access to **Contents**,
   **Issues**, **Pull requests**, and **Actions**. GitHub grants **Metadata**
   read access automatically.
3. Set up the token file from outside the worktree, then paste the token into
   it using an editor. Store only the token in the file.

   ```bash
   token_file="${XDG_CONFIG_HOME:-$HOME/.config}/katl-dev/codex-gh-token"
   mkdir -p -- "${token_file%/*}"
   (umask 077; touch -- "$token_file")
   chmod 0600 -- "$token_file"
   ```

The launcher checks mode `0600` and supplies the token as a read-only systemd
credential. A missing file produces a launch notice with the provisioning path.
The repository development shell includes `jq` and `gh` for this workflow.

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

Reconcile the checkpoint with running jobs and GitHub before acting on it.

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
[Release brief template](briefs/release.md).

1. Merge every release blocker into `main`.
2. Fetch `origin/main` at the cut and create a branch for this version from
   that exact SHA, for example `release/2026.12.1`. Push `release/<version>`
   and start candidate qualification from it.
3. Create milestone `v<version>` and move every issue in **Next release**,
   open or closed, into it unless there is a compelling reason to leave it
   out. Move open PRs as well, subject to the same exclusions. Use the
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
