// WeaverScreen — S7b of weaver-qwen3 plan.
//
// Read-only surface for the HUD's weaver + aimodels endpoints. Mirrors
// the HUD WeaverPanel.svelte: status header (with degraded banner),
// metrics, recent queries, role defaults. No mutations land from this
// screen; submission is intentionally deferred until the daemon-socket
// auth contract for mobile is settled.
//
// Spec: services/loom-core/.loom/111-product-spec-weaver-qwen3-
// integration-2026-05-08.md (IOS-002).

import LoomCompanionKit
import SwiftUI

public struct WeaverScreen: View {
    @State private var status: WeaverStatus?
    @State private var history: [WeaverHistoryEntry] = []
    @State private var metrics: WeaverMetrics = WeaverMetrics()
    @State private var roles: [AIModelRoleEntry] = []
    @State private var overridePath: String?
    @State private var loading = true
    @State private var loadError: String?

    private let api: WeaverAPIProtocol?

    public init(apiClient: APIClient?) {
        self.api = apiClient.map(WeaverAPI.init(client:))
    }

    /// Test-only initializer accepting a fake protocol implementation.
    public init(api: WeaverAPIProtocol?) {
        self.api = api
    }

    public var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: LoomSpacing.md) {
                if let err = loadError {
                    errorCard(err)
                } else if loading && status == nil {
                    ProgressView("Loading weaver…")
                        .tint(LoomColors.accent)
                        .font(LoomTypography.caption)
                        .foregroundStyle(LoomColors.textSecondary)
                        .frame(maxWidth: .infinity, alignment: .center)
                        .padding(LoomSpacing.lg)
                } else if let s = status, s.enabled {
                    if s.isDegraded {
                        degradedCard(s)
                    }
                    statusCard(s)
                    metricsCard(metrics)
                    if !roles.isEmpty {
                        rolesCard(roles, overridePath: overridePath)
                    }
                    historyCard(history)
                } else {
                    // Covers both `{"enabled": false}` and the degradation
                    // catch in WeaverAPI.status() (404/503 → enabled: false).
                    EmptyCard(text: "Weaver is not configured on this HUD.")
                }
            }
            .padding(.vertical, LoomSpacing.lg)
        }
        .background(LoomColors.bgPrimary.ignoresSafeArea())
        .navigationTitle("Weaver")
        .refreshable { await reload() }
        .task { await reload() }
    }

    // MARK: - Cards

    private func degradedCard(_ s: WeaverStatus) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            HStack(spacing: LoomSpacing.xs) {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(LoomColors.statusDegraded)
                Text("Model preflight: degraded")
                    .font(LoomTypography.headlineMedium)
                    .foregroundStyle(LoomColors.statusDegraded)
            }
            if let e = s.catalogError, !e.isEmpty {
                Text("FlexInfer /v1/models unreachable: \(e)")
                    .font(LoomTypography.bodyRegular)
                    .foregroundStyle(LoomColors.textPrimary)
            } else {
                let count = s.missingModels?.count ?? 0
                Text("\(count) configured model\(count == 1 ? "" : "s") not advertised by FlexInfer.")
                    .font(LoomTypography.bodyRegular)
                    .foregroundStyle(LoomColors.textPrimary)
            }
            if let missing = s.missingModels, !missing.isEmpty {
                modelChipRow(missing, tone: .missing)
            }
            if let ready = s.readyModels, !ready.isEmpty {
                Text("Ready (\(ready.count)):")
                    .font(LoomTypography.caption)
                    .foregroundStyle(LoomColors.textSecondary)
                modelChipRow(ready, tone: .ready)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard, accent: .severity(LoomColors.statusDegraded))
        .padding(.horizontal, LoomSpacing.lg)
    }

    private func statusCard(_ s: WeaverStatus) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.sm) {
            row("Enabled", value: s.enabled ? "yes" : "no")
            if let r = s.routerModel { row("Router model", value: r) }
            if let sub = s.subagentModel { row("Subagent model", value: sub) }
            if let domains = s.domains {
                row("Domains", value: "\(domains.count)")
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard)
        .padding(.horizontal, LoomSpacing.lg)
    }

    private func metricsCard(_ m: WeaverMetrics) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            sectionHeader("Metrics")
            row("Total queries", value: "\(m.totalQueries)")
            row("Avg latency", value: m.avgLatencyMs >= 1000
                ? String(format: "%.1fs", m.avgLatencyMs / 1000)
                : "\(Int(m.avgLatencyMs))ms")
            row("Error rate", value: String(format: "%.1f%%", m.errorRate * 100))
            row("Total tokens", value: "\(m.totalTokens)")
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard)
        .padding(.horizontal, LoomSpacing.lg)
    }

    private func rolesCard(_ rs: [AIModelRoleEntry], overridePath: String?) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            sectionHeader("Role defaults")
            ForEach(rs) { r in
                HStack(alignment: .firstTextBaseline) {
                    Text(r.role)
                        .font(LoomTypography.monoMedium)
                        .foregroundStyle(LoomColors.textSecondary)
                    Spacer(minLength: 8)
                    Text(r.primary.isEmpty ? "—" : r.primary)
                        .font(LoomTypography.monoMedium)
                        .foregroundStyle(LoomColors.textPrimary)
                }
                .padding(.vertical, 2)
            }
            if let path = overridePath, !path.isEmpty {
                Text("Override: \(path)")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textTertiary)
                    .padding(.top, 4)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard)
        .padding(.horizontal, LoomSpacing.lg)
    }

    private func historyCard(_ entries: [WeaverHistoryEntry]) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            sectionHeader("Recent queries (\(entries.count))")
            if entries.isEmpty {
                Text("No queries yet.")
                    .font(LoomTypography.bodyRegular)
                    .foregroundStyle(LoomColors.textSecondary)
            } else {
                ForEach(entries.prefix(20)) { e in
                    historyRow(e)
                    Divider().overlay(LoomColors.border)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard)
        .padding(.horizontal, LoomSpacing.lg)
    }

    private func historyRow(_ e: WeaverHistoryEntry) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                StatusBadge(e.status ?? "?", color: badgeColor(for: e.status))
                Spacer()
                if let ms = e.latencyMs {
                    Text("\(ms)ms")
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.textTertiary)
                }
                if let t = e.totalTokens {
                    Text("\(t)t")
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.textTertiary)
                }
            }
            Text(e.query ?? "(no query)")
                .font(LoomTypography.bodyRegular)
                .foregroundStyle(LoomColors.textPrimary)
                .lineLimit(2)
            if let domains = e.domainsUsed, !domains.isEmpty {
                Text("domains: \(domains.joined(separator: ", "))")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textTertiary)
            }
            if let parent = e.parentSessionId, !parent.isEmpty {
                Text("parent session: \(parent)")
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.textTertiary)
            }
        }
        .padding(.vertical, 4)
    }

    private func errorCard(_ msg: String) -> some View {
        VStack(alignment: .leading, spacing: LoomSpacing.xs) {
            Text("Could not load weaver")
                .font(LoomTypography.headlineMedium)
                .foregroundStyle(LoomColors.statusCritical)
            Text(msg)
                .font(LoomTypography.bodyRegular)
                .foregroundStyle(LoomColors.textSecondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .loomCard(priority: .standard, accent: .severity(LoomColors.statusCritical))
        .padding(.horizontal, LoomSpacing.lg)
    }

    // MARK: - Helpers

    private func sectionHeader(_ title: String) -> some View {
        Text(title.uppercased())
            .font(LoomTypography.sectionTitle)
            .tracking(0.8)
            .foregroundStyle(LoomColors.fgSecondary)
    }

    private func row(_ label: String, value: String) -> some View {
        HStack {
            Text(label)
                .font(LoomTypography.bodyRegular)
                .foregroundStyle(LoomColors.textSecondary)
            Spacer()
            Text(value)
                .font(LoomTypography.monoMedium)
                .foregroundStyle(LoomColors.textPrimary)
        }
    }

    private enum ChipTone { case missing, ready }

    private func modelChipRow(_ models: [String], tone: ChipTone) -> some View {
        FlowLayout(spacing: 6) {
            ForEach(models, id: \.self) { m in
                Text(m)
                    .font(.caption2)
                    .monospaced()
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(chipColor(tone).opacity(0.12))
                    .foregroundStyle(chipColor(tone))
                    .overlay(
                        RoundedRectangle(cornerRadius: 4).stroke(chipColor(tone), lineWidth: 1)
                    )
                    .clipShape(RoundedRectangle(cornerRadius: 4))
            }
        }
    }

    private func chipColor(_ tone: ChipTone) -> Color {
        tone == .missing ? LoomColors.statusCritical : LoomColors.statusHealthy
    }

    private func badgeColor(for status: String?) -> Color {
        switch status {
        case "ok", "success": return LoomColors.statusHealthy
        case "error": return LoomColors.statusCritical
        default: return LoomColors.fgMuted
        }
    }

    private func reload() async {
        guard let api else {
            loading = false
            return
        }
        loading = true
        loadError = nil
        async let s = api.status()
        async let h = api.history()
        async let m = api.metrics()
        async let r = api.roles()
        do {
            self.status = try await s
            self.history = (try await h).entries ?? []
            self.metrics = try await m
            let rolesResp = try await r
            self.roles = rolesResp.roles
            self.overridePath = rolesResp.overridePath
        } catch {
            self.loadError = "\(error)"
        }
        loading = false
    }
}

// MARK: - Tiny FlowLayout

/// Minimal flow layout for chip rows. Avoids pulling in additional
/// dependencies; iOS 17+ Layout API gives us this for free.
private struct FlowLayout: Layout {
    var spacing: CGFloat

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let maxWidth = proposal.width ?? .infinity
        var x: CGFloat = 0
        var y: CGFloat = 0
        var rowHeight: CGFloat = 0
        var totalWidth: CGFloat = 0
        for sub in subviews {
            let size = sub.sizeThatFits(.unspecified)
            if x + size.width > maxWidth {
                y += rowHeight + spacing
                x = 0
                rowHeight = 0
            }
            rowHeight = max(rowHeight, size.height)
            x += size.width + spacing
            totalWidth = max(totalWidth, x)
        }
        return CGSize(width: totalWidth, height: y + rowHeight)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var x = bounds.minX
        var y = bounds.minY
        var rowHeight: CGFloat = 0
        for sub in subviews {
            let size = sub.sizeThatFits(.unspecified)
            if x + size.width > bounds.maxX {
                y += rowHeight + spacing
                x = bounds.minX
                rowHeight = 0
            }
            sub.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(size))
            x += size.width + spacing
            rowHeight = max(rowHeight, size.height)
        }
    }
}

