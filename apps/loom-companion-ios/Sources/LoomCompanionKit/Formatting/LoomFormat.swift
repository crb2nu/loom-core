import Foundation

/// Canonical display-enrichment helpers shared across the app and widgets.
///
/// Every view that turns a raw value — an ISO-8601 timestamp, a token/entry
/// count, an elapsed duration, a "/"-separated path, an agent vendor string —
/// into user-facing text routes through here. The goal is that the *same datum
/// reads the same way everywhere*.
///
/// Before this existed, ~6 hand-rolled copies disagreed for a single timestamp:
/// `"5m30s ago"` (Agents) vs `"5m ago"` (Dashboard) vs `"5m"` (Sessions) vs a
/// raw `"2026-06-28T10:30:45.123Z"` (Session detail). Token counts similarly
/// split between `"12.4k"`, `"12400"`, `"12.4k tokens"`, and a copy missing the
/// millions tier. Centralizing the rules makes enrichment consistent by
/// construction and unit-testable in one place.
///
/// Pure `Foundation` only — no `SwiftUI` — so the widget extension and the app
/// both link the same logic, and agent brand colors are exposed as raw RGB data
/// that each target turns into its own `Color`.
public enum LoomFormat {

    // MARK: - ISO-8601 Parsing

    private static let isoFractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    private static let isoPlain: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    /// Parse an ISO-8601 timestamp, tolerating the presence or absence of
    /// fractional seconds (the backend emits both forms).
    public static func date(fromISO iso: String) -> Date? {
        isoFractional.date(from: iso) ?? isoPlain.date(from: iso)
    }

    // MARK: - Relative Time (long form, with "ago")

    /// `"just now"`, `"5s ago"`, `"5m ago"`, `"3h ago"`, `"2d ago"`.
    ///
    /// Used in detail panels and headers where horizontal space allows. Tiers
    /// are shared with ``relativeCompact(seconds:)`` so the numeric magnitude
    /// never disagrees between a list row and the detail it opens.
    public static func relative(seconds: Int) -> String {
        let s = max(0, seconds)
        if s < 5 { return "just now" }
        if s < 60 { return "\(s)s ago" }
        if s < 3600 { return "\(s / 60)m ago" }
        if s < 86_400 { return "\(s / 3600)h ago" }
        return "\(s / 86_400)d ago"
    }

    /// Relative time from an ISO-8601 string. Returns `"—"` when unparseable so
    /// callers never surface a raw timestamp by accident.
    public static func relative(fromISO iso: String, now: Date = Date()) -> String {
        guard let date = date(fromISO: iso) else { return "—" }
        return relative(seconds: Int(now.timeIntervalSince(date)))
    }

    /// Relative time from a concrete `Date`.
    public static func relative(from date: Date, now: Date = Date()) -> String {
        relative(seconds: Int(now.timeIntervalSince(date)))
    }

    // MARK: - Relative Time (compact, no suffix)

    /// `"now"`, `"5s"`, `"5m"`, `"3h"`, `"2d"` — for dense list rows / trailing
    /// metrics where the surrounding layout already implies "ago".
    public static func relativeCompact(seconds: Int) -> String {
        let s = max(0, seconds)
        if s < 5 { return "now" }
        if s < 60 { return "\(s)s" }
        if s < 3600 { return "\(s / 60)m" }
        if s < 86_400 { return "\(s / 3600)h" }
        return "\(s / 86_400)d"
    }

    /// Compact relative time from an ISO-8601 string. Returns `"—"` when
    /// unparseable.
    public static func relativeCompact(fromISO iso: String, now: Date = Date()) -> String {
        guard let date = date(fromISO: iso) else { return "—" }
        return relativeCompact(seconds: Int(now.timeIntervalSince(date)))
    }

    // MARK: - Absolute Time

    private static let absoluteFormatter: DateFormatter = {
        let f = DateFormatter()
        f.dateStyle = .medium
        f.timeStyle = .short
        return f
    }()

    /// A precise, locale-aware timestamp (e.g. `"Jun 28, 2026 at 10:30 AM"`) for
    /// record/detail panels. Returns the original string if it can't be parsed.
    public static func absolute(fromISO iso: String) -> String {
        guard let date = date(fromISO: iso) else { return iso }
        return absoluteFormatter.string(from: date)
    }

    // MARK: - Compact Numbers

    /// Abbreviate a count with one decimal: `950 -> "950"`, `12_400 -> "12.4k"`,
    /// `1_200_000 -> "1.2M"`. The single tiered rule for tokens, entries, and any
    /// large counter. `String(format:)` is locale-independent here, so output is
    /// deterministic.
    public static func compact(_ n: Int) -> String {
        if n >= 1_000_000 {
            return String(format: "%.1fM", Double(n) / 1_000_000.0)
        }
        if n >= 1_000 {
            return String(format: "%.1fk", Double(n) / 1_000.0)
        }
        return "\(n)"
    }

    /// A compact count with an explicit unit suffix, e.g. `"12.4k tokens"`.
    public static func tokens(_ n: Int, unit: String = "tokens") -> String {
        "\(compact(n)) \(unit)"
    }

    // MARK: - Durations

