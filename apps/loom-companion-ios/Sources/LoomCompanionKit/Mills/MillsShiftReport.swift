// Shift report semantics — the last 24 hours of terminal Mills runs told
// as a short, deterministic narrative ("the floor wove 6 bolts, struck 2
// sparks, pattern go-rest-service stamped twice") plus a share-sheet-ready
// markdown export. No summarizer: same runs in → same words out.
//
// This is a 1:1 port of the web HUD's
// `internal/hud/frontend/src/lib/utils/shiftHelpers.ts` (+ the
// `stampedPatternSlug` chain from `patternBooks.ts`) so the iOS Mills tab
// tells the SAME shift story as the Factory panel. If a rule changes there,
// change it here too — the narrative strings are part of the contract and
// pinned by MillsShiftReportTests.

import Foundation

/// One terminal run inside the shift window.
public struct MillsShiftRun: Sendable, Hashable {
    public enum Kind: String, Sendable {
        case bolt
        case spark
    }

    public let kind: Kind
    public let runID: String
    public let backlogID: String
    public let template: String
    public let attempts: Int
    /// The moment the run left the beam (EndedAt, fallback StartedAt).
    public let endedAt: Date
    public let costUSD: Double?

    public init(
        kind: Kind,
        runID: String,
        backlogID: String,
        template: String,
        attempts: Int,
        endedAt: Date,
        costUSD: Double? = nil
    ) {
        self.kind = kind
        self.runID = runID
        self.backlogID = backlogID
        self.template = template
        self.attempts = attempts
        self.endedAt = endedAt
        self.costUSD = costUSD
    }
}

/// One pattern-book line: "go-rest-service stamped twice, both merged".
public struct MillsShiftStamp: Sendable, Equatable {
    public let slug: String
    public let name: String
    public var bolts: Int
    public var sparks: Int

    public init(slug: String, name: String, bolts: Int, sparks: Int) {
        self.slug = slug
        self.name = name
        self.bolts = bolts
        self.sparks = sparks
    }
}

/// Everything the narrative and the markdown are generated from.
public struct MillsShiftStats: Sendable {
    public struct BusiestHour: Sendable, Equatable {
        /// Local hour-of-day (0–23) with the most departures.
        public let hour: Int
        public let count: Int

        public init(hour: Int, count: Int) {
            self.hour = hour
            self.count = count
        }
    }

    public let hours: Int
    public let bolts: [MillsShiftRun]
    public let sparks: [MillsShiftRun]
    public let costUSD: Double
    /// Runs that needed more than one attempt, worst first.
    public let retried: [MillsShiftRun]
    public let busiestHour: BusiestHour?
    /// Pattern-book attribution for the shift's runs, most-woven first.
    public let stamps: [MillsShiftStamp]
}

/// Failing-gate summary for one spark, resolved by the report view from
/// the per-run detail endpoint.
public struct MillsSparkGateSummary: Sendable, Equatable {
    public let runID: String
    /// Names of gates that failed, in evaluation order; [] = none found.
    public let failedGates: [String]

    public init(runID: String, failedGates: [String]) {
        self.runID = runID
        self.failedGates = failedGates
    }
}

public enum MillsShiftReport {
    private static let stampPrefix = "plan-stamp-"

    /// Which catalog pattern stamped this plan, by longest-slug prefix
    /// match (`go-rest` must not swallow `go-rest-service`). Returns nil
    /// for non-stamp plans and for slugs missing from the catalog — an
    /// unknown book attributes to nothing rather than the wrong shelf spot.
    public static func stampedPatternSlug(planID: String?, slugs: [String]) -> String? {
        guard let planID, planID.hasPrefix(stampPrefix) else { return nil }
        let rest = String(planID.dropFirst(stampPrefix.count))
        var best: String?
        for slug in slugs where !slug.isEmpty {
            if rest == slug || rest.hasPrefix(slug + "-") {
                if best == nil || slug.count > (best?.count ?? 0) { best = slug }
            }
        }
        return best
    }

