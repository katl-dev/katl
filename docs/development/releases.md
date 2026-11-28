# Release KatlOS

A dispatch of the **Release** workflow on a release branch takes the next step
of that version's release: it builds, qualifies, tags, and publishes a
candidate, or rebuilds and publishes the final version. GitHub publishes the
built artifacts only after **Release validation** passes, and the final also
waits for approval in the `release` environment. Release tags come only from
the Release action, and no workflow at a commit made since the removal of the
tag trigger starts on a tag push.
[Release state machine](../internal/release-state-machine.md) defines how each
dispatch chooses its step.

## Prepare a release

Follow the [release lifecycle](../../dev/agents/README.md#release-lifecycle)
to cut the branch and track blockers. Triage findings through final
publication under the
[release admission rule](../../dev/agents/roles/release.md#release-admission-rule).

1. Confirm that the planned final version is unused and has an open
   `v<version>` milestone, then merge the intended changes and their
   required checks. Use canonical `YYYY.M.PATCH` for the final version.
   Versions with `-alpha.N`, `-beta.N`, or `-rc.N` receive GitHub's
   prerelease designation.
2. Add or update `docs/releases/next.md` when operators need upgrade
   instructions, required actions, compatibility notices, known limitations, or
   recovery guidance. The `Release-Note` trailers carried from PR bodies into
   squash commits supply the release entries. The notes append guidance from
   the first file that exists among `docs/releases/VERSION.md`,
   `docs/releases/BASE.md` for a prerelease's final version BASE, and `next.md`. A
   release candidate and its final release therefore render the same
   guidance from `next.md`. The notes use the upgrade baseline pinned at the
   release commit and omit commits reachable from that tag, including when
   the tag is on a sibling release branch. If the release commit has no pin
   for an earlier version, the notes use the highest stable tag below the
   version being rendered that has a published, non-prerelease GitHub release.
3. Cut the release branch from the selected commit on `main`. Use the
   complete final version and pass `main` or the full commit SHA.

   ```sh
   scripts/katl-release cut 2026.12.1 main
   ```

   The **Cut release** workflow creates the branch in the `release-cut`
   environment. The command follows the run and verifies the resulting branch
   head. The release branch creation ruleset,
   `.github/rulesets/release-branch-creation.json`, enforces this path: only
   the App that the workflow uses can create release branches. See the
   [release rulesets](release-rulesets.md).

   After the cut, check the journey gate with the previous final release tag,
   which `scripts/production-upgrade-baseline.txt` names on the release branch
   (`scripts/katl-release-artifacts upgrade-baseline`).

   ```sh
   scripts/katl-release journeys 2026.12.1 v2026.12.0
   ```

   A successful check prints that the gate is clear. If the command lists
   outstanding PRs or open journey gap fixes, resolve them and repeat the
   check before dispatching a candidate. Repeat the check after admitting
   changes to the release branch.

   [Check the release credential](../../cmd/katl-release/README.md#check-the-release-credential)
   before dispatching a candidate or final release.
4. Dispatch **Release** on the branch to build, qualify, tag, and publish the
   next candidate as a pre-release. `status` reports the step a dispatch would
   take when it runs from a checkout of the branch head, and `release`
   dispatches that step from such a checkout.

   ```sh
   scripts/katl-release status 2026.12.1
   scripts/katl-release release 2026.12.1
   ```

   `release` prints the run URL and follows the run until it finishes; the run
   also appears in
   [Release](https://github.com/katl-dev/katl/actions/workflows/release.yml).
   To retry a build that did not qualify, run
   `scripts/katl-release release 2026.12.1 --retry REASON`. Until
   [#863](https://github.com/katl-dev/katl/issues/863) loads the modules in
   the workflow, check each extension of the published candidate against its
   runtime (kernel release, bound runtime digest, and unresolved symbols)
   and record the result on the rc tag issue.
5. Once the candidate is releasable, dispatch **Release** on the branch again.
   It rebuilds the final version at the candidate's commit, and its publish
   job waits for approval in the `release` environment before it publishes the
   final release and its tag.
6. After the final release is published, open a small PR against `main` to set
   `scripts/production-upgrade-baseline.txt` to the published stable tag,
   including its `v` prefix. This sets the source release for Production VM
   upgrades and release notes. In the same PR, move `docs/releases/next.md` to
   `docs/releases/VERSION.md` if it exists to keep the published guidance for
   that tag.

The nightly schedule of `release-artifacts.yml` builds and qualifies a
development version of `main`, and a manual dispatch with `version` builds and
qualifies that version of the dispatched ref. Neither publishes anything.

Every Release build repeats the VM regression suite for both flavours.
Production build and qualification always run, because the bytes embed the
version.

## Publish a security advisory

After verifying final release publication, update the draft advisory. Put
references in the description because the advisory API has no `references`
field. Read the draft back and verify the patched versions, vulnerable range,
description, and references. Publish only after verification succeeds, in a
separate operation from the update.

[#778](https://github.com/katl-dev/katl/issues/778) tracks a command for this
procedure.

## Qualification and promotion

Both standard and LTS flavours must pass:

- Runtime, installer, extension closure, module composition, image integrity,
  and publication-bundle checks.
- The [VM regression suite](ci.md), including bootstrap, configuration,
  management, storage, and update/rollback journeys.
- Production-image installation and reboot through SSH and the release
  `katlctl`, with management and persisted boot health checked independently.
- Upgrade from the pinned stable release to the candidate and rollback to
  the source generation, preserving machine identity. These tests use the
  production images without a VM test agent.

Each flavour is built by the reusable `production-build.yml` workflow. Its own
Production VM job (`qualification-standard`, `qualification-lts`) qualifies the
assembled bytes as soon as that build ends, while the other flavour and the
publication bundle are still building, and uploads the checksums it verified. Release validation
additionally requires those qualified checksums to equal the bundle's; only
`RELEASE_NOTES.md`, which the bundle renders, is exempt. Publication consumes
those artifacts without rebuilding.
Skipped, failed, or cancelled required VM tests prevent promotion. Release
publishes the extension OCI bytes and GitHub release assets of a qualified
build. Asset attestations must match the source commit and release branch
before GitHub release publication.

These VM gates do not establish NVIDIA hardware behavior or replace focused
hardware and workload qualification for changes that affect those paths.

## Inspect a release or diagnose qualification

A Release run's summary reports the version, source commit, and `SHA256SUMS`
digest of the bundle it qualified. To check a bundle downloaded from the run's
`katl-VERSION-x86_64-COMMITPREFIX` artifact, run:

```sh
scripts/katl-release verify --branch v2026.12.1-rc.1 "$COMMIT" _build/releases/2026.12.1-rc.1
```

Verification checks checksums and GitHub attestations. Review
`RELEASE_NOTES.md` and `SUPPORT.md` for the selected release.

When qualification fails, inspect the failed job's small VM evidence before
retrying. Guest disks are excluded from retained artifacts. Fix the defect and
dispatch **Release** again; a rebuilt candidate must qualify its own bytes. Do
not bypass **Release validation** or promote a different run's artifacts.
