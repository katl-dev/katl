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

1. Release tags come only from the Release action, and no workflow at a commit
   made since the removal of the tag trigger starts on a tag push. Extending
   the tag creation ruleset to every release tag is tracked in
   [#1021](https://github.com/katl-dev/katl/issues/1021).
2. The `final release tags` ruleset names the `katl-release` App, which the
   `Release` workflow's publish job uses to create final tags. The
   `release branch creation` ruleset names the `katl-release-cut` App, which
   the **Cut release** workflow uses to create release branches. Applying
   `final release tags` before the publish job uses the `katl-release` App
   makes every final publication fail. Apply `release branch creation` only
   after the **Cut release** workflow uses the `katl-release-cut` App;
   otherwise release branches are impossible to create.

In the repository's environment settings, check both environments:

- `release` must admit only `release/[0-9]*`, require a reviewer, and disable
  administrator bypass. Narrow its deployment branch policy to `release/[0-9]*`
  before applying the rulesets.
- `release-cut` admits only `main` and disables administrator bypass.
- `release` stores the `katl-release` App's ID as the variable
  `KATL_RELEASE_APP_ID` and its private key as the secret
  `KATL_RELEASE_APP_PRIVATE_KEY`.
- `release-cut` stores the `katl-release-cut` App's ID as the variable
  `KATL_RELEASE_CUT_APP_ID` and its private key as the secret
  `KATL_RELEASE_CUT_APP_PRIVATE_KEY`.

Every job that reads an App ID runs in that App's environment, so the
environment's variable takes precedence over a repository variable of the same
name.

Before applying either file, check its bypass actor's ID. For
`final-release-tags.json` it must equal `KATL_RELEASE_APP_ID` in the `release`
environment, and for `release-branch-creation.json` it must equal
`KATL_RELEASE_CUT_APP_ID` in the `release-cut` environment.

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

Use a version that will never be released, such as `0.0.0`. With a credential
that is neither release App's, push a final-style tag such as `v0.0.0`
and expect GitHub to reject it. Attempt to create
`release/0.0.0` and expect GitHub to reject it.

Confirm candidate-tag acceptance when the Release workflow creates the next
real `-rc.N` tag.
