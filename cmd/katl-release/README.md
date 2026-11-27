# katl-release

Use `scripts/katl-release` from the Katl checkout to prepare a release tag,
inspect and download its final GitHub Actions bundle, or verify release
evidence. The wrapper runs the checkout's Go source. Commands that consult
GitHub require an authenticated `gh` with access to `katl-dev/katl`.
The [release procedure](../../docs/development/releases.md) describes source
selection, qualification, and publication through GitHub Actions.

Run `scripts/katl-release --help` for command syntax. Only `prepare` defines
a subcommand flag, `--ref`. Its flag help is available with
`scripts/katl-release prepare --help`.

## Prepare and retrieve a candidate

### `prepare`

```sh
scripts/katl-release prepare --ref "$QUALIFIED_SHA" 2026.12.0-rc.1
```

Set `QUALIFIED_SHA` to the full commit SHA you intend to release. `--ref`
also accepts a published source ref and defaults to `main`. Pass the
embedded version without a `v` prefix, using `YYYY.M.PATCH` or one of the
`-dev.N`, `-alpha.N`, `-beta.N`, or `-rc.N` suffixes.

Preparation is read-only. It resolves the source on GitHub, refuses an
existing tag, and prints the tag and source SHA, the release guidance path,
and the Git or Jujutsu commands to create and push the tag after source
review. If the tag already exists, inspect its Actions run or choose a new
version.

### `inspect`

```sh
scripts/katl-release inspect "$RUN_ID"
```

Set `RUN_ID` to the positive integer in the Actions URL for the pushed
KatlOS tag. The command prints JSON with the run, attempt, source commit, and
final artifact metadata. It accepts only tag-push runs of
`release-artifacts.yml` and selects the final bundle named
`katl-VERSION-x86_64-COMMITPREFIX`, where the prefix is the first 12
characters of the source SHA.

A branch or manual run is refused. If the final bundle is unavailable, check
the run URL and status. If multiple final bundles match, resolve the ambiguity
before qualification. An expired bundle requires a fresh build and fresh
qualification.

### `download`

```sh
scripts/katl-release download "$RUN_ID" _build/releases/2026.12.0-rc.1
```

Use a destination that does not exist and create its parent directory first.
The command selects the same final artifact as `inspect`, downloads it,
verifies checksums and provenance, and prints the run and artifact JSON.
It checks the run attempt, commit, and artifact again after verification.
If a rerun replaced the candidate, or if the download was interrupted,
inspect the retained directory and download into a fresh directory before
qualifying the candidate.

## Verify release evidence

Set `BUNDLE` to the directory containing the final release files and
`SHA256SUMS`. For commands that take a commit, supply the full source SHA.

### `verify`

```sh
scripts/katl-release verify v2026.12.0-rc.1 "$QUALIFIED_SHA" "$BUNDLE"
```

Use `verify` on an existing bundle. It checks the files as `checksums`
does, then verifies provenance for every asset and `SHA256SUMS`. The
attestations must match `katl-dev/katl`, its release workflow, the exact
tag ref, and the exact source commit. Check those identities if verification
fails. For trusted-root fetch errors, check network access to
`tuf-repo-cdn.sigstore.dev` and `tuf-repo.github.com`. For per-file
attestation errors, check `gh attestation verify` and network access.

### `checksums`

```sh
scripts/katl-release checksums "$BUNDLE"
```

This local check requires a nonempty, flat bundle of regular files. It refuses
malformed checksum entries, unsafe or duplicate filenames, missing or
unlisted files, and digest mismatches. Recover the complete bundle from its
source if the files fail verification.

### `equivalent`

```sh
scripts/katl-release equivalent "$BUNDLE" \
  "standard=$STANDARD_SUMS" "lts=$LTS_SUMS"
```

Set `STANDARD_SUMS` and `LTS_SUMS` to the `SHA256SUMS` files from the
production VM qualification of each flavour. The command verifies the bundle
and requires matching qualified digests for all published files other than
the rendered `RELEASE_NOTES.md`. Both flavour entries are required. Unknown
or duplicate flavours, missing files, and mismatched or unqualified bytes
fail the check. Use the exact qualified artifacts, or qualify the changed
artifacts again.

