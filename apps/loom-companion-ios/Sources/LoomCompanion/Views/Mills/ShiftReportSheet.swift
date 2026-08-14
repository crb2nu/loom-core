// ShiftReportSheet — the end-of-shift story, on mobile. Port of the web
// Factory panel's ShiftReport overlay: re-reads the last 24 hours of
// terminal runs and tells them as deterministic prose (bolts woven, sparks
// struck, patterns stamped, retries, spend), then hands the same
// standup-pasteable markdown to the share sheet. No summarizer — same runs
// in, same words out.
//
// All semantics live in `LoomCompanionKit`'s `MillsShiftReport` (shared
// with any future surface and pinned by MillsShiftReportTests); this file
// is presentation only. Sparks enrich in the background with their failing
// gate names via bounded per-run detail fetches — the report renders
// immediately and the gate reasons fill in when they land.

import LoomCompanionKit
import SwiftUI

struct ShiftReportSheet: View {
    let api: MillsAPIProtocol

    @Environment(\.dismiss) private var dismiss

    /// The report is a snapshot: the window anchors to the moment it opened.
    @State private var openedAt = Date()
    @State private var loading = true
    @State private var loadError: String?
    @State private var stats: MillsShiftStats?
    @State private var gateSummaries: [MillsSparkGateSummary] = []
    /// True when the Pattern Loom catalog could not be read (older daemon
    /// without /api/patterns in the mobile allowlist → 403, or an operator
    /// that is down). The report still renders; pattern attribution is just
    /// missing, and we say so instead of silently dropping the section.
    @State private var patternsUnavailable = false

    /// Resolve gate detail for at most this many sparks — a spark storm
    /// should not fan out into dozens of detail fetches.
    private let gateFetchMax = 8

