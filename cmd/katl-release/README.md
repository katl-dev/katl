# katl-release

Use `scripts/katl-release` from the Katl checkout to run and follow a release,
inspect and download its GitHub Actions bundle, or verify release evidence.
The wrapper runs the checkout's Go source. Commands that consult
GitHub require an authenticated `gh` with access to `katl-dev/katl`.
The [release procedure](../../docs/development/releases.md) describes source
selection, qualification, and publication through GitHub Actions.

Run `scripts/katl-release --help` for command syntax.

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

Set `RUN_ID` to the positive integer in the Actions run URL. The command
accepts a dispatched Release run on `release/VERSION` or a pushed KatlOS
tag run from `release-artifacts.yml`. It prints JSON with the run, attempt,
source commit, and bundle artifact metadata. For a Release run, the bundle
must be for that branch's final version or one of its release candidates.

If the bundle is unavailable, check the run URL and status. If multiple
bundles match, resolve the ambiguity before qualification. An expired bundle
requires a fresh build and fresh qualification.

### `download`

```sh
scripts/katl-release download "$RUN_ID" _build/releases/2026.12.0-rc.1
```

Use a destination that does not exist and create its parent directory first.
The command selects the same final artifact as `inspect`, downloads it,
verifies checksums and provenance, and prints the run and artifact JSON.
It verifies provenance against the source commit and release branch ref for
a Release run, or against the source commit and tag ref for a tag-push run.
You can download the final bundle while publication waits for approval.
The command checks the run attempt, commit, and artifact again after verification.
If a rerun replaced the candidate, or if the download was interrupted,
inspect the retained directory and download into a fresh directory before
qualifying the candidate.

## Verify release evidence

Set `BUNDLE` to the directory containing the final release files and
`SHA256SUMS`. For commands that take a commit, supply the full source SHA.

### `verify`

```sh
scripts/katl-release verify v2026.12.0-rc.1 "$QUALIFIED_SHA" "$BUNDLE"
scripts/katl-release verify --branch v2026.12.0-rc.1 "$QUALIFIED_SHA" "$BUNDLE"
```

Use `verify` on an existing bundle. It checks the files as `checksums`
does, then verifies provenance for every asset and `SHA256SUMS`. The
attestations must match `katl-dev/katl`, its release build workflow, and the
exact source commit. Use the default tag ref for bundles built by the legacy
tag workflow. Use `--branch` for bundles built by **Release**, which uses
the branch of the final version for both candidate and final builds. For
trusted-root fetch errors, check network access to
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

The command checks CI evidence for the exact source commit. **Fast checks**
must succeed in a CI merge-queue run for a commit that landed on `main`, or
in a successful CI push or manual run on `release/<base-version>`. For
example, `2026.12.0-rc.1` uses `release/2026.12.0`. If Fast checks evidence
is missing, pending, failed, or unreadable, the command exits nonzero and
prints recovery instructions to stderr.

When Fast checks pass, the command prints one JSON object to stdout. Its
`decision` is `skip` if the merge-queue run's latest attempt passed VM
regression for both flavours with the default Kubernetes payload. Then
`vm_flavours` is `[]` and `evidence` is the merge-queue run URL. If the VM
evidence is missing, incomplete, or unreadable, `decision` is `run`,
`vm_flavours` is `["standard","lts"]`, and `evidence` is an empty string.
Production VM qualification remains part of the tag workflow.

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
defines the release decisions. The commands share one decision, read from Git and
GitHub on every call; a read that fails stops the command without a
decision.

### `release`

```sh
scripts/katl-release release 2026.12.1
scripts/katl-release release 2026.12.1 --retry 'runner lost during Production VM (lts)'
```

Start from a checkout of the head of the published release branch and supply
its complete final version without the `v` prefix. The command prints the
decision and dispatches Release only if the state machine allows it. Supply
`--retry` with a reason when the decision calls for retrying a failed build.
After dispatch, the command prints the run URL and follows it through the
same steps as `wait`.

### `wait`

```sh
scripts/katl-release wait 2026.12.1
```

Follow the newest Release run for the version, including one that has already
completed. The command reports job progress and prints the approval URL when
a final run is waiting. It keeps following until the run ends. If the run
fails, the command exits with the failed jobs and run URL. After a successful
run, it checks the publication against the release record and reports the
published tag and digest, or that a new commit superseded the candidate.

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

## Check the release credential

After cutting `release/<version>` and before dispatching a candidate or final
release, run the credential check with the release branch as `--ref`.

```sh
gh workflow run release-credential.yml --ref release/2026.12.1
```

Approve the `release` deployment and confirm that the run succeeds. The check
verifies that the App slug gives the bot identity that `release-artifacts.yml`
skips on tag pushes. The run summary reports that the App minted a token scoped to
this repository with Contents write permission and that the token read
repository contents. Final publication is the first step that exercises the
App's final-tag ruleset bypass.

## Publish a qualified bundle

The Release workflow uses these commands after qualification. Submit release
work through the [Release workflow](../../docs/internal/release-state-machine.md#actions).

### `require-candidate`

```sh
scripts/katl-release require-candidate 2026.12.1-rc.1
```

The pre-release job proceeds only if the decided version is a release
candidate numbered 1 or higher.

### `confirm-final`

```sh
scripts/katl-release confirm-final 2026.12.1 "$QUALIFIED_SHA" "$SHA256SUMS_DIGEST"
```

After approval, the final **Publish** job checks that the state machine
still selects this version and commit and that the qualified digest belongs
to the current run attempt. If the branch moved or an extension reference
already exists, the command reports the state machine's refusal.

### `draft`

```sh
scripts/katl-release draft 2026.12.1-rc.1 "$QUALIFIED_SHA" "$BUNDLE"
```

Verifies the bundle against the release branch, then creates a draft,
uploads the bundle and checks the uploaded asset digests. If a transient
failure interrupts drafting, the command starts over with a fresh draft of
the same tag. An already-published release with matching checksums completes
this step without a write.

### `publish`

```sh
scripts/katl-release publish 2026.12.1-rc.1 "$QUALIFIED_SHA" "$BUNDLE"
```

After pushing the qualified extensions, the workflow checks the draft's
asset digests and publishes it, creating the tag at the source commit if
needed. If the request fails, it checks whether GitHub published the release
and retries a transient failure only while the same verified draft remains.
It reads back the tag, release and checksum file. A candidate publishes as a
pre-release. An already-published matching release is verified without a write.

### `candidate-artifacts`

```sh
scripts/katl-release candidate-artifacts 2026.12.1-rc.1 "$QUALIFIED_SHA" "$RUN_ID" "$RUN_ATTEMPT"
```

When resuming a pre-release, the workflow uses the recorded run and attempt
numbers to find the bundle and both flavours' extension inputs. This
command prints their artifact IDs as JSON. If the bundle has expired, push
a commit to the release branch to qualify the next candidate.

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

### `tag-name`

```sh
scripts/katl-release tag-name 2026.12.1-rc.1
```

Prints the Git tag for the parsed version, such as `v2026.12.1-rc.1`.

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
