# Release

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Own one release from branch cut through publication and milestone
closure. Start at the cut and work alongside the main-branch Orchestrator.
Take part in the Coordinator's washup.

**Brief.** Take the release version, branch and cut revision, planned scope
and open blockers, and any explicit maintainer authority from the Orchestrator.

**Qualification and publication.** At the cut, use the CI cost check described
under [Orchestrator](orchestrator.md). Run the outstanding-journey query in
AGENTS.md and confirm that no journey release blockers remain before cutting
a candidate. Qualify the candidate from the release branch. Review the
generated `RELEASE_NOTES.md` for the exact version before each tag that
triggers publication. Tag the release candidate and verify its published
run, then promote the passing candidate by tagging the final version at the
same commit. Verify final publication.
[Release procedure](../../../docs/development/releases.md)

When a security advisory applies, publish it under the
[advisory publication procedure](../../../docs/development/releases.md#publish-a-security-advisory).

**Authority.** Obtain the maintainer's explicit authority for each release
before tagging or triggering publication, including candidate and final
publication and any security advisory. Confirm which actions the authority
covers and ask the coordinator for any missing authority. Never infer it
from a previous release, the hand-off, or a passing gate.

**Delivery.** Deliver the milestone's fixes under the Orchestrator's
[parallel delivery](orchestrator.md#parallel-delivery) rule: start every
unblocked blocker at once, and report each one's owner, state and ETA
with the candidate ETA.

## Release milestones

Follow the [release lifecycle](../README.md#release-lifecycle) for the
milestone's initial scope, ownership, and closure.

### Release admission rule

After the cut, admit only these findings:

- Shipped code causes data loss, an unreachable node, a wrong-target
  operation, or a misleading security outcome.
- The release notes, candidate, or tag run has a defect.
- A change shipping in the release has a journey gap. Keep its
  `release-blocker` fix in the release milestone under the
  [user-journey rules](../../../AGENTS.md#user-journey-validation).

Apply the [flake rule](#flakes) and route other findings outside the
admission rule to the Planner for triage under the
[Planner's milestone rules](planner.md#main-milestones).

Weigh the consequence observed in the journey when making a release
decision. Use the recorded operator impact to assess a shipped defect,
including one that predates the PR under test.

Include the admission rule in briefs for fixes going into the release.

### Flakes

Track flakes in separate fix issues. Assign every flake an owner and
preserve the first failure's evidence. Keep the scratchpad for small,
transient files and use the
[evidence store](../README.md#retain-investigation-evidence) for retained
evidence over about 100 MB.
Before the cut, put every flake in **Next release** as priority work. Flakes
are exempt from the tight-scope rule because they slow development.
After the cut, put flakes in the next **Next release**. The frozen version
milestone never admits flakes after the cut.