    /// Filter terminal runs to the shift window — the `hours` hours ending
    /// at `now` — using the SAME weave rule as the web Factory panel:
    /// done/merged is a bolt, escalated is a spark, everything else
    /// (paused = held thread) is not cloth. A run lands on the moment it
    /// ENDED (fallback: started); runs without a usable timestamp are
    /// dropped. Oldest first.
    public static func window(
        _ runs: [MillsPipelineRun],
        now: Date,
        hours: Int = 24
    ) -> [MillsShiftRun] {
        let end = now
        let start = now.addingTimeInterval(-Double(hours) * 3600)
        var out: [MillsShiftRun] = []
        for run in runs {
            guard !run.id.isEmpty else { continue }
            let state = run.state.lowercased()
            let kind: MillsShiftRun.Kind?
            switch state {
            case "done", "merged": kind = .bolt
            case "escalated": kind = .spark
            default: kind = nil
            }
            guard let kind else { continue }
            guard let t = run.endedAt ?? run.startedAt else { continue }
            guard t >= start, t <= end else { continue }
            out.append(MillsShiftRun(
                kind: kind,
                runID: run.id,
                backlogID: run.backlogID,
                template: run.template,
                attempts: run.attempts,
                endedAt: t,
                costUSD: run.costUSD
            ))
        }
        // Stable oldest-first, matching the web's (spec-stable) Array.sort.
        return out.enumerated()
            .sorted { a, b in
                a.element.endedAt == b.element.endedAt
                    ? a.offset < b.offset
                    : a.element.endedAt < b.element.endedAt
            }
            .map(\.element)
    }

    /// Aggregate a shift's runs. Pattern attribution reuses the web
    /// shelf's chain (run → backlog → PlanID → catalog slug) — derived,
    /// never guessed; runs that don't trace to an approved pattern simply
    /// don't stamp a book. The busiest hour is local time (`calendar`).
    public static func stats(
        _ runs: [MillsShiftRun],
        patterns: [MillsPatternInfo],
        backlog: [MillsBacklogItem],
        hours: Int = 24,
        calendar: Calendar = .current
    ) -> MillsShiftStats {
        let bolts = runs.filter { $0.kind == .bolt }
        let sparks = runs.filter { $0.kind == .spark }
        let costUSD = runs.reduce(0.0) { $0 + ($1.costUSD ?? 0) }

        let retried = runs.filter { $0.attempts > 1 }
            .enumerated()
            .sorted { a, b in
                a.element.attempts == b.element.attempts
                    ? a.offset < b.offset
                    : a.element.attempts > b.element.attempts
            }
            .map(\.element)

        // First-seen hour wins ties, mirroring the web's Map insertion order
        // + strictly-greater comparison.
        var hourOrder: [Int] = []
        var byHour: [Int: Int] = [:]
        for run in runs {
            let h = calendar.component(.hour, from: run.endedAt)
            if byHour[h] == nil { hourOrder.append(h) }
            byHour[h, default: 0] += 1
        }
        var busiestHour: MillsShiftStats.BusiestHour?
        for hour in hourOrder {
            let count = byHour[hour] ?? 0
            if busiestHour == nil || count > (busiestHour?.count ?? 0) {
                busiestHour = MillsShiftStats.BusiestHour(hour: hour, count: count)
            }
        }

        let approved = patterns.filter { $0.status == "approved" && !$0.slug.isEmpty }
        let slugs = approved.map(\.slug)
        let nameBySlug = Dictionary(approved.map { ($0.slug, $0.name) }, uniquingKeysWith: { a, _ in a })
        var planByBacklog: [String: String] = [:]
        for item in backlog {
            if !item.id.isEmpty, let planID = item.planID, !planID.isEmpty {
                planByBacklog[item.id] = planID
            }
        }
        var stampOrder: [String] = []
        var stampMap: [String: MillsShiftStamp] = [:]
        for run in runs {
            guard let slug = stampedPatternSlug(planID: planByBacklog[run.backlogID], slugs: slugs) else { continue }
            var stamp = stampMap[slug] ?? {
                stampOrder.append(slug)
                return MillsShiftStamp(slug: slug, name: nameBySlug[slug] ?? slug, bolts: 0, sparks: 0)
            }()
            if run.kind == .bolt { stamp.bolts += 1 } else { stamp.sparks += 1 }
            stampMap[slug] = stamp
        }
        let stamps = stampOrder.compactMap { stampMap[$0] }
            .sorted { a, b in
                let at = a.bolts + a.sparks
                let bt = b.bolts + b.sparks
                return at == bt ? a.name < b.name : at > bt
            }

        return MillsShiftStats(
            hours: hours,
            bolts: bolts,
            sparks: sparks,
            costUSD: costUSD,
            retried: retried,
            busiestHour: busiestHour,
            stamps: stamps
        )
    }

    private static func plural(_ n: Int, _ one: String, _ many: String? = nil) -> String {
        "\(n) \(n == 1 ? one : (many ?? one + "s"))"
    }

    private static func hourRange(_ h: Int) -> String {
        func f(_ x: Int) -> String { String(format: "%02d:00", x % 24) }
        return "\(f(h))–\(f(h + 1))"
    }

