# Publishing this fork

This fork publishes to [avaakash/px0](https://github.com/avaakash/px0). Its
self-updater checks this repository's latest stable release. The upstream
px0.ai installer belongs to the upstream project; use this fork's release
assets or build its source.

## Prepare a release

Use the Go version declared in `go.mod` and Node.js 20+ (or Bun). Complete all
feature changes, bundle the frontend, run the tests, and build both macOS targets.
The local worktree should be clean before tagging. Document any known test
failures rather than describing a partial run as a clean suite.

```sh
node scripts/build-web.js
go test ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/px0-0.1.22-darwin-arm64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/px0-0.1.22-darwin-amd64 .
```

The version in the filenames must match `VERSION`. Commit feature/release fixes
first, then commit the version bump separately. Publish only after authorization.

## Automated macOS release

Push a `macos-vX.Y.Z` tag on the desired committed source, with `VERSION` set to
`X.Y.Z`. For example, after committing and validating version 0.1.22:

```sh
git push origin HEAD
git tag -a macos-v0.1.22 -m 'px0 0.1.22 for macOS'
git push origin macos-v0.1.22
```

The [Release workflow](https://github.com/avaakash/px0/actions/workflows/release.yml)
checks out that exact tag, verifies `VERSION`, bundles the tagged frontend, builds
Apple Silicon and Intel binaries with CGO disabled, writes SHA-256 checksums, and
publishes a stable GitHub release. A `vX.Y.Z` tag instead builds the existing
cross-platform target set. Tags must be unique; do not move a published tag.

For manual workflow dispatch, supply an existing stable tag and run the workflow
from a branch containing the updated workflow. It still builds the tagged source,
not the selected branch. Both source and VERSION must already be committed.

## Manual asset upload

If building and publishing locally instead of using the tag-triggered workflow,
keep all files from the same source/version and avoid concurrent uploads from a
workflow run. Required release files for macOS are:

- `px0-X.Y.Z-darwin-arm64`
- `px0-X.Y.Z-darwin-amd64`
- `checksums.txt`, with each binary's SHA-256 hash and exact filename

Use a draft release to upload and verify all assets before publication. Verify
GitHub's uploaded asset digests against local hashes. Do not mark experimental
builds as the latest stable release. Updating the GitHub source or pushing a
branch alone does not make an auto-update available; a published stable release
with matching binary assets is required.

## Verify the published release

Check the workflow result and [release assets](https://github.com/avaakash/px0/releases).
Test `px0 --update` on a disposable copy of an older binary first. The updater
must verify the checksum and embedded version, then replace only that copy.
Normal launches check in the background at most once per day and keep the
current session running; restart to load a downloaded update. `-no-update`
remains available, but is no longer needed to prevent updates from upstream.

Release binaries are standalone CLI executables, not `.app` bundles. The current
workflow does not provide Developer ID signing or notarization.
