//
//  AppSmokeTests.swift
//  LambeDashTests
//
//  U6 smoke proof: the Swift Testing harness runs end to end from a fresh
//  checkout, hosted in the app process. The v1 test bar is zero UI tests
//  (R17); decode and model assertions live in WireDTOTests and
//  ModelTests (U7/U8).
//

import Testing

@testable import LambeDash

struct AppSmokeTests {
    @Test("Swift Testing harness runs and the app module links")
    func harnessRuns() {
        #expect(String(describing: ShellView.self) == "ShellView")
    }
}
