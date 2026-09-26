# Install katlctl

Install `katlctl` on the workstation you use to manage KatlOS. For an install
or upgrade, select the CLI source from the same Katl release as the KatlOS
artifacts. Check the installed CLI with `katlctl version`.

## Install with Homebrew

On a Linux amd64 workstation with Homebrew, install the beta channel from the
[Katl Homebrew tap](https://github.com/katl-dev/homebrew-katlctl):

```sh
brew install katl-dev/katlctl/beta
katlctl version
```

The beta formula follows the newest eligible beta or stable release. To keep a
specific release, install its versioned formula instead. For example:

```sh
brew install katl-dev/katlctl/beta@2026.9.0-beta.16
katlctl version
```

Versioned formulas are available from `2026.9.0-beta.16` onward. When the
stable channel is published, use `brew install katl-dev/katlctl/stable` to
follow stable releases. Uninstall the installed formula before switching
channels or versions because each formula provides the same `katlctl` command.

## Install with Nix

The Katl repository's flake builds `katlctl` for `x86_64-linux` and
`aarch64-darwin`. On a host with Nix flakes enabled, install the CLI from the
repository's main branch to try unreleased changes:

```sh
nix profile add github:katl-dev/katl#katlctl
katlctl version
```

For a KatlOS install or upgrade, pin a release tag that includes the `katlctl`
flake package. Replace `RELEASE_VERSION` with the version of your KatlOS
artifacts:

```sh
VERSION=RELEASE_VERSION
nix profile add "github:katl-dev/katl/v${VERSION}#katlctl"
katlctl version
```

The Nix-built CLI reports the pinned Git commit rather than the release version.
The flake package installs Bash, Fish, and Zsh completion scripts alongside
`katlctl`. The flake's development shell also enables Bash completion.

The `v2026.9.0-beta.16` tag does not include this flake package; for that
release, use Homebrew or the [release binary](installing.md#artifacts).
