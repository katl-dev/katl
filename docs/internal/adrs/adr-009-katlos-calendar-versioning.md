# ADR-009: KatlOS releases use calendar versions

Status: accepted.

Date: 2026-07-11.

## Context

The v0.1 project milestone needs public development, release-candidate, and
stable artifact identities. `v0.1` describes the milestone scope, but using it
as the product version would discard the requested date-based release identity.

The version is embedded in KatlOS image names and metadata, Go binaries,
release-branch artifacts, Git tags, and GitHub Releases. It must sort in a
predictable maturity order, be accepted by ordinary version tooling, and stay
independent from Kubernetes and node-extension payload versions.

## Decision

KatlOS uses calendar versions in this SemVer-compatible form:

```text
YYYY.M.PATCH
YYYY.M.PATCH-dev.N
YYYY.M.PATCH-rc.N
```

`YYYY.M` identifies the calendar release line. The month is not zero-padded.
`PATCH` starts at `0` and increments when more than one stable KatlOS release is
cut in the same month. A release keeps its original version after the month
changes; the version records its release line, not the current date.

`dev.N` identifies development publications and `rc.N` identifies release
candidates. Both sequences start at `0` and increment for materially different
published artifacts. Development builds precede release candidates, which
precede the stable release:

```text
2026.7.0-dev.0
2026.7.0-dev.1
2026.7.0-rc.0
2026.7.0-rc.1
2026.7.0
```

The v0.1 project milestone is therefore a scope milestone, not the literal
KatlOS product version. Its first development publication is
`2026.7.0-dev.0`, and its first release candidate is `2026.7.0-rc.0`.

Git tags use a leading `v`, for example `v2026.7.0-rc.0`. Release branches use
the exact product version after `release/`, for example
`release/2026.7.0-rc.0`. The release tooling accepts an optional `v` in either
place, removes it from embedded artifact metadata, and rejects zero-padded
months, leading-zero counters, other prerelease labels, and non-calendar
versions. Manual workflow runs use the product version without the `v`.

The calendar version identifies a KatlOS release as a whole. Kubernetes payload
bundles and node-extension bundles retain their independent payload versions
and compatibility metadata; they do not inherit the KatlOS calendar version.

## Consequences

Release automation can distinguish development builds and release candidates
using the prerelease suffix while retaining one date-based stable identity.
Standard SemVer comparison places `dev` before `rc` and both before the stable
version for the same release line.

Cutting a new month does not automatically renumber unreleased artifacts. The
release owner chooses the target calendar line, then increments its development
or release-candidate counter for every materially different publication.

The workflow intentionally rejects loose labels such as `nightly`, literal
milestone versions such as `0.1.0`, zero-padded forms such as `2026.07.0`, and
build metadata that is not part of this initial release contract.

## Rejected Alternatives

`0.1.0-dev.N` and `0.1.0-rc.N` were rejected because they make the milestone
name the product version and are not date-based.

`YYYY.MM.DD` was rejected because the day would force a new stable identity for
multiple candidates built across days and gives no natural same-day release
sequence.

Date-stamped prerelease labels such as `YYYY.M.PATCH-dev.YYYYMMDD` were rejected
because they repeat the calendar identity and still need an extra sequence for
multiple publications on one day. Numeric `dev.N` and `rc.N` counters express
maturity directly.
