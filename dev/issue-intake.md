# Check an issue before filing it

Write the draft to a Markdown file with an `## Acceptance criteria` section
and at least one criterion. From the development shell, pass that file and the
repository's kind, area, and priority labels. You can omit the `area: ` and
`priority: ` prefixes.

```sh
scripts/katl-issue new --title 'vmtest: preserve lease timeout diagnostics' \
  --body-file issue.md --kind bug --area vmtest --priority p2
```

The command prints up to five candidates with their scores, milestones, and
body excerpts. It prints any additional strong recurrence notices and lists
the parent and siblings separately when you give `--parent N`. With no
unresolved decisions it files the issue and prints `Created #N.` The optional
`--milestone M` assigns a milestone by title.

- If an existing issue already covers the draft, rerun with
  `--duplicate-of N` to post the draft there. The command prints
  `Commented on #N.`
- If the draft supersedes an issue, rerun with `--supersedes N` to create the
  replacement, link it to the older issue, and comment on the older issue.
- For a strong open match that describes a separate issue, rerun with
  `--distinct-from N`. You can pass several issue numbers separated by commas.
  For an identical open draft, use `--duplicate-of N` to add a comment.
- For a bug or flaky test with a strong match to an earlier failure, including
  a closed one, pass `--recurrence-of N[,M]` for the same failure class or
  `--unrelated N[,M] --reason 'Explain the different failure class'`. A
  strong open match also needs an open-match decision. Family matches are
  advisory, and a superseded issue represents the same work.

When at least two earlier bugs share the failure class, the command opens a
recurrence audit or attaches the newly filed bug to an existing audit covering
a confirmed earlier bug. Use `--failure-class 'repository identity
resolved by multiple owners'` to name a new audit. The new audit inherits the
kind, area, priority, and milestone of the filed bug. Its acceptance criteria
ask for the violated invariant, a repository-wide owner search, and a
consolidation or a record of why the implementations differ.

If GitHub accepts creation but a later link or comment fails, the command
prints the created issue number and names the missing action. Use that number
to finish the action. If you need to file the audit separately, include an
`Audits: #N, #M, #P` line naming the bugs in its body.

## Review the batch report

```sh
scripts/katl-issue report
```

The report prints likely duplicate and supersession pairs among open issues
and clusters of at least three related bugs that lack a covering audit. The
daily **Issue intake** workflow publishes the same report in its run summary.
It uses read-only GitHub permissions.

Both commands calculate TF-IDF and cosine similarity from one paginated
repository issue snapshot, weighting titles and shared identifiers such as
paths, test names, and issue references. Open issues and closed bugs supply
the corpus. The reporting floor is 0.20 and the strong-match floor is 0.40.
