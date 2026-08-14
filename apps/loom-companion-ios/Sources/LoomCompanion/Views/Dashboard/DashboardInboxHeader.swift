import SwiftUI

/// The Dashboard's top-of-surface signal strip. Mirrors the HUD triage
/// overview header (Slice A2): an "Inbox" label plus a count pill that reads
/// either "clear" (success-toned, when pressureCount is 0) or "N pressure
/// point(s)" (severity-toned), and a mono "last refreshed" timestamp on the
/// right.
///
/// This is what tells the operator at a glance whether they need to act —
/// the rest of the dashboard is the *how*.
struct DashboardInboxHeader: View {
    let pressureCount: Int
    let topSeverity: Severity
    let updatedAgo: String?
    /// When true the strip renders a disclosure chevron, signalling that it
    /// opens the alert inbox. The Dashboard wraps it in a Button.
    var isInteractive: Bool = false
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    enum Severity {
        case critical
        case warning
        case info
        case nominal

        var color: Color {
            switch self {
            case .critical: return LoomColors.statusCritical
            case .warning: return LoomColors.statusDegraded
            case .info: return LoomColors.statusInfo
            case .nominal: return LoomColors.statusHealthy
            }
        }
    }

    var body: some View {
        Group {
            if dynamicTypeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: LoomSpacing.xs) {
                    HStack(alignment: .firstTextBaseline, spacing: LoomSpacing.sm) {
                        inboxLabel
                        countPill
                        disclosure
                    }
                    updatedLabel
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            } else {
                HStack(alignment: .firstTextBaseline, spacing: LoomSpacing.sm) {
                    inboxLabel
                    countPill
                    disclosure
                    Spacer(minLength: LoomSpacing.sm)
                    updatedLabel
                }
            }
        }
        .padding(.horizontal, LoomSpacing.xxs)
        .contentShape(Rectangle())
    }

    @ViewBuilder
    private var disclosure: some View {
        if isInteractive {
            Image(systemName: "chevron.right")
                .font(.system(size: 10, weight: .semibold))
                .foregroundStyle(LoomColors.fgMuted)
        }
    }

    private var inboxLabel: some View {
        Text("INBOX")
            .font(LoomTypography.kindLabel)
            .tracking(LoomTypography.kindLabelTracking)
            .foregroundStyle(LoomColors.fgSecondary)
    }

    @ViewBuilder
    private var updatedLabel: some View {
        if let updatedAgo {
            HStack(spacing: LoomSpacing.xxs) {
                Image(systemName: "clock")
                    .font(.system(size: 9, weight: .medium))
                    .foregroundStyle(LoomColors.fgMuted)
                Text(updatedAgo)
                    .font(LoomTypography.monoCaption)
                    .foregroundStyle(LoomColors.fgMuted)
                    .contentTransition(.numericText())
            }
            .accessibilityLabel("Last refreshed \(updatedAgo)")
        }
    }

    @ViewBuilder
    private var countPill: some View {
        if pressureCount == 0 {
            LoomPill(
                "clear",
                icon: "checkmark",
                color: LoomColors.statusHealthy,
                style: .tinted,
                weight: .compact
            )
        } else {
            LoomPill(
                "\(pressureCount) pressure point\(pressureCount == 1 ? "" : "s")",
                icon: "exclamationmark.circle.fill",
                color: topSeverity.color,
                style: .tinted,
                weight: .compact
            )
        }
    }
}

#Preview("DashboardInboxHeader · states") {
    VStack(spacing: LoomSpacing.lg) {
        DashboardInboxHeader(pressureCount: 0, topSeverity: .nominal, updatedAgo: "2s ago")
        DashboardInboxHeader(pressureCount: 1, topSeverity: .warning, updatedAgo: "8s ago")
        DashboardInboxHeader(pressureCount: 4, topSeverity: .critical, updatedAgo: "1m ago")
        DashboardInboxHeader(pressureCount: 12, topSeverity: .critical, updatedAgo: nil)
    }
    .padding()
    .background(LoomColors.bgPrimary)
    .preferredColorScheme(.dark)
}