    /// The shift's story as deterministic prose lines, headline first.
    /// Truth over theater: a quiet shift says so plainly, and nothing here
    /// implies more activity than the runs prove.
    public static func narrative(_ stats: MillsShiftStats) -> [String] {
        var lines: [String] = []
        let total = stats.bolts.count + stats.sparks.count
        if total == 0 {
            lines.append("The loom sat quiet — no cloth came off the beam in the last \(stats.hours) hours.")
            return lines
        }
        let boltPart = plural(stats.bolts.count, "bolt")
        let sparkPart = stats.sparks.isEmpty ? "no sparks" : plural(stats.sparks.count, "spark")
        lines.append("The floor wove \(boltPart) and struck \(sparkPart) over the last \(stats.hours) hours.")

        for s in stats.stamps {
            let times = s.bolts + s.sparks
            let timesWord = times == 1 ? "once" : times == 2 ? "twice" : "\(times) times"
            let outcome: String
            if s.sparks == 0 {
                outcome = times == 1 ? "merged on green" : "all merged on green"
            } else if s.bolts == 0 {
                outcome = times == 1 ? "escalated" : "all escalated"
            } else {
                outcome = "\(plural(s.bolts, "merge")), \(plural(s.sparks, "escalation"))"
            }
            lines.append("Pattern \(s.name) stamped \(timesWord) — \(outcome).")
        }

        if let worst = stats.retried.first {
            let worstID = worst.backlogID.isEmpty ? worst.runID : worst.backlogID
            lines.append("\(plural(stats.retried.count, "run")) needed extra passes (worst: \(worstID) at \(worst.attempts) attempts).")
        }

        // A "busiest hour" of one departure is noise, not a peak.
        if let busiest = stats.busiestHour, busiest.count > 1 {
            lines.append("Busiest hour \(hourRange(busiest.hour)) — \(plural(busiest.count, "departure")).")
        }

        if stats.costUSD > 0 {
            lines.append("The shift burned $\(String(format: "%.2f", stats.costUSD)) of pipeline fuel.")
        }
        return lines
    }

    /// Standup-pasteable markdown. Narrative first, then per-spark detail
    /// (with failing gates when the caller resolved them), then the raw
    /// departures table. Deterministic given the same inputs; all
    /// timestamps UTC so the export reads the same on every device.
    public static func markdown(
        stats: MillsShiftStats,
        narrative: [String],
        generatedAt: Date,
        gateSummaries: [MillsSparkGateSummary] = []
    ) -> String {
        let utc = TimeZone(identifier: "UTC") ?? .current
        let headerFormatter = DateFormatter()
        headerFormatter.locale = Locale(identifier: "en_US_POSIX")
        headerFormatter.timeZone = utc
        headerFormatter.dateFormat = "yyyy-MM-dd HH:mm"
        let hhmmFormatter = DateFormatter()
        hhmmFormatter.locale = Locale(identifier: "en_US_POSIX")
        hhmmFormatter.timeZone = utc
        hhmmFormatter.dateFormat = "HH:mm"

        let gatesByRun = Dictionary(gateSummaries.map { ($0.runID, $0.failedGates) }, uniquingKeysWith: { a, _ in a })
        var out: [String] = []
        out.append("# Mills shift report — \(headerFormatter.string(from: generatedAt)) UTC")
        out.append("")
        out.append(contentsOf: narrative)
        if !stats.sparks.isEmpty {
            out.append("")
            out.append("## Sparks")
            for s in stats.sparks {
                let gates = gatesByRun[s.runID]
                let why = (gates?.isEmpty == false) ? " — failed \(gates!.joined(separator: ", "))" : ""
                let id = s.backlogID.isEmpty ? s.runID : s.backlogID
                let template = s.template.isEmpty ? "pipeline" : s.template
                out.append("- `\(id)` (\(template), \(plural(s.attempts, "attempt")))\(why)")
            }
        }
        if stats.bolts.count + stats.sparks.count > 0 {
            out.append("")
            out.append("## Departures")
            out.append("| when (UTC) | outcome | item | template | attempts |")
            out.append("|---|---|---|---|---|")
            let all = (stats.bolts + stats.sparks)
                .enumerated()
                .sorted { a, b in
                    a.element.endedAt == b.element.endedAt
                        ? a.offset < b.offset
                        : a.element.endedAt < b.element.endedAt
                }
                .map(\.element)
            for r in all {
                let id = r.backlogID.isEmpty ? r.runID : r.backlogID
                let template = r.template.isEmpty ? "—" : r.template
                out.append("| \(hhmmFormatter.string(from: r.endedAt)) | \(r.kind == .bolt ? "🟢 bolt" : "🟡 spark") | \(id) | \(template) | \(r.attempts) |")
            }
        }
        out.append("")
        return out.joined(separator: "\n")
    }
}