    /// Compact elapsed duration from whole seconds: `"5s"`, `"3m"`, `"2h"`,
    /// `"1d"`. Distinct from ``relativeCompact(seconds:)`` only in that a length
    /// of zero reads `"0s"`, not `"now"`.
    public static func duration(seconds: Int) -> String {
        let s = max(0, seconds)
        if s < 60 { return "\(s)s" }
        if s < 3600 { return "\(s / 60)m" }
        if s < 86_400 { return "\(s / 3600)h" }
        return "\(s / 86_400)d"
    }

    /// Elapsed duration from milliseconds: `"850ms"`, `"1.2s"`, then rolls up to
    /// the second-based tiers for longer spans.
    public static func duration(millis: Int) -> String {
        let ms = max(0, millis)
        if ms < 1_000 { return "\(ms)ms" }
        if ms < 60_000 { return String(format: "%.1fs", Double(ms) / 1_000.0) }
        return duration(seconds: ms / 1_000)
    }

    /// Duration from fractional seconds, keeping one decimal of precision in the
    /// chosen unit: `45.0 -> "45s"`, `90.0 -> "1.5m"`, `5400.0 -> "1.5h"`. Mirrors
    /// the HUD's `fmtDuration` so the Mills "slice→merge p50" KPI reads the same
    /// on mobile as it does on the web. `nil`/non-finite renders the em dash.
    public static func duration(seconds: Double?) -> String {
        guard let s = seconds, s.isFinite, s >= 0 else { return "—" }
        if s < 60 { return String(format: "%.0fs", s) }
        let m = s / 60
        if m < 60 { return String(format: "%.1fm", m) }
        let h = m / 60
        if h < 24 { return String(format: "%.1fh", h) }
        return String(format: "%.1fd", h / 24)
    }

    // MARK: - Ratios & Currency

    /// A `0...1` ratio as a whole-number percent: `0.93 -> "93%"`. `nil`/non-finite
    /// renders the em dash so callers can pass an optional metric straight through.
    /// Pass `decimals: 1` for the tighter `"92.5%"` form used in dense KPI rows.
    public static func percent(_ v: Double?, decimals: Int = 0) -> String {
        guard let v, v.isFinite else { return "—" }
        return String(format: "%.\(max(0, decimals))f%%", v * 100)
    }

    /// USD with tiered precision so small and large dollar figures stay legible:
    /// `4.22 -> "$4.22"`, `12.5 -> "$12.5"`, `120 -> "$120"`. Mirrors the HUD's
    /// `fmtUSD`. `nil`/non-finite renders the em dash.
    public static func usd(_ v: Double?) -> String {
        guard let v, v.isFinite else { return "—" }
        if v >= 100 { return String(format: "$%.0f", v) }
        if v >= 10 { return String(format: "$%.1f", v) }
        return String(format: "$%.2f", v)
    }

    // MARK: - Paths

    /// Last path component (basename) of a "/"-separated path, with the full
    /// string as fallback. Replaces scattered
    /// `path.components(separatedBy: "/").last ?? path` call sites.
    public static func lastPathComponent(_ path: String) -> String {
        path.split(separator: "/").last.map(String.init) ?? path
    }

    // MARK: - Agent Branding

    /// Canonical per-vendor brand: RGB components in `0...1` plus an SF Symbol
    /// name. Exposed as pure data so the app's `LoomColors` and the widget's
    /// Live Activity helpers build the *same* color without this module
    /// depending on SwiftUI.
    public struct AgentBrand: Equatable, Sendable {
        public let red: Double
        public let green: Double
        public let blue: Double
        public let icon: String

        public init(red: Double, green: Double, blue: Double, icon: String) {
            self.red = red
            self.green = green
            self.blue = blue
            self.icon = icon
        }
    }

    /// Brand for unrecognized agent types — cyan, matching the app's `info`
    /// token.
    public static let defaultAgentBrand = AgentBrand(
        red: 0.0, green: 0.784, blue: 1.0, icon: "cpu.fill"
    )

    /// Resolve the brand for an agent type / vendor string (case-insensitive).
    public static func agentBrand(_ type: String) -> AgentBrand {
        switch type.lowercased() {
        case "claude-code", "claude":
            return AgentBrand(red: 1.0, green: 0.420, blue: 0.616,
                              icon: "terminal.fill")              // #FF6B9D
        case "gemini":
            return AgentBrand(red: 0.0, green: 0.784, blue: 1.0,
                              icon: "wand.and.sparkles")          // #00C8FF
        case "codex":
            return AgentBrand(red: 0.133, green: 0.878, blue: 0.463,
                              icon: "chevron.left.forwardslash.chevron.right") // #22E076
        case "copilot":
            return AgentBrand(red: 1.0, green: 0.722, blue: 0.188,
                              icon: "cpu.fill")                   // #FFB830
        case "kilocode":
            return AgentBrand(red: 0.690, green: 0.424, blue: 0.871,
                              icon: "ruler.fill")                 // #B06CDE
        case "antigravity":
            return AgentBrand(red: 1.0, green: 0.420, blue: 0.208,
                              icon: "arrow.up.circle.fill")       // #FF6B35
        default:
            return defaultAgentBrand
        }
    }
}
