# Releasing lambe

The git tag `vX.Y.Z` ([SemVer 2.0](https://semver.org/spec/v2.0.0.html)) is the
single source of truth for a version. Everything else is derived from it:

| Artifact | Derived from the tag by |
| --- | --- |
| `lambe` binaries, archives, `checksums.txt` | goreleaser (`.goreleaser.yaml` ldflags `main.version/commit/date`) |
| `lambe version` / `--json` | the same ldflags; `go install ...@vX.Y.Z` reads the tag from build info |
| `LambeDash.app` (`CFBundleShortVersionString`, `CFBundleVersion`) | `scripts/package-dashboard.sh` (tag at `HEAD`, or `X.Y.Z` argument) |
| GitHub release notes | the matching `CHANGELOG.md` section |

Development builds (a checkout, `go build`) report `vX.Y.Z-dev+<shortsha>`
(`.dirty` appended for uncommitted changes). `X.Y.Z` is the release the build
is heading toward, so it sorts below that release. `lambe version --json`
prints `version`, `commit`, `date`, and `api_version`.

## SemVer policy while on 0.x

Before 1.0.0 anything may change, but we keep the discipline:

- **Minor (0.Y.0)**: a new feature or any breaking change to the public API
  below, or a removal that finished its deprecation window.
- **Patch (0.9.Z)**: bug fixes, performance, docs, dashboard polish, and
  additive changes that do not alter existing behavior.
- **1.0.0** is cut when the public API below is declared stable; after that,
  breaking changes need a major bump.

Pre-releases use `vX.Y.Z-rc.N`; goreleaser publishes them as normal releases,
so mark them as pre-release on GitHub.

## Public API

Breaking one of these needs a minor bump (major after 1.0) and a CHANGELOG
entry under "Changed" or "Removed":

1. **CLI**: subcommands, flags, exit codes, and `lambe version --json` keys.
2. **`lambe.yaml` schema**: keys, defaults, and precedence. Adding an optional
   key is not breaking.
3. **State API contract**: routes and DTOs in `docs/api/state-api.md`, pinned
   by `testdata/contract/v1/`. It is versioned **separately** from the binary
   as `api_version` (the integer `httpstate.SchemaVersion`, also carried as
   `schema_version` in every response). Additive fields keep the number; a
   breaking wire change bumps it, ships under a new `/api/vN/` prefix, and
   `/api/v(N-1)/` keeps working for at least one minor release. Clients should
   check `api_version` from `lambe version --json` before attaching.

Not public: Go packages under `internal/`, log text, TUI layout, dashboard
visuals, and the `WORKFLOW.md` Symphony semantics beyond what the spec fixes.

## Deprecation policy

Deprecate in one minor release (CHANGELOG "Deprecated", a runtime warning
where practical), keep working for at least one further minor release, then
remove in a later minor release and list it under "Removed".

## Release steps

1. **Changelog.** On a PR to `master`, move the `[Unreleased]` entries into a
   new `## [X.Y.Z] - YYYY-MM-DD` section (Added / Changed / Deprecated /
   Removed / Fixed, plain language), leave a fresh `[Unreleased]`, and update
   the compare links at the bottom of `CHANGELOG.md`. Merge once CI is green.
2. **Validate and tag.** From a clean `master` equal to `origin/master`:
   ```sh
   scripts/release.sh X.Y.Z          # dry run: checks tree, branch, changelog, CI
   scripts/release.sh X.Y.Z --tag    # creates the annotated tag locally
   git push origin vX.Y.Z
   ```
   `scripts/release.sh` is a convenience; the checks that matter also run in
   CI. A `v*` tag whose CHANGELOG section is missing fails `ci` and `release`.
3. **CLI release.** Pushing the tag runs `.github/workflows/release.yml`:
   goreleaser builds linux/darwin x amd64/arm64 archives plus
   `checksums.txt`, and publishes the GitHub release using the CHANGELOG
   section as notes.
4. **Mac app.** On a Mac with Xcode and a Developer ID certificate, from the
   tagged commit:
   ```sh
   git checkout vX.Y.Z
   scripts/package-dashboard.sh              # reads the version from the tag
   ```
   The script stamps `CFBundleShortVersionString` and `CFBundleVersion` with
   `X.Y.Z`, verifies them, and writes `LambeDash.zip` and `LambeDash.zip.sha256`
   under `apps/dashboard/.build/package/`. It signs ad hoc by default. For a
   notarized build, re-sign with the Developer ID identity, run
   `xcrun notarytool submit LambeDash.zip --keychain-profile <profile> --wait`,
   `xcrun stapler staple` the app, and re-zip with `ditto -c -k --keepParent`.
   Then attach both files to the same release:
   ```sh
   gh release upload vX.Y.Z apps/dashboard/.build/package/LambeDash.zip \
     apps/dashboard/.build/package/LambeDash.zip.sha256
   ```
   This stays manual because notarization needs Apple credentials that are not
   in CI.
5. **Verify.**
   ```sh
   gh release download vX.Y.Z --dir /tmp/lambe-verify   # use a scratch dir
   cd /tmp/lambe-verify && shasum -a 256 -c checksums.txt --ignore-missing
   shasum -a 256 -c LambeDash.zip.sha256
   ```
6. **Install check.** Unpack the archive for your platform and confirm
   `./lambe version --json` reports `"version": "vX.Y.Z"` and the expected
   `api_version`; open `LambeDash.app` and confirm About shows `X.Y.Z`.

## Legacy

Before this policy the Mac app shipped under its own `dashboard-v1.0` tag and
release. That tag and release are kept for history and are no longer used: the
app now ships as an extra asset on the matching `vX.Y.Z` release.
