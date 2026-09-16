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
    /// Control surface for the one-tap taste grade. nil (test init without a
    /// control fake, or an unpaired admin token) renders the bolts read-only.
    var controlAPI: MillsControlAPIProtocol?

    @Environment(\.dismiss) private var dismiss

    /// The report is a snapshot: the window anchors to the moment it opened.
    @State private var openedAt = Date()
    @State private var loading = true
    @State private var loadError: String?
    @State private var stats: MillsShiftStats?
    @State private var gateSummaries: [MillsSparkGateSummary] = []
    /// backlogID → grade for bolts already graded (seeded from the backlog
    /// read, updated optimistically after a successful one-tap).
    @State private var gradeByBacklogID: [String: String] = [:]
    /// runID currently being graded — disables that row's buttons.
    @State private var gradingRunID: String?
    /// Last grade failure, rendered under the bolts header until the next
    /// successful tap.
    @State private var gradeError: String?
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
        let backlog = await backlogTask
        let built = MillsShiftReport.stats(shift, patterns: patternsResult.patterns, backlog: backlog)
        stats = built
        // Seed graded state so an already-graded bolt shows its grade instead
        // of offering the one-tap again.
        gradeByBacklogID = Dictionary(
            backlog.compactMap { item in
                guard let grade = item.grade, !grade.isEmpty else { return nil }
                return (item.id, grade)
            },
            uniquingKeysWith: { a, _ in a })
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

            if !stats.bolts.isEmpty {
                boltsSection(stats)
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

    // MARK: - Bolts + taste

    /// The bolts of the shift, each with the one-tap taste grade. Grading is
    /// the human half of the taste loop — the S5/S6 autonomy gate holds until
    /// enough merged bolts carry a grade — so the report is where the tap
    /// should live: you just read what the bolt did.
    private func boltsSection(_ stats: MillsShiftStats) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            Text("BOLTS OFF THE LOOM")
                .font(LoomTypography.sectionTitle)
                .tracking(0.8)
                .foregroundStyle(LoomColors.fgSecondary)
            if let gradeError {
                Text(gradeError)
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.statusDegraded)
                    .fixedSize(horizontal: false, vertical: true)
            }
            ForEach(stats.bolts, id: \.runID) { bolt in
                boltRow(bolt)
            }
        }
    }

    private func boltRow(_ bolt: MillsShiftRun) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xxs) {
            HStack(spacing: LoomSpacing.xs) {
                Text(bolt.endedAt, format: .dateTime.hour().minute())
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                Text(bolt.backlogID.isEmpty ? bolt.runID : bolt.backlogID)
                    .font(LoomTypography.monoMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 0)
            }
            HStack(spacing: LoomSpacing.xs) {
                Text(boltMeta(bolt))
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                Spacer(minLength: 0)
                if let grade = gradeByBacklogID[bolt.backlogID], let known = MillsGrade(rawValue: grade) {
                    LoomPill(grade, icon: known.icon, color: gradeColor(known), style: .tinted)
                } else if controlAPI != nil {
                    gradeButtons(bolt)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .compact, accent: .severity(LoomColors.statusHealthy))
    }

    private func gradeButtons(_ bolt: MillsShiftRun) -> some View {
        HStack(spacing: LoomSpacing.xs) {
            ForEach(MillsGrade.allCases, id: \.rawValue) { grade in
                Button {
                    Task { await gradeBolt(bolt, grade: grade) }
                } label: {
                    Image(systemName: grade.icon)
                        .font(.footnote)
                        .foregroundStyle(gradeColor(grade))
                        .frame(width: 30, height: 26)
                        .background(gradeColor(grade).opacity(0.12), in: RoundedRectangle(cornerRadius: 6))
                }
                .buttonStyle(.plain)
                .disabled(gradingRunID != nil)
                .accessibilityLabel("Grade \(grade.rawValue)")
            }
        }
        .opacity(gradingRunID == bolt.runID ? 0.4 : 1)
    }

    private func gradeBolt(_ bolt: MillsShiftRun, grade: MillsGrade) async {
        guard let controlAPI else { return }
        gradingRunID = bolt.runID
        defer { gradingRunID = nil }
        do {
            let ack = try await controlAPI.gradeRun(id: bolt.runID, grade: grade, note: nil)
            gradeByBacklogID[ack.itemID] = ack.grade
            gradeError = nil
        } catch {
            gradeError = millsMutationFailureMessage(error)
        }
    }

    private func gradeColor(_ grade: MillsGrade) -> Color {
        switch grade {
        case .keep: return LoomColors.statusHealthy
        case .meh: return LoomColors.fgMuted
        case .regret: return LoomColors.statusDegraded
        }
    }

    private func boltMeta(_ bolt: MillsShiftRun) -> String {
        let template = bolt.template.isEmpty ? "pipeline" : bolt.template
        var meta = "\(template) · \(bolt.attempts) attempt\(bolt.attempts == 1 ? "" : "s")"
        if let cost = bolt.costUSD, cost > 0 {
            meta += " · $\(String(format: "%.2f", cost))"
        }
        return meta
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
