// LiveSessionsView.swift — Phase 5 of the spectator plan
// (`.loom/99-implementation-plan-agent-telemetry-spectator-2026-05-04.md`).
//
// iOS-side mirror of the HUD `LiveSessionsCard` (Phase 3). Subscribes to
// the existing app-wide `SSEEventBroadcaster` and renders every active
// agent session with its trailing tool calls. Tap a row to drill into
// the full ring buffer.

import SwiftUI
import LoomCompanionKit

public struct LiveSessionsView: View {
    @State private var viewModel: LiveSessionsViewModel

    private let broadcaster: SSEEventBroadcaster
    private let onOpenSession: (String) -> Void

    public init(
        apiClient: APIClient?,
        broadcaster: SSEEventBroadcaster,
        onOpenSession: @escaping (String) -> Void
    ) {
        self.broadcaster = broadcaster
        self.onOpenSession = onOpenSession
        _viewModel = State(initialValue: LiveSessionsViewModel(apiClient: apiClient))
    }

    public var body: some View {
        NavigationStack {
            content
                .navigationTitle("Live Sessions")
                .navigationBarTitleDisplayMode(.inline)
                .task { viewModel.subscribe(to: broadcaster) }
                .onDisappear { viewModel.unsubscribe() }
        }
    }

    @ViewBuilder
    private var content: some View {
        let sessions = viewModel.visibleSessions
        if sessions.isEmpty {
            emptyState
        } else {
            List {
                Section {
                    summaryRow
                }

                Section("Sessions") {
                    ForEach(sessions, id: \.id) { session in
                        Button {
                            onOpenSession(session.id)
                        } label: {
                            sessionRow(session)
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
            .listStyle(.insetGrouped)
            .loomTabBarClearance()
            .refreshable {
                await viewModel.loadInitialSessions(force: true)
            }
        }
    }

    private var summaryRow: some View {
        HStack {
            Label("\(viewModel.activeSessionCount) active", systemImage: "circle.fill")
                .labelStyle(.titleAndIcon)
                .foregroundStyle(.green)
                .font(.subheadline)
            Spacer()
            if viewModel.inFlightCallCount > 0 {
                Label("\(viewModel.inFlightCallCount) in flight", systemImage: "bolt.horizontal.fill")
                    .labelStyle(.titleAndIcon)
                    .foregroundStyle(.orange)
                    .font(.subheadline)
            }
        }
    }

    private var emptyState: some View {
        Group {
            if viewModel.isLoadingSnapshot {
                VStack(spacing: LoomSpacing.md) {
                    ProgressView()
                    Text("Linking active sessions…")
                        .foregroundStyle(.secondary)
                }
            } else if let error = viewModel.snapshotError {
                ContentUnavailableView {
                    Label("Live sessions unavailable", systemImage: "wifi.exclamationmark")
                } description: {
                    Text(error.description)
                } actions: {
                    Button("Try Again") {
                        Task { await viewModel.loadInitialSessions(force: true) }
                    }
                }
            } else {
                ContentUnavailableView {
                    Label("No active sessions", systemImage: "waveform.path.badge.minus")
                } description: {
                    Text("Active sessions will appear here from the same session index used by the Sessions tab.")
                }
            }
        }
        .loomTabBarClearance()
    }

    @ViewBuilder
    private func sessionRow(_ session: LiveSession) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Circle()
                    .fill(color(for: session.agentStatus))
                    .frame(width: 8, height: 8)
                Text(session.agentID.isEmpty ? "(unknown agent)" : session.agentID)
                    .font(.headline)
                    .lineLimit(1)
                Spacer()
                Text(String(session.id.prefix(8)))
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                if session.endedAt != nil {
                    Text("ended")
                        .font(.caption2)
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .background(.gray.opacity(0.2))
                        .clipShape(Capsule())
                        .foregroundStyle(.secondary)
                }
            }

            if !session.namespace.isEmpty || !session.description.isEmpty {
                Text(session.description.isEmpty ? session.namespace : session.description)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }

            if let task = session.currentTask, !task.isEmpty {
                Label(task, systemImage: "scope")
                    .font(.caption)
                    .foregroundStyle(LoomColors.info)
                    .lineLimit(1)
            }

            // Show up to 3 most-recent calls inline.
            ForEach(Array(session.recentCalls.prefix(3)), id: \.id) { call in
                HStack(spacing: 6) {
                    Circle()
                        .fill(color(for: call))
                        .frame(width: 6, height: 6)
                    Text(call.displayName)
                        .font(.caption.monospaced())
                        .lineLimit(1)
                    Spacer()
                    Text(formatDuration(call.durationMs))
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                }
            }

            if session.recentCalls.count > 3 {
                Text("+\(session.recentCalls.count - 3) more")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            } else if session.recentCalls.isEmpty {
                Text(session.entryCount > 0 ? "Loading recent activity…" : "Waiting for activity…")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                    .italic()
            }
        }
        .padding(.vertical, 4)
    }

    private func color(for status: LiveAgentStatus) -> Color {
        switch status {
        case .active: return .green
        case .idle: return .yellow
        case .offline, .expired: return .gray
        case .unknown: return .secondary
        }
    }

    private func color(for call: LiveToolCall) -> Color {
        if call.inFlight { return .blue }
        if call.error != nil { return .red }
        if let exit = call.exitCode, exit != 0 { return .red }
        return .green
    }

    private func formatDuration(_ ms: Int?) -> String {
        guard let ms else { return "—" }
        if ms < 1000 { return "\(ms)ms" }
        return String(format: "%.1fs", Double(ms) / 1000.0)
    }
}
