# Release KatlOS

A release tag starts the build, qualification, and promotion pipeline. GitHub
publishes the built artifacts only after **Release validation** passes. No
separate publication approval is required.

## Prepare a release

1. Merge the intended changes and their required checks. Choose an unused
   canonical `YYYY.M.PATCH` version, optionally with `-alpha.N`, `-beta.N`, or
   `-rc.N`. Prerelease versions receive GitHub's prerelease designation.
2. Add `docs/releases/VERSION.md` when operators need upgrade instructions,
   required actions, compatibility notices, known limitations, or recovery
   guidance. Otherwise commit subjects supply the release notes.
3. Resolve the source and print the tag commands:

   ```sh
   scripts/katl-release prepare 2026.10.2
   ```

   Preparation reads GitHub's `main`, rejects an existing tag, and prints the
   exact source SHA and commands for the checkout's version-control system. Use
   `--ref REF` before the version to select another published source.
4. Review the source and run the printed commands. Do not move or reuse a
   release tag. Follow the tag run in
   [Katl Release Artifacts](https://github.com/katl-dev/katl/actions/workflows/release-artifacts.yml).

A manual dispatch with `version` builds and qualifies a candidate without
publishing. A tag still qualifies its own built bytes before promotion.

## Qualification and promotion

Both standard and LTS flavours must pass:

- Runtime, installer, extension closure, module composition, image integrity,
  and publication-bundle checks.
- The [VM regression suite](ci.md), including bootstrap, configuration,
  management, storage, and update/rollback journeys.
- Production-image installation and reboot through SSH and the release
  `katlctl`, with management and persisted boot health checked independently.
- Upgrade from the previous stable release to the candidate and rollback to
  the source generation, preserving machine identity. These tests use the
  production images without a VM test agent.

Qualification downloads the final publication bundle from the same run and
verifies its checksums. Publication consumes those artifacts without rebuilding.
Skipped, failed, or cancelled required VM tests prevent promotion. Successful
tag runs publish the extension OCI bytes and GitHub release assets, then verify
attestations against the source commit and tag.

These VM gates do not establish NVIDIA hardware behavior or replace focused
hardware and workload qualification for changes that affect those paths.

## Inspect or diagnose a candidate

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
rerunning. Guest disks are excluded from retained artifacts. Fix the defect and
repeat the affected gates; a rebuilt candidate must qualify its own bytes. Do
not bypass **Release validation** or promote a different run's artifacts.
