# Release rulesets

The files in `.github/rulesets/` are complete GitHub repository ruleset payloads.

| File | Purpose |
| --- | --- |
| [`release-branches.json`](../../.github/rulesets/release-branches.json) | Live protection for release branches, including pull requests and required checks. |
| [`release-tags.json`](../../.github/rulesets/release-tags.json) | Live protection against deleting or moving release tags, including candidates. |
| [`final-release-tags.json`](../../.github/rulesets/final-release-tags.json) | Creation restriction for final release tags. |
| [`release-branch-creation.json`](../../.github/rulesets/release-branch-creation.json) | Creation restriction for release branches. |

GitHub applies a bypass to every rule in its ruleset. Keeping creation in its
own rulesets means tag deletion and update protection still covers final and
candidate tags, and release branch updates still require pull requests and checks.

The `release/<digit>...` namespace is reserved for release branches, so use
`rel/` for release-related PR branches.

## Check the prerequisites

1. No workflow starts on a tag push. A tag push runs the workflow file at the
   tagged commit, so a tag on a commit from before the tag trigger was removed
   still runs the old tag workflow; the `final release tags` creation ruleset
   is the control for those commits.
2. The `Release` workflow's publish job uses the `katl-release` App token to
   create final tags, and the cut workflow uses it to create release branches.
   Applying `final release tags` before the publish job uses the App makes
   every final publication fail. Applying `release branch creation` before the
   cut workflow uses the App makes release branches impossible to create.

In the repository's environment settings, check both environments:

- `release` must admit only `release/[0-9]*`, require a reviewer, and disable
  administrator bypass. Narrow its deployment branch policy to `release/[0-9]*`
  before applying the rulesets.
- `release-cut` admits only `main` and disables administrator bypass.
- Both environments store the App's private key as `KATL_RELEASE_APP_PRIVATE_KEY`.

Before applying either file, check that its bypass actor's ID equals the
repository variable `KATL_RELEASE_APP_ID`.

## Apply or revert the rulesets

A repository administrator runs these commands from the repository root. To
create the two creation rulesets, use the files as supplied.

```sh
gh api --method POST repos/katl-dev/katl/rulesets \
  --input .github/rulesets/final-release-tags.json
gh api --method POST repos/katl-dev/katl/rulesets \
  --input .github/rulesets/release-branch-creation.json
```

For an existing ruleset, find its ID with `gh api repos/katl-dev/katl/rulesets`.
Update it with `gh api --method PUT repos/katl-dev/katl/rulesets/<id>` and pass
the corresponding file with `--input`. To revert the creation restrictions,
run `gh api --method DELETE repos/katl-dev/katl/rulesets/<id>` for each of the
two creation ruleset IDs.

## Verify the rulesets

Read each applied ruleset with `gh api repos/katl-dev/katl/rulesets/<id>` and
compare its definition fields with the corresponding file.

Use a version that will never be released, such as `0.0.0`. With a repository
credential other than the App's, push a final-style tag such as `v0.0.0`
and expect GitHub to reject it. Attempt to create
`release/0.0.0` and expect GitHub to reject it.

Confirm candidate-tag acceptance when the Release workflow creates the next
real `-rc.N` tag.
