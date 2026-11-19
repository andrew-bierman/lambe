//
//  AppSmokeTests.swift
//  LambeDashTests
//
//  U6 smoke proof: the Swift Testing harness runs end to end from a fresh
//  checkout, hosted in the app process. The v1 test bar is zero UI tests
//  (R17); decode and model assertions arrive with the contract client (U7).
//

import Testing

@testable import LambeDash

struct AppSmokeTests {
    @Test("Swift Testing harness runs and the app module links")
    func harnessRuns() {
        #expect(String(describing: ShellPlaceholderView.self) == "ShellPlaceholderView")
    }
}
