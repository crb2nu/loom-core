import Foundation
import Testing
@testable import LoomCompanionKit

/// Port of the web HUD's `shiftHelpers.test.ts` — the narrative strings are
/// a cross-surface contract (same runs in → same words out on web and iOS),
/// so these cases mirror the vitest suite line for line.
@Suite("MillsShiftReport")
struct MillsShiftReportTests {

    /// Fixed anchor so window edges are unambiguous in any TZ.
    static let now = Date(timeIntervalSince1970: 1_783_000_000)

    func run(
        id: String = "run-x",
        backlogID: String = "psl-x",
        template: String = "implement",
        state: String = "merged",
        attempts: Int = 1,
        startedAt: Date? = nil,
        endedAt: Date? = nil,
        costUSD: Double? = nil
    ) -> MillsPipelineRun {
        MillsPipelineRun(
            id: id, backlogID: backlogID, template: template, state: state,
            attempts: attempts, startedAt: startedAt, endedAt: endedAt, costUSD: costUSD)
    }

    /// Timestamp `hoursAgo` hours before `now`.
    func ago(_ hoursAgo: Double) -> Date {
        Self.now.addingTimeInterval(-hoursAgo * 3600)
    }

    func sr(
        kind: MillsShiftRun.Kind = .bolt,
        runID: String = "run-x",
        backlogID: String = "psl-x",
        template: String = "implement",
        attempts: Int = 1,
        endedAt: Date = now.addingTimeInterval(-3600),
        costUSD: Double? = nil
    ) -> MillsShiftRun {
        MillsShiftRun(
            kind: kind, runID: runID, backlogID: backlogID, template: template,
            attempts: attempts, endedAt: endedAt, costUSD: costUSD)
    }

    func pattern(slug: String = "go-rest-service", name: String = "go-rest-service", status: String = "approved") -> MillsPatternInfo {
        MillsPatternInfo(slug: slug, name: name, status: status)
    }

    // MARK: - window

    @Test("keeps terminal runs inside the window, oldest first")
    func windowKeepsTerminalRuns() {
        let runs = MillsShiftReport.window(
            [
                run(id: "a", state: "merged", endedAt: ago(2)),
                run(id: "b", state: "escalated", endedAt: ago(23)),
                run(id: "c", state: "done", endedAt: ago(10)),
            ],
            now: Self.now)
        #expect(runs.map(\.runID) == ["b", "c", "a"])
        #expect(runs.map(\.kind) == [.spark, .bolt, .bolt])
    }

    @Test("drops runs outside the window, non-woven states, and missing stamps")
    func windowDropsNonCloth() {
        let runs = MillsShiftReport.window(
            [
                run(id: "old", endedAt: ago(25)),
                run(id: "future", endedAt: ago(-1)),
                run(id: "paused", state: "paused", endedAt: ago(1)),
                run(id: "running", state: "running", endedAt: ago(1)),
                run(id: "nostamp"),
            ],
            now: Self.now)
        #expect(runs.isEmpty)
    }

    @Test("falls back to StartedAt when EndedAt is missing")
    func windowFallsBackToStartedAt() {
        let runs = MillsShiftReport.window([run(id: "a", startedAt: ago(3))], now: Self.now)
        #expect(runs.count == 1)
        #expect(runs[0].endedAt == ago(3))
    }

    @Test("honors a custom window length")
    func windowCustomLength() {
        let runs = MillsShiftReport.window(
            [run(id: "in", endedAt: ago(5)), run(id: "out", endedAt: ago(9))],
            now: Self.now, hours: 8)
        #expect(runs.map(\.runID) == ["in"])
    }

    // MARK: - stats

    @Test("splits bolts/sparks, sums cost, and ranks retries worst-first")
    func statsSplitsAndSums() {
        let stats = MillsShiftReport.stats(
            [
                sr(runID: "a", costUSD: 1.5),
                sr(kind: .spark, runID: "b", attempts: 3, costUSD: 2),
                sr(runID: "c", attempts: 2),
            ],
            patterns: [], backlog: [])
        #expect(stats.bolts.map(\.runID) == ["a", "c"])
        #expect(stats.sparks.map(\.runID) == ["b"])
        #expect(abs(stats.costUSD - 3.5) < 0.0001)
        #expect(stats.retried.map(\.runID) == ["b", "c"])
    }

