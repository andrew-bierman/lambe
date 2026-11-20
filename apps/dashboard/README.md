# LambeDash — lambe operator dashboard

Native macOS SwiftUI app for the lambe daemon. It attaches to a running
daemon over the daemon's authenticated local HTTP state API and renders
Queue, Attention, History, and run detail. The app is read-only: it polls
with GET requests and changes nothing — needs-human items are resolved in
Beads or the tracker, not here.

The project is a single committed `.xcodeproj` with no third-party
dependencies and no code-generation step.

## Requirements

- Xcode 26 or newer (project built with Xcode 27.2, Swift 6 toolchain).
- Deployment target macOS 26.0.
- Signing is ad-hoc ("Sign to Run Locally"): the app builds and runs without
  a developer certificate.

## Open in Xcode

```
open apps/dashboard/LambeDash.xcodeproj
```

Press Cmd+R to run: the `LambeDash` scheme builds the app and shows its
window. The project uses file-synchronized groups, so files added under
`Sources/` or `Tests/` are picked up without editing the project file.

## Demo mode

The window toolbar has a segmented Live/Demo picker. Choose **Demo** to
browse all four views with bundled sample data — no daemon, tracker, or
state API needed. The **Dashboard** menu offers the same switch ("Show Demo
Data" / "Connect to Daemon"), and the window subtitle reads "Demo data"
while sample data is up. Switch back to **Live** to attach to a daemon
(below).

## CLI build and test

mise wraps both commands with a repo-local derived-data path:

```
mise dashboard:build
mise dashboard:test
```

Directly:

```
xcodebuild -project apps/dashboard/LambeDash.xcodeproj \
  -scheme LambeDash -destination 'platform=macOS' build

xcodebuild -project apps/dashboard/LambeDash.xcodeproj \
  -scheme LambeDash -destination 'platform=macOS' test
```

`DerivedData/` and `.build/` under `apps/dashboard/` are git-ignored; keep
build artifacts out of tracked source.

## Attaching to a daemon

The dashboard discovers daemons by enumerating the per-instance descriptor
files the daemon publishes under the state directory's `live/` directory.
Descriptors exist only when the daemon opts in: `stateapi.enable` is off by
default, and `stateapi.addr` defaults to an ephemeral loopback address
(`127.0.0.1:0`). When enabled, every route requires the per-start bearer
token carried in those 0600 descriptors; the token never appears in logs.

Instance selection is explicit in Settings, so a dev or smoke instance is
easy to tell apart from a production daemon, and the window subtitle names
the daemon — identity and version — it is attached to. Attaching is
read-only: the app never starts, stops, or restarts a daemon. Production
lifecycle stays with the terminal running `lambe run` or the launchd/systemd
units in the root README's
[OS-level restart](../../README.md#os-level-restart-production) section.

The wire contract — routes, DTO fields, discovery, and auth posture — is
specified in [docs/api/state-api.md](../../docs/api/state-api.md); the
fixtures the Swift tests decode live in `testdata/contract/v1/`.

## Distribution

Download the [private macOS preview](https://github.com/andrew-bierman/lambe/releases/tag/dashboard-v1.0)
with repository access. The preview requires a running Lambe v0.9.0 or later
daemon with the local state API enabled for live data.

`mise dashboard:package` builds the ZIP locally. The script builds the app natively with Xcode
(`-configuration Release`, universal arm64 + x86_64), signs it with the
project's own ad-hoc identity, verifies the signature with `codesign --verify`,
and packs `LambeDash.app` into `apps/dashboard/.build/package/`:

- `LambeDash.zip` — the app, packed with `ditto` so the signature and
  metadata survive.
- `LambeDash.zip.sha256` — checksum of the ZIP; verify a copy with
  `shasum -a 256 -c LambeDash.zip.sha256` from that directory.

The packaging build has its own DerivedData path under
`apps/dashboard/.build/package/` and does not disturb `dashboard:build` or
`dashboard:test`. The same machine and Xcode rebuild the same app; ZIP
timestamps differ per run, so the checksum identifies each build.

### Requirements on the receiving Mac

- macOS 26.0 or newer — the deployment target is macOS 26.0; older systems
  refuse to launch the app.
- No Xcode, command-line tools, or third-party dependencies: the ZIP contains
  one self-contained `.app` bundle.

### Ad-hoc signing vs Developer ID

The app is ad-hoc signed ("Sign to Run Locally"), matching the project's
committed signing settings: it builds and runs with no developer certificate
and no Apple Developer account. An ad-hoc signature proves the bundle was not
modified after signing, but it carries no verified developer identity, so the
app is not notarized and Gatekeeper treats it as coming from an unknown
developer:

- On the machine that built it, the app just opens.
- Copied to another Mac, macOS quarantines the ZIP and asks for confirmation
  on first launch (System Settings → Privacy & Security → "Open Anyway").
  That prompt is expected for ad-hoc builds, not a malfunction. Do not
  disable Gatekeeper or strip quarantine attributes; the confirmation dialog
  is the supported flow.

For distribution beyond trusted colleagues — verified identity, no
confirmation prompt — sign with a Developer ID certificate and notarize the
ZIP (`notarytool`). That is out of scope here on purpose: it requires a paid
Apple Developer account and adds signing infrastructure this repo does not
want. Nothing in the project prevents adding it later.

### Connecting to a daemon

The packaged app attaches to daemons exactly like the dev build; nothing is
baked in at package time. The daemon must opt in to the state API first —
`stateapi.enable` is off by default, `stateapi.addr` defaults to loopback
(`127.0.0.1:0`), and every route requires the per-start bearer token published
in the daemon's 0600 descriptor file under the state directory's `live/`.
Pick the instance in Settings; the app is read-only and never starts, stops,
or restarts a daemon — production lifecycle stays with the terminal running
`lambe run` or the launchd/systemd units. Full details: [Attaching to a
daemon](#attaching-to-a-daemon) and
[docs/api/state-api.md](../../docs/api/state-api.md).

## Smoke test

The Go assembled smoke (`test/dashsmoke`, run with
`LAMBE_TEST_BD=1 mise exec go -- go test ./test/dashsmoke`; the test lives
in the sibling `../lambe-shared-client` worktree until it is integrated
here) builds the compiled `lambe` binary,
boots it against a disposable tracker with `stateapi.enable: true`, and
exercises the daemon's Go API client: descriptor discovery, an authenticated
state read, uniform 401s without the token, a 202 refresh, and descriptor
cleanup after a graceful stop. It covers the daemon and its client only.

The app itself is verified manually: run it with Cmd+R, select an enabled
instance in Settings, and compare the four views against `lambe status`.
Demo mode (above) covers the sample-data path.

## Layout

- `Sources/LambeDashApp.swift` — `@main` with `WindowGroup` + `Settings`.
- `Sources/Api/` — contract client: wire DTOs, the `DaemonClient` protocol,
  HTTP transport, descriptor decode.
- `Sources/Models/` — connection and feed models, sample data, and the
  instance store behind Settings.
- `Sources/Views/` — shell, section and status views, run detail, and the
  Settings instance picker.
- `Tests/ApiTests/` — Swift Testing decode, store, and transport tests over
  the committed contract fixtures.
- `Tests/ModelsTests/` — Swift Testing model state tests.
- `Tests/AppSmokeTests.swift` — Swift Testing smoke suite proving the test
  harness runs; hosted in the app process.

## Tests

The suite is Swift Testing unit tests: decode, store, and transport tests
over the committed fixtures, plus model state tests. The UI is verified by
running the app (Cmd+R); see Smoke test above for the daemon side.
