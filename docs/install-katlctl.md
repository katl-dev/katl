# Install katlctl

Install `katlctl` on the workstation you use to manage KatlOS. Choose the
same release as the KatlOS files you use for an install or upgrade.

## Install the release binary

Go to [KatlOS releases](https://github.com/katl-dev/katl/releases).
Download the file that matches your workstation.

- On Linux x86-64, download `katlctl-<version>-linux-amd64`.
- On an Apple Silicon Mac, download `katlctl-<version>-darwin-arm64`.

In the download directory, replace `RELEASE_VERSION` with the version from
the filename. If you use an Apple Silicon Mac, set `PLATFORM=darwin-arm64`.

```sh
VERSION=RELEASE_VERSION
PLATFORM=linux-amd64
mkdir -p "$HOME/.local/bin"
install -m 0755 "katlctl-$VERSION-$PLATFORM" "$HOME/.local/bin/katlctl"
export PATH="$HOME/.local/bin:$PATH"
katlctl version
```

Make sure that the CLI reports the release you selected. If
`$HOME/.local/bin` is absent from your usual path, add it to your shell
configuration for future sessions.

## Install with Homebrew

On a Linux amd64 workstation with Homebrew, install the stable channel from the
[Katl Homebrew tap](https://github.com/katl-dev/homebrew-katlctl):

```sh
brew install katl-dev/katlctl/stable
katlctl version
```

Use the versioned formula when you need the CLI from a specific release.
For example, to match a KatlOS 2026.11.0 installer:

```sh
brew install katl-dev/katlctl/stable@2026.11.0
katlctl version
```

The beta channel follows the newest eligible beta or stable release. Check
the tap for the versioned formulas it publishes. Uninstall the installed
formula before switching channels or versions because each formula provides
the same `katlctl` command.

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
`katlctl`. Shells started by `nix develop` load those completions. If you use
direnv in an already-running Fish shell, install the Fish script in its user
completion directory once; Fish does not refresh its completion search path
when direnv changes the environment:

```sh
mkdir -p ~/.config/fish/completions
katlctl completion fish > ~/.config/fish/completions/katlctl.fish
source ~/.config/fish/completions/katlctl.fish
```

New Fish sessions load the installed script automatically.

The `v2026.9.0-beta.16` tag does not include this flake package; for that
release, use Homebrew or the [release binary](#install-the-release-binary).