    @Test("finds the busiest local hour")
    func statsBusiestHour() {
        // Build dates at known local hours, like the web test does.
        let cal = Calendar.current
        let h14 = cal.date(from: DateComponents(year: 2026, month: 7, day: 8, hour: 14, minute: 10))!
        let h9 = cal.date(from: DateComponents(year: 2026, month: 7, day: 8, hour: 9, minute: 5))!
        let stats = MillsShiftReport.stats(
            [
                sr(runID: "a", endedAt: h14),
                sr(runID: "b", endedAt: h14.addingTimeInterval(60)),
                sr(runID: "c", endedAt: h9),
            ],
            patterns: [], backlog: [], calendar: cal)
        #expect(stats.busiestHour == MillsShiftStats.BusiestHour(hour: 14, count: 2))
    }

    @Test("attributes pattern stamps via run → backlog → PlanID → slug")
    func statsPatternAttribution() {
        let backlog = [
            MillsBacklogItem(id: "psl-1", planID: "plan-stamp-go-rest-service-x1"),
            MillsBacklogItem(id: "psl-2", planID: "plan-stamp-go-rest-service-x2"),
            MillsBacklogItem(id: "psl-3", planID: "plan-organic"),
        ]
        let stats = MillsShiftReport.stats(
            [
                sr(runID: "a", backlogID: "psl-1"),
                sr(kind: .spark, runID: "b", backlogID: "psl-2"),
                sr(runID: "c", backlogID: "psl-3"),
            ],
            patterns: [pattern()], backlog: backlog)
        #expect(stats.stamps == [
            MillsShiftStamp(slug: "go-rest-service", name: "go-rest-service", bolts: 1, sparks: 1),
        ])
    }

    @Test("ignores unapproved patterns")
    func statsIgnoresUnapproved() {
        let backlog = [MillsBacklogItem(id: "psl-1", planID: "plan-stamp-go-rest-service-x1")]
        let stats = MillsShiftReport.stats(
            [sr(backlogID: "psl-1")],
            patterns: [pattern(status: "draft")], backlog: backlog)
        #expect(stats.stamps.isEmpty)
    }

    @Test("longest slug wins prefix attribution")
    func stampedSlugLongestWins() {
        let slug = MillsShiftReport.stampedPatternSlug(
            planID: "plan-stamp-go-rest-service-x1",
            slugs: ["go-rest", "go-rest-service"])
        #expect(slug == "go-rest-service")
        #expect(MillsShiftReport.stampedPatternSlug(planID: "plan-organic", slugs: ["go-rest"]) == nil)
        #expect(MillsShiftReport.stampedPatternSlug(planID: nil, slugs: ["go-rest"]) == nil)
    }

    // MARK: - narrative

    @Test("says a quiet shift plainly")
    func narrativeQuiet() {
        let stats = MillsShiftReport.stats([], patterns: [], backlog: [])
        #expect(MillsShiftReport.narrative(stats) == [
            "The loom sat quiet — no cloth came off the beam in the last 24 hours.",
        ])
    }

    @Test("leads with the bolt/spark headline, singular and plural correct")
    func narrativeHeadline() {
        let one = MillsShiftReport.stats([sr()], patterns: [], backlog: [])
        #expect(MillsShiftReport.narrative(one)[0]
            == "The floor wove 1 bolt and struck no sparks over the last 24 hours.")

        let many = MillsShiftReport.stats(
            [sr(runID: "a"), sr(runID: "b"), sr(kind: .spark, runID: "c")],
            patterns: [], backlog: [])
        #expect(MillsShiftReport.narrative(many)[0]
            == "The floor wove 2 bolts and struck 1 spark over the last 24 hours.")
    }

    @Test("narrates pattern stamps with once/twice phrasing and outcomes")
    func narrativeStamps() {
        let backlog = [
            MillsBacklogItem(id: "psl-1", planID: "plan-stamp-go-rest-service-a"),
            MillsBacklogItem(id: "psl-2", planID: "plan-stamp-go-rest-service-b"),
        ]
        let stats = MillsShiftReport.stats(
            [sr(runID: "a", backlogID: "psl-1"), sr(runID: "b", backlogID: "psl-2")],
            patterns: [pattern()], backlog: backlog)
        #expect(MillsShiftReport.narrative(stats)
            .contains("Pattern go-rest-service stamped twice — all merged on green."))
    }

    @Test("reports retries with the worst run named")
    func narrativeRetries() {
        let stats = MillsShiftReport.stats(
            [sr(runID: "a", backlogID: "psl-a", attempts: 4), sr(runID: "b", attempts: 2)],
            patterns: [], backlog: [])
        #expect(MillsShiftReport.narrative(stats)
            .contains("2 runs needed extra passes (worst: psl-a at 4 attempts)."))
    }

    @Test("narrates the busiest hour only when it has more than one departure")
    func narrativeBusiestHour() {
        let cal = Calendar.current
        let h14 = cal.date(from: DateComponents(year: 2026, month: 7, day: 8, hour: 14, minute: 10))!
        let spread = MillsShiftReport.narrative(MillsShiftReport.stats(
            [sr(runID: "a", endedAt: h14), sr(runID: "b", endedAt: h14.addingTimeInterval(-4 * 3600))],
            patterns: [], backlog: [], calendar: cal))
        #expect(!spread.contains { $0.hasPrefix("Busiest hour") })

        let peaked = MillsShiftReport.narrative(MillsShiftReport.stats(
            [sr(runID: "a", endedAt: h14), sr(runID: "b", endedAt: h14.addingTimeInterval(60))],
            patterns: [], backlog: [], calendar: cal))
        #expect(peaked.contains("Busiest hour 14:00–15:00 — 2 departures."))
    }

    @Test("includes cost only when spend is nonzero")
    func narrativeCost() {
        let free = MillsShiftReport.narrative(MillsShiftReport.stats([sr()], patterns: [], backlog: []))
        #expect(!free.contains { $0.contains("$") })
        let paid = MillsShiftReport.narrative(MillsShiftReport.stats([sr(costUSD: 1.25)], patterns: [], backlog: []))
        #expect(paid.contains("The shift burned $1.25 of pipeline fuel."))
    }

    @Test("is deterministic — same runs, same words")
    func narrativeDeterministic() {
        let runs = [sr(runID: "a"), sr(kind: .spark, runID: "b", attempts: 2)]
        let a = MillsShiftReport.narrative(MillsShiftReport.stats(runs, patterns: [], backlog: []))
        let b = MillsShiftReport.narrative(MillsShiftReport.stats(runs, patterns: [], backlog: []))
        #expect(a == b)
    }

    // MARK: - markdown

    /// 2026-07-08 19:00 UTC.
    static let generatedAt: Date = {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "UTC")!
        return cal.date(from: DateComponents(year: 2026, month: 7, day: 8, hour: 19))!
    }()

    func utcDate(hour: Int, minute: Int) -> Date {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "UTC")!
        return cal.date(from: DateComponents(year: 2026, month: 7, day: 8, hour: hour, minute: minute))!
    }

    @Test("renders headline, sparks with failing gates, and the departures table")
    func markdownFull() {
        let runs = [
            sr(runID: "a", backlogID: "psl-a", endedAt: utcDate(hour: 9, minute: 30)),
            sr(kind: .spark, runID: "b", backlogID: "psl-b", attempts: 2, endedAt: utcDate(hour: 11, minute: 0)),
        ]
        let stats = MillsShiftReport.stats(runs, patterns: [], backlog: [])
        let md = MillsShiftReport.markdown(
            stats: stats,
            narrative: MillsShiftReport.narrative(stats),
            generatedAt: Self.generatedAt,
            gateSummaries: [MillsSparkGateSummary(runID: "b", failedGates: ["judge_gate", "tests"])])
        #expect(md.contains("# Mills shift report — 2026-07-08 19:00 UTC"))
        #expect(md.contains("- `psl-b` (implement, 2 attempts) — failed judge_gate, tests"))
        #expect(md.contains("| 09:30 | 🟢 bolt | psl-a | implement | 1 |"))
        #expect(md.contains("| 11:00 | 🟡 spark | psl-b | implement | 2 |"))
    }

    @Test("omits the sparks and departures sections on a quiet shift")
    func markdownQuiet() {
        let stats = MillsShiftReport.stats([], patterns: [], backlog: [])
        let md = MillsShiftReport.markdown(
            stats: stats,
            narrative: MillsShiftReport.narrative(stats),
            generatedAt: Self.generatedAt)
        #expect(md.contains("The loom sat quiet"))
        #expect(!md.contains("## Sparks"))
        #expect(!md.contains("## Departures"))
    }

    // MARK: - wire decoding

    @Test("run detail decodes gates from the operator-shaped payload")
    func decodesRunDetail() throws {
        let json = """
        {
          "run": {"ID": "PIPE-A", "BacklogID": "psl-a", "Template": "implement", "State": "escalated", "Attempts": 2, "CostUSD": 1.75},
          "stages": [{"ID": 1, "PipelineRunID": "PIPE-A", "Stage": "tests", "Attempt": 1, "StartedAt": "2026-07-08T09:00:00Z", "CostUSD": 0.5}],
          "gates": [
            {"ID": 1, "PipelineRunID": "PIPE-A", "AfterStage": "tests", "GateName": "judge_gate", "Outcome": "fail", "EvaluatedAt": "2026-07-08T09:05:00Z"},
            {"ID": 2, "PipelineRunID": "PIPE-A", "AfterStage": "tests", "GateName": "lint", "Outcome": "pass", "EvaluatedAt": "2026-07-08T09:05:00Z"}
          ]
        }
        """.data(using: .utf8)!
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let detail = try decoder.decode(MillsPipelineRunDetail.self, from: json)
        #expect(detail.run?.id == "PIPE-A")
        #expect(detail.run?.costUSD == 1.75)
        let failed = (detail.gates ?? []).filter { $0.outcome == "fail" }.map(\.gateName)
        #expect(failed == ["judge_gate"])
    }

    @Test("backlog items and pattern catalog decode their wire shapes")
    func decodesBacklogAndPatterns() throws {
        let backlogJSON = """
        [{"ID": "psl-1", "Title": "Do a thing", "State": "merged", "Priority": "P2", "PlanID": "plan-stamp-go-rest-service-x"}]
        """.data(using: .utf8)!
        let items = try JSONDecoder().decode([MillsBacklogItem].self, from: backlogJSON)
        #expect(items == [MillsBacklogItem(id: "psl-1", planID: "plan-stamp-go-rest-service-x")])

        let catalogJSON = """
        {"patterns": [{"id": "pat-1", "slug": "go-rest-service", "name": "go-rest-service", "makes": "a Go REST service", "version": "1", "status": "approved"}]}
        """.data(using: .utf8)!
        let catalog = try JSONDecoder().decode(MillsPatternCatalog.self, from: catalogJSON)
        #expect(catalog.patterns == [pattern()])

        let emptyCatalog = try JSONDecoder().decode(MillsPatternCatalog.self, from: "{\"patterns\": null}".data(using: .utf8)!)
        #expect(emptyCatalog.patterns == nil)
    }

    @Test("terminal-runs endpoint carries state and limit query")
    func terminalRunsQuery() throws {
        let req = try Endpoint.millsPipelineRuns(state: "terminal", limit: 500)
            .urlRequest(baseURL: URL(string: "http://hud.local")!)
        let query = req.url?.query ?? ""
        #expect(query.contains("state=terminal"))
        #expect(query.contains("limit=500"))
        #expect(req.url?.path == "/api/mills/pipeline/runs")

        // The plain call keeps the original unfiltered semantics.
        let plain = try Endpoint.millsPipelineRuns()
            .urlRequest(baseURL: URL(string: "http://hud.local")!)
        #expect(plain.url?.query == nil)
    }

    @Test("MillsAPI shift reads degrade to empty on operator absence")
    func shiftReadsDegrade() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/pipeline/runs"] =
            .apiError(code: .upstreamError, message: "bad gateway", requestId: "")
        mock.endpointFailures["/api/mills/backlog"] =
            .apiError(code: .notConfigured, message: "operator unset", requestId: "")
        mock.endpointFailures["/api/patterns"] =
            .apiError(code: .notFound, message: "not found", requestId: "")
        mock.endpointFailures["/api/mills/pipeline/runs/PIPE-X"] =
            .apiError(code: .upstreamError, message: "bad gateway", requestId: "")
        let api = MillsAPI(client: mock)
        #expect(try await api.terminalRuns(limit: 500).isEmpty)
        #expect(try await api.backlog().isEmpty)
        #expect(try await api.approvedPatterns().isEmpty)
        #expect(try await api.runDetail(id: "PIPE-X") == nil)
    }

    @Test("Pattern catalog 403 degrades to an empty-but-unavailable result")
    func patternsForbiddenIsTolerated() async throws {
        // Daemons whose mobile companion allowlist predates /api/patterns
        // answer 403. That used to throw straight through `try?` in
        // ShiftReportSheet, silently dropping pattern attribution.
        let mock = MockAPIClient()
        mock.endpointFailures["/api/patterns"] =
            .apiError(code: .forbidden, message: "mobile_operator token is restricted", requestId: "")
        let api = MillsAPI(client: mock)

        let result = try await api.approvedPatternsResult()
        #expect(result.patterns.isEmpty)
        #expect(result.unavailable)
        // The legacy accessor still degrades quietly for existing callers.
        #expect(try await api.approvedPatterns().isEmpty)
    }

    @Test("A reachable catalog reports available even when empty")
    func patternsAvailableWhenEmpty() async throws {
        let mock = MockAPIClient()
        mock.patternsCatalogResponse = try JSONDecoder().decode(
            MillsPatternCatalog.self, from: Data("{\"patterns\": null}".utf8))
        let api = MillsAPI(client: mock)

        let result = try await api.approvedPatternsResult()
        #expect(result.patterns.isEmpty)
        #expect(result.unavailable == false)
    }
}