    private var markdown: String {
        guard let stats else { return "" }
        return MillsShiftReport.markdown(
            stats: stats,
            narrative: MillsShiftReport.narrative(stats),
            generatedAt: openedAt,
            gateSummaries: gateSummaries)
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: LoomSpacing.lg) {
                    if loading {
                        loadingState
                    } else if let loadError {
                        errorState(loadError)
                    } else if let stats {
                        report(stats)
                    }
                }
                .padding(.horizontal, LoomSpacing.lg)
                .padding(.vertical, LoomSpacing.lg)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(LoomColors.bgPrimary.ignoresSafeArea())
            .navigationTitle("Shift report")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button("Done") { dismiss() }
                        .tint(LoomColors.accent)
                }
                ToolbarItem(placement: .topBarTrailing) {
                    ShareLink(
                        item: markdown,
                        subject: Text("Mills shift report"),
                        preview: SharePreview("Mills shift report — last 24h")
                    ) {
                        Label("Share", systemImage: "square.and.arrow.up")
                    }
                    .tint(LoomColors.accent)
                    .disabled(loading || loadError != nil)
                    .accessibilityLabel("Share shift report markdown")
                }
            }
        }
        .task { await load() }
    }

    // MARK: - Load

    private func load() async {
        // Patterns and backlog degrade to empty (attribution just goes
        // quiet); only the archive read itself surfaces as an error —
        // matching the web overlay.
        async let patternsTask = (try? api.approvedPatternsResult())
            ?? MillsPatternsResult(patterns: [], unavailable: true)
        async let backlogTask = (try? api.backlog()) ?? []
        let runs: [MillsPipelineRun]
        do {
            runs = try await api.terminalRuns(limit: 500)
        } catch {
            loadError = "Couldn't tally the shift. Pull down to dismiss and retry."
            loading = false
            return
        }
        let patternsResult = await patternsTask
        patternsUnavailable = patternsResult.unavailable
        let shift = MillsShiftReport.window(runs, now: openedAt)
        let built = MillsShiftReport.stats(shift, patterns: patternsResult.patterns, backlog: await backlogTask)
        stats = built
        loading = false

        // Background gate enrichment: sequential keeps it gentle on the
        // operator; each landed summary re-renders its spark row.
        for spark in built.sparks.prefix(gateFetchMax) {
            guard !Task.isCancelled else { return }
            guard let detail = try? await api.runDetail(id: spark.runID) else { continue }
            let failed = (detail.gates ?? [])
                .filter { $0.outcome == "fail" }
                .map(\.gateName)
            gateSummaries.append(MillsSparkGateSummary(runID: spark.runID, failedGates: failed))
        }
    }

    // MARK: - Report body

    private func report(_ stats: MillsShiftStats) -> some View {
        let narrative = MillsShiftReport.narrative(stats)
        let gatesByRun = Dictionary(
            gateSummaries.map { ($0.runID, $0.failedGates) },
            uniquingKeysWith: { a, _ in a })
        return VStack(alignment: .leading, spacing: LoomSpacing.lg) {
            Text("the last 24 hours, told straight — every line a real run")
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgMuted)

            VStack(alignment: .leading, spacing: LoomSpacing.sm) {
                ForEach(Array(narrative.enumerated()), id: \.offset) { index, line in
                    Text(line)
                        .font(index == 0 ? LoomTypography.headlineMedium : LoomTypography.caption)
                        .foregroundStyle(index == 0 ? LoomColors.fgPrimary : LoomColors.fgSecondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .loomCard(priority: .standard)

            chips(stats)

            if patternsUnavailable {
                Label(
                    "patterns unavailable — this HUD didn't serve the Pattern Loom catalog, so pattern attribution is missing from this shift",
                    systemImage: "questionmark.circle"
                )
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgMuted)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
            }

            if !stats.sparks.isEmpty {
                sparksSection(stats, gatesByRun: gatesByRun)
            }

            Text("\(stats.bolts.count + stats.sparks.count) departures this shift · share the markdown for the standup thread")
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgMuted)
                .frame(maxWidth: .infinity, alignment: .center)
        }
    }

    private func chips(_ stats: MillsShiftStats) -> some View {
        HStack(spacing: LoomSpacing.xs) {
            LoomPill(
                "\(stats.bolts.count) bolts",
                icon: "checkmark.seal.fill",
                color: LoomColors.statusHealthy,
                style: stats.bolts.isEmpty ? .outlined : .tinted)
            LoomPill(
                "\(stats.sparks.count) sparks",
                icon: "exclamationmark.triangle.fill",
                color: LoomColors.statusDegraded,
                style: stats.sparks.isEmpty ? .outlined : .tinted)
            if !stats.retried.isEmpty {
                LoomPill("\(stats.retried.count) retried", color: LoomColors.fgMuted, style: .outlined)
            }
            if stats.costUSD > 0 {
                LoomPill("$\(String(format: "%.2f", stats.costUSD))", color: LoomColors.fgMuted, style: .outlined)
            }
        }
    }

    private func sparksSection(_ stats: MillsShiftStats, gatesByRun: [String: [String]]) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            Text("SPARKS ON THE FLOOR")
                .font(LoomTypography.sectionTitle)
                .tracking(0.8)
                .foregroundStyle(LoomColors.fgSecondary)
            ForEach(stats.sparks, id: \.runID) { spark in
                sparkRow(spark, failed: gatesByRun[spark.runID])
            }
        }
    }

    private func sparkRow(_ spark: MillsShiftRun, failed: [String]?) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
            HStack(spacing: LoomSpacing.xs) {
                Text(spark.endedAt, format: .dateTime.hour().minute())
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                Text(spark.backlogID.isEmpty ? spark.runID : spark.backlogID)
                    .font(LoomTypography.monoMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 0)
            }
            Text(sparkMeta(spark, failed: failed))
                .font(LoomTypography.monoCaption)
                .foregroundStyle(failed?.isEmpty == false ? LoomColors.statusDegraded : LoomColors.fgMuted)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .compact, accent: .severity(LoomColors.statusDegraded))
    }

    private func sparkMeta(_ spark: MillsShiftRun, failed: [String]?) -> String {
        let template = spark.template.isEmpty ? "pipeline" : spark.template
        var meta = "\(template) · \(spark.attempts) attempt\(spark.attempts == 1 ? "" : "s")"
        if let failed {
            meta += failed.isEmpty ? " · no failing gate recorded" : " · failed \(failed.joined(separator: ", "))"
        }
        return meta
    }

    // MARK: - Loading / error

    private var loadingState: some View {
        VStack(alignment: .leading, spacing: LoomSpacing.lg) {
            Text("tallying the shift…")
                .font(LoomTypography.monoCaption)
                .foregroundStyle(LoomColors.fgMuted)
                .frame(maxWidth: .infinity, alignment: .center)
                .padding(.top, LoomSpacing.xxl)
            ForEach(0..<3, id: \.self) { _ in
                SkeletonSessionRow().loomCard(priority: .standard)
            }
        }
    }

    private func errorState(_ message: String) -> some View {
        LoomEmptyState(
            tone: .idle,
            title: "Couldn't tally the shift",
            detail: message
        )
        .loomCard(priority: .standard, accent: .severity(LoomColors.statusDegraded))
        .padding(.top, LoomSpacing.xxl)
    }
}
