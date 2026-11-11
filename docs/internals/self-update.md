# Fork self-update

The default update repository is **avaakash/px0**. `PX0_REPO=owner/repo` remains
an explicit override. The background updater starts after the HTTP listener is
ready and checks the latest stable GitHub release at most once per 24 hours.
`px0 --update` checks immediately; `-no-update` disables the background updater.
As before, quiet mode skips automatic updates. A failed check leaves px0 running.

The release tag is normalized separately from the asset version:

| Tag | Version | macOS Apple Silicon asset |
| --- | --- | --- |
| `macos-v0.1.22` | `0.1.22` | `px0-0.1.22-darwin-arm64` |
| `v0.1.22` | `0.1.22` | `px0-0.1.22-darwin-arm64` |

Stable tags must contain three numeric version components. Drafts, prereleases,
and unsupported tag formats are rejected. A release with an equal or older
version never replaces the current binary, including a local build ahead of
the published release. A release lacking the current OS/architecture asset
fails explicitly rather than guessing a download URL.

The existing update cache stays outside the workspace in
`$XDG_STATE_HOME/px0/update_check.json` or `~/.px0/update_check.json`. It now
records `repo` beside the check time and latest version. Legacy caches without
a repository and caches for another repository are ignored, so a recent
upstream check cannot suppress this fork's initial check.

Before replacement, the updater downloads the release's `checksums.txt`, verifies
the binary's SHA-256 digest, makes the temporary file executable, and runs its
`-version` command with a five-second timeout. The reported version must match
the release. Files are staged beside the installed executable and renamed into
place. All failure paths preserve the original binary. macOS keeps the existing
process running after replacement; the terminal notice requests a restart.

The release workflow accepts `macos-vX.Y.Z` for Apple Silicon/Intel macOS assets
and `vX.Y.Z` for the existing cross-platform targets. It checks out the exact
tag before selecting Go, bundling assets or compiling, requires the committed
VERSION to match, and publishes platform binaries plus `checksums.txt`. Until
new code and its release tag are pushed, changes to that workflow are local.

Regression tests cover the fork default/override, repository cache migration,
macOS tag normalization, stable-release validation, checksum mismatch, missing
assets, wrong embedded versions, and preservation of the installed binary on
failure. End-to-end smoke checks use disposable binary copies, never the
running test executable or the user's installation.
