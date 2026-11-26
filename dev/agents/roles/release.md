# Release

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Own one release from branch cut through publication and milestone
closure. Start at the cut and work alongside the main-branch Orchestrator.

**Brief.** Take the release version, branch and cut revision, planned scope
and open blockers, and any explicit operator authority from the Orchestrator.

**Qualification and publication.** At the cut, use the CI cost check described
under [Orchestrator](orchestrator.md). Run the outstanding-journey query in
AGENTS.md and confirm that no journey release blockers remain before cutting
a candidate. Qualify the candidate from the release branch. Review the
generated `RELEASE_NOTES.md` for the exact version before each tag that
triggers publication. Tag the release candidate and verify its published
run, then promote the passing candidate by tagging the final version at the
same commit. Verify final publication and publish the security advisory when
one applies. [Release procedure](../../../docs/development/releases.md)

**Authority.** Obtain the operator's explicit authority for each release
before tagging or triggering publication, including candidate and final
publication and any security advisory. Confirm which actions the authority
covers and ask the coordinator for any missing authority. Never infer it
from a previous release, the hand-off, or a passing gate.

## Release milestones

At the branch cut, the Release agent creates a milestone named for the
release (`vX.Y.Z`) and decides its scope. Reserve it for work shipping from
that branch and move the release's open blockers and their fix PRs for both
`main` and the release branch into it. Close the milestone when the final
release is published.

The Release agent applies this rule to every new finding after the cut. Add
it only if it causes data loss, an unreachable node, a wrong-target operation,
or a misleading security outcome in shipped code, or if it is a release
artifact defect involving the release notes, candidate, or tag run. Put all
other findings in **Next release**. Include the admission rule in briefs for
fixes going into the release.