### `regression`

```sh
scripts/katl-release regression 2026.12.0-rc.1 "$QUALIFIED_SHA"
```

The tag workflow uses this command to decide whether to repeat VM regression.
It prints `skip` only for an RC or final version when successful merge-queue
CI on the exact commit ran regression for both flavours with the default
Kubernetes payload and the commit is reachable from `main`. Missing or
unreadable CI evidence yields `run`. A final version also requires a
successful matching RC tag run on the same commit. If that evidence is
missing, the command fails. Qualify an RC on that commit before rerunning
the final release workflow. Production VM qualification remains part of
the tag workflow.

### `record`

```sh
scripts/katl-release record 2026.12.0-rc.1 qualified "$SHA256SUMS_DIGEST"
```

The Release workflow uses this command to write the
[release record](../../docs/internal/release-state-machine.md#records) of a
candidate or final version. The phase is `building`, `failed` or `qualified`;
only `qualified` takes the SHA-256 of the bundle's `SHA256SUMS` as 64
lowercase hexadecimal digits. The command reads the run attempt from
`GITHUB_SERVER_URL`, `GITHUB_REPOSITORY`, `GITHUB_RUN_ID` and
`GITHUB_RUN_ATTEMPT`, so it runs only in GitHub Actions, and it writes with
the workflow token, which needs `statuses: write`. A record is trusted only on
its run attempt's commit and from a run on the version's release branch, so
the command writes on `GITHUB_SHA` and refuses unless `GITHUB_REF` is
`refs/heads/release/<final version>` for the version given, such as
`refs/heads/release/2026.12.0` for `2026.12.0-rc.1`.

## Release state

The [release state machine](../../docs/internal/release-state-machine.md)
defines both commands below. They share one decision, read from Git and
GitHub on every call; a read that fails stops the command without a
decision.

### `status`

```sh
scripts/katl-release status 2026.12.1
```

Read-only. Prints the state of `release/2026.12.1`, the step a dispatch
without a retry reason would take or the refusal it would give, the
candidate's and final's records with their run attempts and digests, and a
pending approval with its URL. Statuses that look like records but fail the
trust rule are listed as warnings. On a repository without the Release
workflow it fails, saying so. Run it from a checkout of the release
branch's head, because the workflow decides with that branch's own
`katl-release`.

### `next`

```sh
scripts/katl-release next --ref "$GITHUB_REF" --dispatch "$GITHUB_SHA" --retry "$RETRY"
```

The Release workflow's decide job runs this and acts on its JSON output.
`--ref` and `--dispatch` default to `GITHUB_REF` and `GITHUB_SHA`, and the
run named by `GITHUB_RUN_ID` is not counted as an active run. It exits zero
for every decision, a refusal included.

## Version grammar

These commands let scripts use the one owner of the
[calendar version](../../docs/internal/adrs/adr-009-katlos-calendar-versioning.md)
grammar instead of a pattern of their own.

### `version`

```sh
scripts/katl-release version 2026.12.1-rc.1
```

Fails, naming the problem, unless the argument is a canonical version
without the `v` prefix.

### `stable-tags`

```sh
scripts/katl-release stable-tags v2026.12.1 v2026.12.1-rc.1 nightly
```

Prints the arguments that are final release tags, in their order.

### `bundle-name`

```sh
scripts/katl-release bundle-name 2026.12.1-rc.1 "$GITHUB_SHA"
```

Prints the name of the bundle artifact that a build of the version at that
full commit uploads, such as `katl-2026.12.1-rc.1-x86_64-0123456789ab`.

## State and logs

GitHub Actions holds the run logs, artifact metadata, and final bundle.
The tool writes inspection JSON to stdout and download and verification
progress to stderr. Downloaded files, including any partial download, stay
in the directory you supplied. Verification fetches the Sigstore trusted root
once into a temporary `katl-release-*` directory outside the bundle and removes
it when the check finishes. The GitHub workflow publishes after its
validation gates pass.
