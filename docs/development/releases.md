# Release KatlOS

Use this procedure to prepare and qualify a KatlOS release, then explicitly
approve publication of the tested artifacts. The existing release workflow owns
builds, attestations, and publication. `scripts/katl-release` reads GitHub state
and verifies downloads; it does not push tags, approve deployments, or publish.

## Prerequisites

- Go and an authenticated GitHub CLI (`gh auth status`).
- Maintainer access to `katl-dev/katl` and permission to review its `release`
  environment. That environment must have required reviewers; a single
  maintainer can approve their own run if self-review is permitted.
- A capable libvirt host for the applicable [VM gates](../developing.md#vm-tests-on-a-capable-host)
  and a persistent `katldev` VM for hands-on qualification.
- A release Bead for the version, source commit, run and artifact identities,
  commands, observations, failures, and approval decision. Keep this evidence
  local. Do not put transient qualification logs in release notes.

## Prepare the source and version

1. Choose a canonical `YYYY.M.PATCH` version, optionally with `-dev.N`,
   `-alpha.N`, `-beta.N`, or `-rc.N`. Use an unused version. Prereleases receive
   GitHub's prerelease designation.
2. Finish the intended changes and their required gates. Merge any operator
   guidance into `docs/releases/VERSION.md` before tagging. Include verified
   upgrade instructions, breaking changes, known limitations, or recovery
   guidance when needed. The release generator appends this file to the
   commit-derived release notes; it is optional when commit entries suffice.
3. Check the release plan. For example:

   ```sh
   scripts/katl-release prepare 2026.10.2
   ```

   Preparation resolves GitHub's `main` to a full commit SHA, rejects an existing
   tag, and checks for required reviewers on the `release` environment. To select
   another published source, put `--ref REF` before the version. This command
   makes no remote changes and prints commands for your checkout's version
   control system.
4. Review the selected source, required checks, and release workflow. The source
   must include the protected **Approve Release Publication** job. Confirm that
   `origin` points to `katl-dev/katl`, then run the printed fetch, tag, and push
   commands. They tag the exact SHA, so subsequent changes to `main` do not
   change the candidate. Do not move or reuse a release tag.

Pushing the tag starts the candidate build. A manual dispatch or a release branch
can test packaging, but it does not replace qualification of the tag-built bytes.

## Download the candidate

1. Find the pushed tag in [Katl Release Artifacts](https://github.com/katl-dev/katl/actions/workflows/release-artifacts.yml).
   Use the numeric run ID from its URL. Inspect it:

   ```sh
   scripts/katl-release inspect RUN_ID
   ```

   Inspection rejects branch builds, manual builds, unrelated workflows,
   missing or expired final bundles, and ambiguous bundles. It reports the
   commit, run attempt, final artifact ID, name, archive digest, and run status.
   If the bundle is unavailable, follow the run URL to check progress or failure.
2. Wait for **Approve Release Publication**. Download into a fresh directory
   whose parent already exists, saving the JSON receipt outside that directory:

   ```sh
   mkdir -p _build/releases
   scripts/katl-release download RUN_ID _build/releases/2026.10.2 \
     > _build/releases/2026.10.2-receipt.json
   ```

   Download verifies that `SHA256SUMS` covers every bundle file and that every
   checksum matches. It then verifies every file's GitHub attestation against
   the release workflow, exact tag, and source commit, including `SHA256SUMS`
   itself. It rechecks the run attempt and artifact identity before emitting a
   receipt. Progress and errors go to stderr; a failed verification emits no
   receipt and retains downloaded files for diagnosis. Retry in a fresh
   directory after resolving the failure.
3. Review the downloaded `RELEASE_NOTES.md`, `SUPPORT.md`, component inventories,
   and metadata. Confirm that they describe the version and support surface
   you intend to publish. Record the receipt in the release Bead.

Integrity and provenance checks do not prove that the OS boots, installs,
upgrades, or supports the documented operator journey.

## Qualify the exact artifacts

Run the applicable automated VM gates on a capable host, as documented in
[Developing Katl](../developing.md#vm-tests-on-a-capable-host). Instrumented VM
test builds provide regression evidence; they do not replace qualification of
the downloaded production artifacts.

The release-artifact workflow runs the full hosted VM regression suite for main
and candidate builds and requires success before publication approval. Review
its selected tests and outcomes as part of qualification. See
[Hosted VM scenarios](../internal/hosted-vmtests.md) for selection, artifacts,
evidence, and the remaining production-artifact qualification boundary.

Use the downloaded `katlctl` and production images for the affected public
journeys in a persistent `katldev` VM. Follow the supported
[development VM workflow](../developing.md) and public operator documentation.
Cover the release's supported install and management paths, including:

- Install from the downloaded ISO, boot the installed system, and verify
  management access and release-critical services.
- Bootstrap Kubernetes, install the selected CNI and a representative workload,
  and observe cluster health through the public interfaces.
- Apply representative configuration, repeat the apply, and verify the Linux,
  systemd, and persisted state before and after reboot where applicable.
- Upgrade from the supported previous release, verify workloads and management
  access, and exercise applicable rollback or recovery paths.
- Exercise both standard and LTS flavours and any additional advertised paths,
  including extension selection, PXE, Kubernetes upgrades, or wipe/reinstall
  when affected by the release.

Record exact commands, artifact identities, and independently observed outcomes
in the Bead. A success message alone is insufficient. Inspect and preserve a
failure before reinstalling; fix product gaps and repeat the affected gates.
If the host lacks a required capability, record the gap and outstanding commands
and leave the release unapproved.

## Approve and verify publication

1. Run `scripts/katl-release inspect RUN_ID` again. Compare the commit, run
   attempt, artifact ID, and artifact digest with the download receipt. If any
   changed, download and qualify again. Confirm that the run is waiting at
   **Approve Release Publication** and that all required qualification passed.
2. In that exact Actions run, select **Review deployments**, select `release`,
   and approve. Include the release Bead reference in your approval comment.
   This is the explicit publication decision. GitHub then publishes the existing
   extension bundles and GitHub release assets without rebuilding them.
3. Monitor the run to completion:

   ```sh
   gh run watch RUN_ID --repo katl-dev/katl --exit-status
   gh release view v2026.10.2 --repo katl-dev/katl
   ```

   Confirm the title, prerelease designation, generated notes, and expected
   assets. Verify the public downloads using
   [Verify release artifacts](../operations/verify-release.md), and confirm that
   advertised extension and Kubernetes bundles are anonymously accessible.
   Record the publication URL and observations before closing the release Bead.

The build artifacts and extension publication inputs are retained for 30 days.
Complete qualification and approval within that window.

## Recover a failed candidate or publication

- **Source or product defect:** preserve evidence, reject the pending deployment,
  merge the fix, and prepare an unused version. Never retarget the failed tag.
- **Expired artifacts or rerun producer jobs:** the previous qualification does
  not apply. Download the resulting bundle and qualify it again, even when the
  source commit is unchanged. A run ID alone does not identify the tested bytes.
- **Publication job failure:** inspect which extensions and GitHub assets became
  public before rerunning anything. Prefer rerunning only the failed publication
  jobs, retaining the qualified producer outputs. Do not bypass the approval
  environment or replace public assets. The workflow does not overwrite existing
  GitHub asset names; an interrupted upload can require investigation rather
  than an automatic retry.
- **Missing provenance or corrupt downloads:** keep the failed directory and run
  details, diagnose the failure, and verify a fresh download before qualification.
  If `gh` cannot initialize its Sigstore verifier, check its network access and
  ensure its cache directory is writable. Do not skip attestation verification.
