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

The dashboard ships as a local Xcode build; there is no packaged download.

## Smoke test

The Go assembled smoke (`cmd/lambe/stateapi_test.go`, run with
`LAMBE_TEST_BD=1 go test ./cmd/lambe`) builds the compiled `lambe` binary,
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
