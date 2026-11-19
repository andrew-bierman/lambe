# LambeDash — lambe operator dashboard

Native macOS SwiftUI app for the lambe daemon. This is the slice-one scaffold
(U6): one application target plus one Swift Testing target, zero third-party
dependencies, and a deliberate placeholder shell so the app shows a window on
day one. The daemon HTTP state client (U7) and the four operator views (U8)
build on this skeleton.

## Requirements

- Xcode 26 or newer (project built with Xcode 27.2, Swift 6 toolchain).
- Deployment target macOS 26.0; no external packages, no generation step
  (KTD6 rejected XcodeGen/Tuist; KTD8 rejected JavaScript orchestration).

## Open in Xcode

```
open apps/dashboard/LambeDash.xcodeproj
```

Press Cmd+R to run: the `LambeDash` scheme builds the app and shows the
placeholder window. The project uses file-synchronized groups, so files added
under `Sources/` or `Tests/` are picked up without editing the project file.

## CLI build and test

```
xcodebuild -project apps/dashboard/LambeDash.xcodeproj \
  -scheme LambeDash -destination 'platform=macOS' build

xcodebuild -project apps/dashboard/LambeDash.xcodeproj \
  -scheme LambeDash -destination 'platform=macOS' test
```

Both commands build into Xcode's default DerivedData outside this repository.
To keep output local, pass `-derivedDataPath <dir>` — `DerivedData/` and
`.build/` under `apps/dashboard/` are git-ignored, but default to keeping
build artifacts out of tracked source.

## Layout

- `Sources/LambeDashApp.swift` — `@main` with `WindowGroup` + `Settings`, and
  the placeholder shell / settings content.
- `Tests/AppSmokeTests.swift` — Swift Testing smoke suite proving the test
  harness runs; hosted in the app process.

## Toolchain decisions (KTD6)

- Committed `.xcodeproj` with synchronized groups — no generate-before-build
  step, no worktree staleness risk.
- `SWIFT_VERSION = 6.0`, `SWIFT_DEFAULT_ACTOR_ISOLATION = MainActor`,
  `SWIFT_APPROACHABLE_CONCURRENCY = YES` (project level, both targets).
- Ad-hoc signing ("Sign to Run Locally", `CODE_SIGN_IDENTITY = -`); no
  entitlements, no ATS keys, App Sandbox off (not required outside the
  Mac App Store, and off keeps the deferred UDS option open).
- Generated Info.plist (`GENERATE_INFOPLIST_FILE = YES`); no plist checked in.

## Test bar (R17)

v1 runs Swift Testing unit tests only — decode and model tests arrive with
the contract client in U7. Zero UI tests; GUI verification is Cmd+R plus the
`test/dashsmoke` assembled pass.
