# Release KatlOS

A release tag starts the build, qualification, and promotion pipeline. GitHub
publishes the built artifacts only after **Release validation** passes. No
separate publication approval is required.

## Prepare a release

Follow the [release lifecycle](../../dev/agents/README.md#release-lifecycle)
to cut the branch and track blockers. Triage findings through final
publication under the
[release admission rule](../../dev/agents/roles/release.md#release-admission-rule).

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
   version being rendered that has a published, non-prerelease GitHub release.
3. Cut `release/<version>` from the selected `main` commit SHA, using the
   final version for the branch name.
   [Check the release credential](../../cmd/katl-release/README.md#check-the-release-credential)
   before dispatching a candidate or final release. Dispatch qualification
   from that branch and wait for it to pass. Production-image checks run only in this workflow, so
   qualify the commit before tagging it. Dispatch only after the branch
   exists, with `--ref release/<version>`. Until
   [#863](https://github.com/katl-dev/katl/issues/863) loads the modules in
   the workflow, check each extension of the passing candidate against its
   runtime (kernel release, bound runtime digest, and unresolved symbols)
   and record the result on the rc tag issue.

   ```sh
   gh workflow run release-artifacts.yml --ref release/2026.12.1 -f version=2026.12.1-rc.1
   ```

4. Set `QUALIFIED_SHA` to the head SHA of the passing qualification run and
   prepare the candidate tag.

   ```sh
   scripts/katl-release prepare --ref "$QUALIFIED_SHA" 2026.12.1-rc.1
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

For a tag push, `scripts/katl-release regression VERSION COMMIT` requires
successful **Fast checks** at the tagged commit. It returns a JSON decision
that skips the VM regression lanes only with complete merge-queue VM evidence
for that commit on `main`. See the
[regression command](../../cmd/katl-release/README.md#regression) for the
evidence rules and recovery instructions.

Scheduled and dispatched runs always repeat the suite. Production build and
qualification run for every version because the bytes embed the version.
Tag the final version at the candidate's commit after the candidate publishes.
If you need to build the candidate again, tag a new `-rc.N` at that commit.

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
scripts/katl-release download RUN_ID _build/releases/2026.12.1 \
  > _build/releases/2026.12.1-receipt.json
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
