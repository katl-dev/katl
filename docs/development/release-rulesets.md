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

## Check the prerequisites

1. No workflow starts on a tag push.
2. The `Release` workflow's publish job uses the `katl-release` App token to
   create final tags, and the cut workflow uses it to create release branches.
   Applying `final release tags` before the publish job uses the App makes
   every final publication fail. Applying `release branch creation` before the
   cut workflow uses the App makes release branches impossible to create.

In the repository's environment settings, check both environments:

- `release` admits only `release/*`, requires a reviewer, and disables
  administrator bypass.
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
and expect GitHub to reject it. Push the corresponding candidate tag
`v0.0.0-rc.1` and expect GitHub to accept it. Attempt to create
`release/0.0.0` and expect GitHub to reject it.

Have a repository administrator remove the accepted candidate test tag
afterward. The live tag ruleset blocks deletion, so the administrator must
temporarily exclude that exact test tag, delete it, and reapply
`release-tags.json` to restore the protection.
