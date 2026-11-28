# katl-queue

Use `katl-queue` to check whether a pull request fits the GitHub merge queue,
follow its progress, and diagnose removals. Run it from the Katl checkout in
the Nix development shell, or run `scripts/katl-queue` from the checkout.
Authenticate with `gh auth login` and use a login with permission for any
changes you intend to make. GitHub requests always target `katl-dev/katl`.
You can run commands like `ci-cost` and `status` with the packaged binary from
any directory. Run commands that simulate merges from the Katl checkout.

## Check and follow a pull request

### `status`

Run `katl-queue status` to see queued PRs in order and PRs with auto-merge
enabled that are waiting to enter.

### `check`

Run `katl-queue check 123` before queueing PR 123. The check fetches the
current base, queued heads, and PR head from GitHub, then simulates the
merges with `git merge-tree` in a temporary object store. It leaves the
checkout, branches, and refs untouched. It skips entries that already
conflict with the queue ahead of them and refuses closed or merged PRs.

A conflict exits with status 1 and names the conflicting paths. If the PR
conflicts with the base branch, rebase onto the reported `origin` branch.
If it conflicts with a PR ahead of it, wait for that PR to merge and check
again.

When the PR merges cleanly over the queue, the check also simulates it
against each PR that has auto-merge enabled but has not yet joined the queue.
Either may join first, depending on when its checks finish, so both orders
are simulated. A conflict in either order exits with status 1 and names the
waiting PR and the conflicting paths: the queue removes whichever of the two
joins second. A waiting PR that already conflicts with the queue is left to
the queue and not reported.

### `enqueue`

Run `katl-queue enqueue 123` when the PR is authorized to merge. The command
runs the same simulation and refuses on conflict. If the PR is already queued,
it reports its position. Otherwise it enables auto-merge. GitHub admits the
PR once its required checks pass. Use `status` or a watch to see whether
admission has happened.

A conflict with a PR waiting to join the queue also refuses. Rebase after
that PR merges, or run `katl-queue enqueue 123 --allow-pending-conflict` to
enable auto-merge anyway and accept that the queue removes whichever PR joins
second.

### `review`

After reviewing a pull request, save the verdict in a JSON file. Use the
reviewed head and its base commit as full 40-character SHAs. The base must
be an ancestor of the reviewed head.

```json
{
  "version": 1,
  "pr": 123,
  "head": "0123456789abcdef0123456789abcdef01234567",
  "base": "123456789abcdef0123456789abcdef012345678",
  "verdict": "changes-requested",
  "summary": "Fix the recovery path before merging.",
  "findings": [
    {
      "severity": "blocker",
      "path": "internal/worker/worker.go",
      "line": 41,
      "title": "Preserve state when a write fails",
      "body": "Keep the previous state available so the operation can be retried."
    }
  ]
}
```

Use version `1` and a verdict of `approved` or `changes-requested`. The
summary and each finding's title and body are required. Finding severity
must be `blocker`, `major`, `minor`, or `note`. The `path` and `line` are
optional. A `changes-requested` verdict requires at least one finding, and
an `approved` verdict cannot include a blocker. Use `"findings": []` for an
approval without findings.

From the Katl checkout, run `katl-queue review post 123 verdict.json` with
GitHub comment access. The command verifies the open PR and its current
head, then posts one comment with the verdict, summary, and findings in
severity order. If the head has changed, review the current head and update
the file. Repeating a post for the same verdict file reports that the
review is already recorded if its comment is still the newest review for
that head. A subsequent verdict can supersede it.

The review record is advisory. Record selection considers comments from
repository members and owners. A review can carry forward to another head
when the PR diff has the same `git patch-id --stable` as the reviewed diff.

### `watch`

Run `katl-queue watch --interval 30s 123 124` to follow two PRs in the
foreground. The default poll interval is one minute. With no PR numbers, the
command follows the PRs that are queued or have auto-merge enabled at startup.
It prints state changes and retries failed polls. An `UNMERGEABLE` entry
conflicts with a PR ahead of it and stays in the queue until GitHub reaches
it or it is dequeued. When a watched PR becomes `UNMERGEABLE`, the watch
simulates it over the entries ahead of it and names the entry it conflicts
with and the conflicting paths. A PR whose required checks failed on its head cannot
join the queue until the head is fixed, so it settles with the failing checks
and their links. When all watched PRs settle, the command exits with status 1
if any failed to merge.

