//
//  LambeDashApp.swift
//  lambe operator dashboard
//
//  Slice-one scaffold (U6). Renders an intentional placeholder shell so the
//  app shows a window on day one; the Queue, Attention, History, and run
//  detail views arrive in U8 against the daemon HTTP state client (U7).
//  Presentation only: no tracker access, no process APIs, no dependencies.
//

import SwiftUI

/// lambe operator dashboard entry point.
///
/// Minimal scene layout on purpose: one main window plus a Settings scene
/// that later hosts the instance picker (U8). The project compiles with
/// `SWIFT_DEFAULT_ACTOR_ISOLATION = MainActor`, so everything here is
/// MainActor-isolated by default — no explicit annotations needed.
@main
struct LambeDashApp: App {
    var body: some Scene {
        WindowGroup {
            ShellPlaceholderView()
        }

        Settings {
            SettingsPlaceholderView()
        }
    }
}

/// Day-one placeholder window content.
///
/// Deliberate empty state, not a stub to mistake for a bug: names the app,
/// states the read-only posture, and says what replaces it.
struct ShellPlaceholderView: View {
    var body: some View {
        VStack(spacing: 16) {
            Image(systemName: "terminal")
                .font(.system(size: 44))
                .foregroundStyle(.tertiary)
                .accessibilityHidden(true)

            Text("LambeDash")
                .font(.title2.weight(.semibold))

            Text("lambe operator dashboard")
                .font(.subheadline)
                .foregroundStyle(.secondary)

            Text("Placeholder shell — Queue, Attention, History, and run details arrive with the daemon state client.")
                .font(.footnote)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .padding(.horizontal, 32)
        }
        .frame(minWidth: 480, minHeight: 320)
    }
}

/// Placeholder Settings scene content.
///
/// Replaced by the instance picker in U8; exists so the Settings scene is
/// real from the first launch.
struct SettingsPlaceholderView: View {
    var body: some View {
        ContentUnavailableView(
            "No settings yet",
            systemImage: "gear",
            description: Text("Instance selection arrives with the daemon state client.")
        )
    }
}