private struct EmptyCard: View {
    let text: String
    var body: some View {
        Text(text)
            .font(LoomTypography.bodyRegular)
            .foregroundStyle(LoomColors.textSecondary)
            .frame(maxWidth: .infinity, alignment: .center)
            .loomCard(priority: .standard)
            .padding(.horizontal, LoomSpacing.lg)
    }
}

#Preview {
    NavigationStack {
        WeaverScreen(api: WeaverScreenPreviewAPI())
    }
}

private struct WeaverScreenPreviewAPI: WeaverAPIProtocol, Sendable {
    func status() async throws -> WeaverStatus {
        WeaverStatus(
            enabled: true,
            routerModel: "qwen3-1p7b-tools-radeonvii",
            subagentModel: "qwen3-8b",
            domains: [
                WeaverDomainSummary(name: "ops", description: "Operations", model: "qwen3-8b", backend: "flexinfer", tools: ["t1", "t2"]),
                WeaverDomainSummary(name: "ci", model: "qwen3-8b", backend: "flexinfer", tools: ["t1"]),
            ],
            degraded: false,
            missingModels: [],
            readyModels: ["qwen3-1p7b-tools-radeonvii", "qwen3-8b"],
            catalogSize: 8
        )
    }

    func history() async throws -> WeaverHistoryResponse {
        WeaverHistoryResponse(entries: [
            WeaverHistoryEntry(
                queryId: "q1",
                query: "What is the latest cluster status?",
                status: "ok",
                latencyMs: 1280,
                totalTokens: 412,
                domainsUsed: ["ops"]
            )
        ])
    }

    func metrics() async throws -> WeaverMetrics {
        WeaverMetrics(totalQueries: 12, avgLatencyMs: 1450, errorRate: 0.0, totalTokens: 4892, errorCount: 0)
    }

    func roles() async throws -> AIModelRolesResponse {
        AIModelRolesResponse(
            roles: [
                AIModelRoleEntry(role: "weaver-router", primary: "qwen3-1p7b-tools-radeonvii"),
                AIModelRoleEntry(role: "weaver-subagent", primary: "qwen3-8b"),
                AIModelRoleEntry(role: "mills-judge", primary: "qwen3-8b"),
            ],
            overridePath: "/Users/example/.config/loom/aimodel-roles.yaml"
        )
    }
}
