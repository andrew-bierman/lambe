# Release template: ship a Go CLI like Lambe

Copyable release setup taken from Lambe's working config (v0.10.1). It gives a
small Go CLI: tagged releases, linux/darwin archives, deb/rpm/apk packages, a
Homebrew formula, a keyless cosign signature over `checksums.txt`, SBOMs,
build-provenance attestations, SHA-pinned actions, and a changelog gate on tags.

Placeholders look like `__TOOL__`. Nothing here is wired into Lambe's own
release; the live files are `.goreleaser.yaml`, `.github/workflows/release.yml`,
and `scripts/`. When Lambe's files change, refresh these examples.

## Prerequisites

- A Go module with a `main` package and a `version`, `commit`, `date` string
  variable set via ldflags (`main.version` etc.).
- `gh` logged in as the repo owner; `ssh-keygen`.
- The tap repo `__OWNER__/homebrew-tap` exists (shared by all tools).
- A CI workflow named `ci` that runs on pushes and PRs, plus the
  changelog step from Lambe's `ci.yml`: on `v*` tags, run
  `scripts/changelog-section.sh "$GITHUB_REF_NAME" > /dev/null`.
- Optional locally: `goreleaser` (for `goreleaser check`), `cosign`, `brew`.

## Files to copy

| Template file | Install as |
| --- | --- |
| `goreleaser.example.yaml` | `.goreleaser.yaml` |
| `release.example.yml` | `.github/workflows/release.yml` |
| `release.example.sh` | `scripts/release.sh` (executable) |
| `changelog-section.example.sh` | `scripts/changelog-section.sh` (executable) |
| `install.example.sh` | `scripts/install.sh` (executable) |
| `CHANGELOG.example.md` | `CHANGELOG.md` (skip if you have one; keep its format) |

Also copy `LICENSE` handling as your repo needs; the nfpm block ships `LICENSE`.

## One-time setup per repo

1. Generate a deploy key pair in a scratch directory (not in the repo, not in `~`):
   ```sh
   d=$(mktemp -d) && ssh-keygen -t ed25519 -N '' -C "__TOOL__ release" -f "$d/tap_key"
   ```
2. Add the public half as a deploy key **with write access** on the tap:
   ```sh
   gh repo deploy-key add "$d/tap_key.pub" --repo __OWNER__/homebrew-tap \
     --title "__TOOL__ release" --allow-write
   ```
3. Store the private half as an Actions secret on the tool repo. Redirect the
   file into `gh`; never `cat` or `echo` it:
   ```sh
   gh secret set HOMEBREW_TAP_DEPLOY_KEY --repo __OWNER__/__TOOL__ < "$d/tap_key"
   ```
4. Delete the key material: `rm -rf "$d"`.
5. Enable Actions on the repo (Settings > Actions) and leave default workflow
   permissions read-only; `release.yml` declares what it needs.
6. Branch protection on the default branch: require PR review, and require the
   `ci` check if you want it. Do not require status checks you cannot run on
   every PR. Tag creation is not gated by the ruleset, so `scripts/release.sh`
   plus the `ci` changelog check are the gate.
7. Rotation: repeat steps 1-4 with a new pair and delete the old deploy key.

One deploy key per tool repo keeps the blast radius to the tap alone.

## Find/replace checklist

Fill these, then run `check.sh` on the target repo.

| Placeholder | Meaning |
| --- | --- |
| `__TOOL__` | binary, project, and formula name (lowercase) |
| `__OWNER__` | GitHub owner (`andrew-bierman`) |
| `__MAIN_PKG__` | main package path, e.g. `./cmd/__TOOL__` |
| `__DESCRIPTION__` | one-line description (appears twice) |
| `__LICENSE__` | SPDX id, e.g. `GPL-3.0-or-later` |
| `__MAINTAINER_NAME__`, `__MAINTAINER_EMAIL__` | package maintainer and tap commit author (use a noreply address) |
| `__GO_VERSION__` | Go minor, e.g. `1.27` (`release.yml`) |
| `__DEFAULT_BRANCH__` | `master` or `main` (`release.sh`, `install.sh`) |
| `__CI_WORKFLOW__` | name of the CI workflow `release.sh` checks (`ci`) |
| `__ENVPREFIX__` | env var prefix in `install.sh`, e.g. `BEADLE` |
| `__BREW_DEPENDENCY__` | runtime formula, or delete the `dependencies` block |
| `__CAVEATS__` | brew caveats text, or delete the block |
| `__VERSION_CMD__` | command the formula test runs, e.g. `version` or `--version` |
| `__NEXT_STEPS__` | last line `install.sh` prints |
| `__FIRST_VERSION__`, `__YYYY-MM-DD__` | first changelog entry |

Also check the pinned goreleaser version and action SHAs in `release.yml`; bump
them together and keep the version comment next to each SHA.

## Drop for non-daemon tools

Lambe is a daemon, so some of its config does not apply to a plain CLI:

- The nfpm `contents` entry for `examples/services/__TOOL__.service` and that
  file itself. Keep the `LICENSE` entry.
- `service install` text in `caveats` and `__NEXT_STEPS__`.
- The state API `api_version` and `version --json` notes in Lambe's
  `docs/RELEASING.md` ("Public API" item 3). Version your own public surface
  instead, or skip the section.
- The `beads` dependency, unless the tool shells out to `bd`.

## Private repos

Hosted Actions minutes are billed for private repos, and the account has hit
its spending limit before. Prefer public tools. The release workflow runs only
on `v*` tags, so a private repo spends minutes only when you release; keep it
that way, and use `[skip ci]` on WIP pushes. For a private repo,
`install.sh` supports `gh`-based downloads, and the tap formula must be
reachable by users who install it (a public tap cannot fetch private release
assets without a token).

## Release flow

1. PR: move `[Unreleased]` into `## [X.Y.Z] - date`, add the compare links.
2. `scripts/release.sh X.Y.Z` (dry run), then `--tag`, then `git push origin vX.Y.Z`.
3. The workflow builds, signs, attests, pushes the formula, and publishes
   notes from the changelog section.

## Verify a release

```sh
d=$(mktemp -d) && cd "$d"
gh release download vX.Y.Z --repo __OWNER__/__TOOL__
shasum -a 256 -c checksums.txt --ignore-missing
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/__OWNER__/__TOOL__/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify __TOOL___X.Y.Z_linux_amd64.tar.gz --repo __OWNER__/__TOOL__
```

Homebrew: `brew install __OWNER__/tap/__TOOL__ && __TOOL__ __VERSION_CMD__`.

Packages (Linux): `dpkg-deb -I __TOOL___X.Y.Z_linux_amd64.deb` and
`dpkg-deb -c ...` (or `rpm -qpi` / `rpm -qpl` for the rpm) show metadata and
contents; install on a throwaway machine or container to confirm the binary
lands in `/usr/bin`.

## Check the template

```sh
docs/release-template/check.sh /path/to/target-repo
```

It lists leftover `__PLACEHOLDER__` strings. It is a convenience, not a gate.
