# Verify KatlOS Release Artifacts

This is an optional expert workflow for operators who want to authenticate
downloaded artifacts against the Katl release pipeline. It is not a prerequisite
for installing or operating KatlOS on the normal trusted home-lab path.

When using it, download every asset in a verification set from one GitHub
release; never mix files from different tags.

## Inputs

- exact KatlOS tag, such as `v2026.7.0-alpha.2`;
- assets required for the chosen operation;
- `SHA256SUMS`; and
- `PROVENANCE.md`.

For an ISO install, the minimum payload set is:

```text
katl-installer.iso
katl-installer.iso.sha256
katlctl-<version>-linux-amd64
katlctl-<version>-linux-amd64.sha256
SHA256SUMS
PROVENANCE.md
```

For a host upgrade, use the matching
`katlos-upgrade-<version>-<arch>.squashfs` plus its adjacent `.json` and
`.sha256` files.

## Verify Integrity

Run from the directory containing the downloaded assets:

```sh
sha256sum --ignore-missing --check SHA256SUMS
```

Every downloaded file named by `SHA256SUMS` must report `OK`. An adjacent
checksum can verify one file, but it does not replace the release-wide manifest:

```sh
sha256sum --check katl-installer.iso.sha256
```

Stop if a digest fails. Delete the mismatched file and fetch it again from the
same release. Do not edit a release artifact or its metadata.

## Verify Build Provenance

Authenticate each executable or image asset against the exact tag and Katl
release workflow:

```sh
TAG=v2026.7.0-alpha.2
gh attestation verify katl-installer.iso \
  --repo katl-dev/katl \
  --signer-workflow katl-dev/katl/.github/workflows/release-artifacts.yml \
  --source-ref "refs/tags/$TAG"
```

Repeat for `katlctl`, a loose PXE artifact, or the upgrade SquashFS you will
actually use. Record the tag, source commit, filename, SHA-256, and whether
attestation verification passed.

## Confirm Release Identity

Install the matching CLI under its stable name and inspect its identity:

```sh
VERSION=2026.7.0-alpha.2
install -m 0755 "katlctl-$VERSION-linux-amd64" ~/.local/bin/katlctl
katlctl version
```

The CLI and KatlOS assets must come from the same release unless release notes
explicitly declare another combination supported.

## Trust Boundary

Checksums detect changed bytes. GitHub attestations bind bytes to a repository,
workflow, source ref, and commit. They do not provide Secure Boot signatures,
node-side signature enforcement, revocation, downgrade prevention, vulnerability
scanning guarantees, or a production incident-response commitment.

Kubernetes bundles have their own OCI digest and GitHub provenance. An operator
who wants exact byte identity can use a reference containing both the readable
tag and immutable manifest digest:

```text
ghcr.io/katl-dev/kubernetes:<version>@sha256:<oci-manifest-digest>
```
