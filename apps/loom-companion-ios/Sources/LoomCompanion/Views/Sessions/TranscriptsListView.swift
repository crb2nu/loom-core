// TranscriptsListView — the phone's window into what the CLIs actually did.
//
// First app-side consumer of the vendor-session bridge (LoomCompanionKit/
// VendorSessions), which mirrors the workstation's claude/codex transcripts
// onto the HUD (!1251 HTTP surface + federation mirror). The agent-context
// Sessions list shows loom's session ENVELOPES (entries, tasks, telemetry);
// this view shows the conversations themselves: newest-modified transcript
// files, plus substring search with role-tagged snippet hits.
//
// Degradation contract (mirrors VendorSessionsAPI's two tiers): `degraded`
// renders "bridge offline", `unavailable` renders "needs a newer HUD" —
// neither is ever presented as "no sessions".

import LoomCompanionKit
import SwiftUI

struct TranscriptsListView: View {
    private let api: VendorSessionsAPIProtocol?

    @State private var loading = true
    @State private var sessions: [VendorSession] = []
    @State private var degraded = false
    @State private var unavailable = false
    @State private var loadError: String?

    @State private var query = ""
    @State private var searching = false
    @State private var matches: [VendorSessionMatch]?

    init(apiClient: APIClient?) {
        self.api = apiClient.map(VendorSessionsAPI.init(client:))
    }

    /// Test seam.
    init(api: VendorSessionsAPIProtocol?) {
        self.api = api
    }

    var body: some View {
        List {
            if api == nil {
                stateRow("Pair with a HUD to browse transcripts.", icon: "wifi.slash")
            } else if loading {
                stateRow("Reading transcripts…", icon: "clock")
            } else if unavailable {
                stateRow("Transcript routes need a newer HUD.", icon: "arrow.triangle.2.circlepath")
            } else if degraded {
                stateRow("HUD's agent bridge is offline — transcript list unavailable.", icon: "bolt.slash")
            } else if let loadError {
                stateRow(loadError, icon: "exclamationmark.triangle")
            } else if let matches {
                searchResults(matches)
            } else if sessions.isEmpty {
                stateRow("No transcripts on this workstation yet.", icon: "text.bubble")
            } else {
                Section("Recent transcripts") {
                    ForEach(sessions, id: \.uid) { session in
                        sessionRow(session)
                    }
                }
            }
        }
        .navigationTitle("Transcripts")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $query, prompt: "Search inside transcripts")
        .onSubmit(of: .search) { Task { await runSearch() } }
        .onChange(of: query) { _, newValue in
            if newValue.isEmpty { matches = nil }
        }
        .overlay(alignment: .top) {
            if searching { ProgressView().padding(.top, 4) }
        }
        .refreshable { await load() }
        .task { await load() }
    }

    // MARK: - Rows

    private func sessionRow(_ session: VendorSession) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: LoomSpacing.xs) {
                vendorBadge(session.vendor)
                Text(scopeLabel(cwd: session.cwd, path: session.path))
                    .font(LoomTypography.bodyMedium)
                    .foregroundStyle(LoomColors.fgPrimary)
                    .lineLimit(1)
                Spacer(minLength: 0)
                if let modified = session.modifiedAt {
                    Text(LoomFormat.relativeCompact(fromISO: modified))
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.fgMuted)
                }
            }
            HStack(spacing: LoomSpacing.xs) {
                Text(String(session.id.suffix(12)))
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                if let bytes = session.sizeBytes {
                    Text(Int64(bytes).formatted(.byteCount(style: .file)))
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.fgMuted)
                }
                if let host = session.host, !host.isEmpty {
                    Text(host)
                        .font(LoomTypography.monoCaption)
                        .foregroundStyle(LoomColors.info)
                }
                Spacer(minLength: 0)
            }
        }
        .padding(.vertical, 2)
        .contextMenu {
            Button {
                query = scopeLabel(cwd: session.cwd, path: session.path)
                Task { await runSearch() }
            } label: {
                Label("Search this scope", systemImage: "magnifyingglass")
            }
        }
    }

    @ViewBuilder
    private func searchResults(_ matches: [VendorSessionMatch]) -> some View {
        if matches.isEmpty {
            stateRow("No matches for “\(query)”.", icon: "magnifyingglass")
        } else {
            Section("Matches") {
                ForEach(Array(matches.enumerated()), id: \.offset) { _, match in
                    VStack(alignment: .leading, spacing: 3) {
                        HStack(spacing: LoomSpacing.xs) {
                            vendorBadge(match.vendor)
                            if let role = match.role, !role.isEmpty {
                                Text(role)
                                    .font(LoomTypography.monoCaption)
                                    .foregroundStyle(LoomColors.info)
                            }
                            Text(scopeLabel(cwd: match.cwd, path: match.path))
                                .font(LoomTypography.caption)
                                .foregroundStyle(LoomColors.fgSecondary)
                                .lineLimit(1)
                            Spacer(minLength: 0)
                            if let ts = match.timestamp {
                                Text(LoomFormat.relativeCompact(fromISO: ts))
                                    .font(LoomTypography.monoCaption)
                                    .foregroundStyle(LoomColors.fgMuted)
                            }
                        }
                        Text(match.snippet)
                            .font(LoomTypography.caption)
                            .foregroundStyle(LoomColors.fgPrimary)
                            .lineLimit(3)
                    }
                    .padding(.vertical, 2)
                }
            }
        }
    }

    private func stateRow(_ message: String, icon: String) -> some View {
        HStack(spacing: LoomSpacing.sm) {
            Image(systemName: icon)
                .foregroundStyle(LoomColors.fgMuted)
            Text(message)
                .font(LoomTypography.caption)
                .foregroundStyle(LoomColors.fgSecondary)
        }
        .padding(.vertical, 4)
    }

    private func vendorBadge(_ vendor: String) -> some View {
        Text(vendor)
            .font(LoomTypography.monoCaption)
            .foregroundStyle(LoomColors.accent)
            .padding(.horizontal, 6)
            .padding(.vertical, 1)
            .background(Capsule().strokeBorder(LoomColors.accent.opacity(0.4), lineWidth: 1))
    }

    /// A transcript's human scope: the cwd's tail (repo dir) when known, else
    /// the file name. Never the full path — rows must scan.
    private func scopeLabel(cwd: String?, path: String) -> String {
        if let cwd, !cwd.isEmpty {
            let tail = cwd.split(separator: "/").suffix(2).joined(separator: "/")
            if !tail.isEmpty { return tail }
        }
        return path.split(separator: "/").last.map(String.init) ?? path
    }

    // MARK: - Data

    private func load() async {
        guard let api else { return }
        loading = sessions.isEmpty
        do {
            let result = try await api.recentSessions(cwdContains: nil, limit: 50)
            sessions = result.sessions
            degraded = result.degraded
            unavailable = result.unavailable
            loadError = nil
        } catch {
            loadError = "Couldn't read transcripts. Pull to retry."
        }
        loading = false
    }

    private func runSearch() async {
        guard let api, !query.trimmingCharacters(in: .whitespaces).isEmpty else { return }
        searching = true
        defer { searching = false }
        do {
            let result = try await api.search(query: query, cwdContains: nil, maxResults: 40)
            matches = result.matches
            degraded = result.degraded
            unavailable = result.unavailable
        } catch {
            loadError = "Search failed. Try again."
        }
    }
}
