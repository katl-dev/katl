# Release KatlOS

A release tag starts the build, qualification, and promotion pipeline. GitHub
publishes the built artifacts only after **Release validation** passes. No
separate publication approval is required.

## Prepare a release

When cutting a release branch, follow the
[release milestone policy](../../dev/agents/roles/release.md#release-milestones) to track blockers
and triage findings through final publication.

1. Merge the intended changes and their required checks. Choose an unused
   canonical `YYYY.M.PATCH` version, optionally with `-alpha.N`, `-beta.N`, or
   `-rc.N`. Prerelease versions receive GitHub's prerelease designation.
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
   version being rendered.
3. Cut `release/<version>` from the selected `main` commit SHA, using the
   final version for the branch name. Dispatch qualification from that branch and
   wait for it to pass. Production-image checks run only in this workflow, so
   qualify the commit before tagging it.

   ```sh
   gh workflow run release-artifacts.yml --ref release/2026.10.2 -f version=2026.10.2-rc.1
   ```

4. Set `QUALIFIED_SHA` to the head SHA of the passing qualification run and
   prepare the candidate tag.

   ```sh
   scripts/katl-release prepare --ref "$QUALIFIED_SHA" 2026.10.2-rc.1
   ```

   Preparation rejects an existing tag and prints the exact source SHA and
   commands for the checkout's version-control system. When preparing the final
   tag, pass the published, passing candidate's commit SHA with `--ref`.
5. Review the source and run the printed commands. Do not move or reuse a
   release tag. Follow the tag run in
   [Katl Release Artifacts](https://github.com/katl-dev/katl/actions/workflows/release-artifacts.yml).
6. After the final release is published, open a small PR against `main` to set
   `scripts/production-upgrade-baseline.txt` to the published stable tag,
   including its `v` prefix. This sets the source release for Production VM
   upgrades and release notes. In the same PR, move `docs/releases/next.md` to
   `docs/releases/VERSION.md` if it exists to keep the published guidance for
   that tag.

A manual dispatch with `version` builds and qualifies a candidate without
publishing. A tag still qualifies its own built bytes before promotion.
Only a tag push can publish; a dispatch never reaches the publication jobs.

The release workflow repeats the VM regression suite for a release candidate
or final version only when CI has not already passed it at the tagged commit.
`scripts/katl-release regression VERSION COMMIT` decides, and prints `skip`
only when a successful `CI` run of the merge queue produced the exact commit on
`main` and ran every VM job of both flavours to success on the default
Kubernetes payload. A merge-queue run for a change that selects no VM suite,
such as documentation, or that tested a presubmit Kubernetes candidate, does
not qualify.
Pushes, pull requests, other versions, and any lookup that fails run the suite.
Release validation accepts skipped regression lanes only on that decision.
Only tag pushes may skip; scheduled and dispatched runs always repeat the
suite. Production build and qualification always run, because the bytes embed the
version. A final version also requires a successful release run of one of its
`-rc.N` tags at the same commit; without one the run
fails before building, so
tag a candidate first and tag the final version once it publishes.
Do not re-run the whole workflow run of a published `-rc.N` tag: it fails at
the upload guard and makes that candidate ineligible for promotion. Tag a new
`-rc.N` at the same commit instead.

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
Skipped, failed, or cancelled required VM tests prevent promotion. Successful
tag runs publish the extension OCI bytes and GitHub release assets. Asset
attestations must match the source commit and tag before GitHub release
publication.

These VM gates do not establish NVIDIA hardware behavior or replace focused
hardware and workload qualification for changes that affect those paths.

## Inspect a release or diagnose qualification

For a tag run:

```sh
scripts/katl-release inspect RUN_ID
mkdir -p _build/releases
scripts/katl-release download RUN_ID _build/releases/2026.10.2 \
  > _build/releases/2026.10.2-receipt.json
```

Inspection reports the tag, source SHA, run attempt, and final artifact identity.
Download requires a fresh directory and verifies checksums and GitHub
attestations. Review `RELEASE_NOTES.md` and `SUPPORT.md` for the selected release.

When qualification fails, inspect the failed job's small VM evidence before
rerunning. To exercise the production journeys against the same built bundle
with the selected workflow revision, run:

```sh
gh workflow run release-artifacts.yml --ref main -f version=VERSION -f qualify_run=RUN_ID
```

This diagnostic run reuses the immutable bundle and ignores VERSION, which
the dispatch form requires. Production VM runs once per flavour using the
upgrade baseline pinned at the commit that produced the bundle.
Tag promotion still requires all gates in the tag's own build run.

Guest disks are excluded from retained artifacts. Fix the defect and
repeat the affected gates; a rebuilt candidate must qualify its own bytes. Do
not bypass **Release validation** or promote a different run's artifacts.
