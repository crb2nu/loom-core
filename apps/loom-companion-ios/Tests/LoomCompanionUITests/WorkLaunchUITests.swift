import XCTest

final class WorkLaunchUITests: XCTestCase {
    @MainActor
    func testWorkFirstLaunchIsStableThreeTimes() throws {
        for run in 1 ... 3 {
            let app = makeApp()
            app.launch()

            XCTAssertTrue(element("work.loading", in: app).waitForExistence(timeout: 2), "Run \(run) skipped the loading state")
            let summary = element("work.queue.summary", in: app)
            XCTAssertTrue(summary.waitForExistence(timeout: 12), "Run \(run) did not load Work")
            XCTAssertEqual(summary.label, "4 blockers need attention. Active and pending work follows.")

            app.terminate()
        }
    }

    @MainActor
    func testWorkRemainsUsableAtAccessibilityXXXL() throws {
        let app = makeApp(contentSize: "UICTContentSizeCategoryAccessibilityXXXL")
        app.launch()

        let summary = element("work.queue.summary", in: app)
        XCTAssertTrue(summary.waitForExistence(timeout: 12))
        XCTAssertEqual(summary.label, "4 blockers need attention. Active and pending work follows.")

        let sessionControls = element("work.session-controls", in: app)
        for _ in 0 ..< 10 where !sessionControls.isHittable {
            app.swipeUp()
        }
        XCTAssertTrue(sessionControls.exists)
        XCTAssertTrue(sessionControls.isHittable)

        let tabBar = app.tabBars.firstMatch
        XCTAssertTrue(tabBar.exists)
        XCTAssertLessThanOrEqual(sessionControls.frame.maxY, tabBar.frame.minY + 2)

        let screenshot = XCTAttachment(screenshot: app.screenshot())
        screenshot.name = "Work-AccessibilityXXXL"
        screenshot.lifetime = .keepAlways
        add(screenshot)
    }

    @MainActor
    private func makeApp(contentSize: String? = nil) -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = [
            "--uitesting-work-fixture",
            "--uitesting-response-delay-ms=5000",
        ]
        if let contentSize {
            app.launchArguments += ["-UIPreferredContentSizeCategoryName", contentSize]
        }
        return app
    }

    @MainActor
    private func element(_ identifier: String, in app: XCUIApplication) -> XCUIElement {
        app.descendants(matching: .any).matching(identifier: identifier).firstMatch
    }
}
