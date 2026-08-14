import XCTest
@testable import LoomCompanionKit

final class LoomFormatTests: XCTestCase {

    // MARK: - Relative time (long form)

    func testRelativeSecondsTiers() {
        XCTAssertEqual(LoomFormat.relative(seconds: 0), "just now")
        XCTAssertEqual(LoomFormat.relative(seconds: 4), "just now")
        XCTAssertEqual(LoomFormat.relative(seconds: 5), "5s ago")
        XCTAssertEqual(LoomFormat.relative(seconds: 59), "59s ago")
        XCTAssertEqual(LoomFormat.relative(seconds: 90), "1m ago")
        XCTAssertEqual(LoomFormat.relative(seconds: 3_600), "1h ago")
        XCTAssertEqual(LoomFormat.relative(seconds: 90_000), "1d ago")
    }

    func testRelativeNegativeClampsToJustNow() {
        XCTAssertEqual(LoomFormat.relative(seconds: -42), "just now")
    }

    func testRelativeFromISOMatchesSeconds() {
        let now = Date(timeIntervalSince1970: 1_000_000)
        let fiveMinAgo = now.addingTimeInterval(-300)
        let iso = ISO8601DateFormatter().string(from: fiveMinAgo)
        XCTAssertEqual(LoomFormat.relative(fromISO: iso, now: now), "5m ago")
    }

    func testRelativeFromISOUnparseable() {
        XCTAssertEqual(LoomFormat.relative(fromISO: "not-a-date"), "—")
    }

    // MARK: - Relative time (compact)

    func testRelativeCompactTiersHaveNoSuffix() {
        XCTAssertEqual(LoomFormat.relativeCompact(seconds: 0), "now")
        XCTAssertEqual(LoomFormat.relativeCompact(seconds: 42), "42s")
        XCTAssertEqual(LoomFormat.relativeCompact(seconds: 120), "2m")
        XCTAssertEqual(LoomFormat.relativeCompact(seconds: 7_200), "2h")
        XCTAssertEqual(LoomFormat.relativeCompact(seconds: 172_800), "2d")
    }

    /// The magnitude of compact and long form must never disagree for the same input.
    func testCompactAndLongShareMagnitude() {
        for s in [42, 90, 3_600, 90_000] {
            let long = LoomFormat.relative(seconds: s)        // e.g. "1m ago"
            let compact = LoomFormat.relativeCompact(seconds: s) // e.g. "1m"
            XCTAssertEqual(long, "\(compact) ago",
                           "compact/long diverged for \(s)s")
        }
    }

    // MARK: - Compact numbers

    func testCompactNumberTiers() {
        XCTAssertEqual(LoomFormat.compact(0), "0")
        XCTAssertEqual(LoomFormat.compact(950), "950")
        XCTAssertEqual(LoomFormat.compact(12_400), "12.4k")
        XCTAssertEqual(LoomFormat.compact(1_200_000), "1.2M")
    }

    /// Regression guard for the divergence where some views dropped the millions tier.
    func testCompactKeepsMillionsTier() {
        XCTAssertEqual(LoomFormat.compact(2_500_000), "2.5M")
        XCTAssertNotEqual(LoomFormat.compact(2_500_000), "2500.0k")
    }

    func testTokensAddsUnit() {
        XCTAssertEqual(LoomFormat.tokens(12_400), "12.4k tokens")
        XCTAssertEqual(LoomFormat.tokens(900, unit: "tok"), "900 tok")
    }

    // MARK: - Durations

    func testDurationSeconds() {
        XCTAssertEqual(LoomFormat.duration(seconds: 0), "0s")
        XCTAssertEqual(LoomFormat.duration(seconds: 45), "45s")
        XCTAssertEqual(LoomFormat.duration(seconds: 600), "10m")
        XCTAssertEqual(LoomFormat.duration(seconds: 7_200), "2h")
        XCTAssertEqual(LoomFormat.duration(seconds: 172_800), "2d")
    }

    func testDurationMillis() {
        XCTAssertEqual(LoomFormat.duration(millis: 850), "850ms")
        XCTAssertEqual(LoomFormat.duration(millis: 1_500), "1.5s")
        XCTAssertEqual(LoomFormat.duration(millis: 90_000), "1m")
    }

    // MARK: - Paths

    func testLastPathComponent() {
        XCTAssertEqual(LoomFormat.lastPathComponent("services/loom-core"), "loom-core")
        XCTAssertEqual(LoomFormat.lastPathComponent("loom-core"), "loom-core")
        XCTAssertEqual(LoomFormat.lastPathComponent("a/b/c.swift"), "c.swift")
        XCTAssertEqual(LoomFormat.lastPathComponent(""), "")
    }

    // MARK: - Agent branding

    func testAgentBrandIsCaseInsensitive() {
        XCTAssertEqual(LoomFormat.agentBrand("Claude-Code"), LoomFormat.agentBrand("claude-code"))
    }

    func testAgentBrandKnownVendors() {
        let claude = LoomFormat.agentBrand("claude-code")
        XCTAssertEqual(claude.icon, "terminal.fill")
        XCTAssertEqual(claude.red, 1.0, accuracy: 0.001)
        XCTAssertEqual(claude.green, 0.420, accuracy: 0.001)
        XCTAssertEqual(claude.blue, 0.616, accuracy: 0.001)
    }

    func testAgentBrandFallback() {
        XCTAssertEqual(LoomFormat.agentBrand("mystery-agent"), LoomFormat.defaultAgentBrand)
    }
}
