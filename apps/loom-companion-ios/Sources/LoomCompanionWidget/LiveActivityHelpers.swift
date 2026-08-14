import SwiftUI
import LoomCompanionKit

// MARK: - Shared helpers for Live Activity views

/// Return an SF Symbol name for the given agent type. Delegates to the shared
/// ``LoomFormat/agentBrand(_:)`` so widgets match the app exactly.
func agentIcon(_ agentType: String) -> String {
    LoomFormat.agentBrand(agentType).icon
}

/// Return a brand color for the given agent type, built from the same canonical
/// RGB data the app's `LoomColors.agentTypeColor` uses (previously this widget
/// rendered a divergent palette — e.g. Claude as brown rather than pink).
func agentColor(_ agentType: String) -> Color {
    let brand = LoomFormat.agentBrand(agentType)
    return Color(red: brand.red, green: brand.green, blue: brand.blue)
}

/// Return an SF Symbol name representing the session status.
func statusDot(_ status: String) -> String {
    switch status {
    case "active": return "circle.fill"
    case "idle": return "circle.dotted"
    case "ended", "summarized": return "checkmark.circle.fill"
    case "error", "failed": return "exclamationmark.circle.fill"
    default: return "circle"
    }
}

/// Return a color for the given session status.
func statusDotColor(_ status: String) -> Color {
    switch status {
    case "active": return .green
    case "idle": return .orange
    case "ended": return .gray
    case "summarized": return .blue
    case "error", "failed": return .red
    default: return .secondary
    }
}

/// Format a token count for compact display (e.g., 1200 -> "1.2k", 1.2M).
func formatTokens(_ count: Int) -> String {
    LoomFormat.compact(count)
}