### `why`

Run `katl-queue why 123` after a queue removal. It reports the latest removal
reason, failed merge-group checks and their links, and excerpts from failed
job logs or available VM diagnostics. If the logs cannot be retrieved, use
the reported check links to inspect the run in GitHub. For a PR outside the
queue, it also names any required checks that failed on the head. For an
`UNMERGEABLE` entry, it names the entry ahead that it conflicts with and the
conflicting paths; this simulation needs the Katl checkout, so run `why` there.

## Keep a background wait

### `watchd`

Enable the watcher through the [Home Manager module](../../dev/README.md#programskatl-dev)
to run the daemon with a systemd user socket, or run
`katl-queue watchd --interval 30s --notify` in a user session to run it in the
foreground. Use one daemon per state directory. The poll interval defaults
to one minute and must be positive. `--notify` uses `notify-send` when
available to report merges and outcomes that need action.

With Home Manager, set `programs.katl-dev.watcher.interval` to change the
default `"60s"`. Use a positive whole number followed by `ms`, `s`, `m`,
or `h`. Set `programs.katl-dev.watcher.notify = true` to enable desktop
notifications.

### `wait`

After enabling auto-merge, run `katl-queue wait 123 --timeout 2h`. The
daemon saves the wait and the client prints a resume command to stderr.
Omit `--timeout` to wait indefinitely. Use
`katl-queue wait --resume WATCH_ID --timeout 2h` to reattach after a timeout
or interruption, or replay the recorded verdict. Resume takes a watch ID
without a PR number. It preserves the original verdict, including the head
and time recorded with it. Start a fresh `katl-queue wait 123` after each push.

| Exit status | Meaning |
| --- | --- |
| 0 | The PR merged. |
| 1 | The PR needs action. Read the reported condition. |
| 2 | The invocation has no verdict. Read the error and resume when possible. |

The daemon reports a closed unmerged PR or a departure after it observed the
PR in the queue. Before admission, it also reports conflicts, requested
changes, failed head checks, or auto-merge still being off after two poll
intervals. If the head has no check runs or commit statuses after ten minutes,
close and reopen the PR or push to retrigger CI, then start a fresh wait.
A new observed head restarts that grace period.

### `watches`

Run `katl-queue watches` to list pending waits and recent results. Pending
waits survive daemon restarts and completed results are retained for seven
days.

## Plan and maintain the queue

### `plan`

Run `katl-queue plan --milestone 2026.12.0` to report enqueue order and holds.
Omit `--milestone` for a plan across milestones. The planner simulates eligible
PRs over the default branch, the queue, and the PRs it has already chosen. It
prioritizes `release-blocker`, then `dev-experience`, then other PRs, with
older PRs first within each group.

The planner holds PRs for a `Hold: REASON` line outside a code fence, unresolved
dependencies, conflicts, failed required checks, and queue removals that
require a new head. Dependencies come from GitHub blocked-by links on the PR
or the issue it closes, and from `Depends on #N` in the PR body. Dependencies
in other repositories hold the PR because the planner cannot check them.
A failed optional check holds a PR in the plan when another PR depends on it.
Use the [delivery rules](../../AGENTS.md#task-and-delivery-workflow) to decide
when to add or remove a hold.

Use `katl-queue plan --milestone 2026.12.0 --next` to list open, unassigned
issues whose blockers are closed. Use
`katl-queue plan --milestone 2026.12.0 --apply` for one autopilot pass.
`--apply` requires a milestone and cannot be combined with `--next`.

### `autopilot`

Run `katl-queue autopilot --milestone 2026.12.0 --once` to preview one
pass. Add `--apply` to make the reported changes. Apply requires a milestone.
Without `--once`, the command repeats until interrupted. Set
`--interval 2m` to change the default one-minute interval between passes.

Autopilot enables auto-merge for clean PRs, disables it for blocking holds
other than a pending rebase, dequeues eligible entries marked `UNMERGEABLE`,
and comments on failed or timed-out queue checks. It leaves failed checks for
you to fix and push. It excludes drafts, forks, PRs targeting a branch other
than the default branch, and PRs with a `release/*` head or base.

Autopilot attempts a rebase when a PR would otherwise be ready but conflicts
with the base. It pushes a clean result over HTTPS using
`--force-with-lease` and GitHub CLI credentials. A conflicting rebase gets a
`needs-rebase` label and a comment with the paths. Resolve those conflicts
and push. If the push lease fails, review the new head before making another
pass. Autopilot reports optional failures once per head and leaves
auto-merge unchanged for the optional-failure hold.

### `prune-runs`

Run `katl-queue prune-runs` to preview cancellations, then add `--apply` to
cancel. The command considers unfinished CI merge-group runs at least two minutes
old and selects those whose queue ref disappeared or points at a different
commit. If the ref listing is empty while the queue has entries, or the
required GitHub reads fail, it cancels nothing. The
[pruning workflow](../../.github/workflows/prune-merge-runs.yml) also runs this
command.

### `ci-cost`

Run `katl-queue ci-cost --limit 10` to report CI cost and queue time across
every workflow for the last seven days. Set `--since 2026-11-01` for an
inclusive start date at midnight UTC, or `--workflow ci.yml` to report one
workflow; `--workflow` accepts a filename, display name, or ID, and an
ambiguous display name needs the filename or ID. Add `--json` for structured
output. The report covers completed runs created in the window and has five
tables:

- Each workflow's runs, failed and cancelled runs, runner-minutes, and runner
  wait: the 50th and 90th percentiles and the maximum of the time from a job
  being queued to its start.
- Each workflow's merge-group runs by conclusion, with the 50th and 90th
  percentile wall time of successful ones, from creation to completion.
- Failed and cancelled merge-group runs counted by the first job of their last
  attempt to finish unsuccessfully. A job that hit its time limit shows as
  cancelled under its own name. A group cancelled from outside, for example
  when the queue replaced it, shows whichever job stopped first.
- Job names and job prefixes ranked by runner-minutes, per workflow. These
  include timed attempts from reruns and omit skipped jobs and jobs that never
  reached a runner.

Percentiles are nearest-rank, so each is a duration that occurred. A `--limit`
of zero shows all rows in each ranking; the limit applies to the
failing-job, job and prefix rankings. GitHub server errors and dropped
connections are retried with backoff, so one failed request does not abort a
long report; an error that persists, or any other error, ends the command and
names the request.

### `cache`

Run `katl-queue cache` from the checkout root to compare the repository's
Actions cache entries with [`.github/cache-policy.json`](../../.github/cache-policy.json).
It prints each family's usage against its budget and lists the entries outside
policy: those saved off the policy's save ref, those matching no family, and
keys beyond a family's `max_generations`, where the keys restored most recently
are kept. Add `--apply` to delete those entries, each reported on standard
error as it happens; it refuses unless the local policy file matches the
default branch's copy exactly. Add `--json` for
structured output. The command exits with status 1 when the entries that
remain exceed a family's budget or the policy's alert threshold. The
[CI health workflow](../../.github/workflows/ci-health.yml) runs it daily with
`--apply`.

### `issue`

Use `scripts/katl-issue new` to check a draft before filing it and
`scripts/katl-issue report` to review existing issues. Follow the
[issue intake command reference](../../dev/issue-intake.md).

### `help`

Run `katl-queue --help` or `katl-queue help` for the command list. Run
`katl-queue watch --help`, for example, for that command's flags.

## State, logs, and recovery

- The checkout wrapper builds `_build/katl-queue`. Simulations, automatic
  rebases, and downloads for diagnostic excerpts use temporary directories
  that the command removes when it finishes.
- Queue entries, check results, and CI logs live on GitHub. Autopilot stores
  head-specific markers in PR comments to distinguish its own dequeues from
  manual removals and avoid repeating reports.
- The daemon stores `watches.json`, `rate-limit.json`, and `watch.lock`
  in `${XDG_STATE_HOME:-$HOME/.local/state}/katl-queue`. Clients connect
  through `$XDG_RUNTIME_DIR/katl-queue/watch.sock`.
- Foreground commands report to the terminal. For the Home Manager watcher,
  read `journalctl --user -u katl-queue-watch.service`. If the socket is
  unavailable, use a user session and start it with
  `systemctl --user start katl-queue-watch.socket`. A lock error can indicate
  another running daemon, so check the service before starting a foreground
  instance.
- If GitHub requests fail, check `gh auth status` and repository access. For
  a background wait, restore access and resume the saved watch.
