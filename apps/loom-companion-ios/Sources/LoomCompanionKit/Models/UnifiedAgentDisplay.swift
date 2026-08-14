import Foundation

// UnifiedAgentDisplay — human titles and conversation grouping for agent
// rosters. Raw agent ids (`claude-code-2928522331-1011922604`) are identity,
// not presentation: every roster surface (Operator deck, Agents tab) should
// lead with the harness name plus a short conversation tag, and collapse the
// members of one conversation (a chat that moved across repos, or one app's
// workspace twins) into a single row. The grouping key is
// `UnifiedAgent.conversationId`, which already mirrors `fleetview.ConversationID`.

public extension UnifiedAgent {
    /// Humanized harness name: "claude-code" → "Claude Code", "codex" →
    /// "Codex". Prefers `agentType`; falls back to the agent-id base (the
    /// dash-joined parts before the first numeric segment) so presence rows
    /// from older servers without a type still read as a name, and finally to
    /// the raw id rather than an empty string.
    var harnessDisplayName: String {
        let type = agentType.trimmingCharacters(in: .whitespacesAndNewlines)
        // The decoder defaults an absent agent_type to "unknown" — treat that
        // as missing so the id-base fallback still names the harness.
        if !type.isEmpty && type.lowercased() != "unknown" {
            return UnifiedAgent.humanizeHarness(type)
        }
        let base = UnifiedAgent.agentIdBase(agentId)
        if !base.isEmpty { return UnifiedAgent.humanizeHarness(base) }
        return agentId
    }

    /// Short conversation tag ("#9048") — the last 4 digits of the
    /// conversation's numeric scope. Hex-flavored ids (Mills spawn pods like
    /// `spawn-claude-6e92ff16d7da`) shred into meaningless 1-2 digit runs
    /// ("#6", "#2"), so a scope under 4 digits is treated as degenerate and
    /// the tag falls back to the spawn id's tail ("#d7da") — stable, and it
    /// matches what the HUD spawn surfaces print. Empty when neither source
    /// yields anything.
    var conversationTag: String {
        let digitRuns = conversationId
            .split(whereSeparator: { !$0.isNumber })
            .map(String.init)
        if let scope = digitRuns.last, scope.count >= 4 {
            return "#" + String(scope.suffix(4))
        }
        if let spawn = spawnId?.trimmingCharacters(in: .whitespacesAndNewlines),
           spawn.count >= 4 {
            return "#" + String(spawn.suffix(4))
        }
        return ""
    }

    /// Roster row title: harness name plus the short conversation tag when one
    /// exists — "Claude Code · #9048". The raw agent id stays available for
    /// detail surfaces; it is never a title.
    var displayTitle: String {
        let tag = conversationTag
        return tag.isEmpty ? harnessDisplayName : "\(harnessDisplayName) · \(tag)"
    }

    /// What the agent is doing, cleaned for display under a harness-named
    /// title: prefers `currentTask` over `description`, strips a redundant
    /// leading harness prefix ("Claude Code · libs/fi-fhir" → "libs/fi-fhir"
    /// when the title already says Claude Code), and quiets the hooks'
    /// synthetic "Heartbeat bootstrap session" to "heartbeat only" — a
    /// keepalive is presence, not work. Empty when there is nothing to say.
    var cleanedActivityLine: String {
        // A spawn pod's currentTask is the prompt head — shouty truncated
        // instruction text, not status. The branch names the work ("feat/
        // bl-hud-spawn-split-a-…/extract"); show its item slug instead.
        if isSpawned {
            let slug = UnifiedAgent.branchSlug(branch)
            if !slug.isEmpty { return slug }
        }
        let raw = !currentTask.isEmpty ? currentTask : description
        let line = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if line.isEmpty { return "" }
        if line.caseInsensitiveCompare("Heartbeat bootstrap session") == .orderedSame {
            return "heartbeat only"
        }
        let prefix = harnessDisplayName + " · "
        if line.lowercased().hasPrefix(prefix.lowercased()) {
            return String(line.dropFirst(prefix.count))
                .trimmingCharacters(in: .whitespacesAndNewlines)
        }
        return line
    }

    /// Title for a row INSIDE a conversation fold, where the section header
    /// already names the harness ("Claude Code · conversation across repos")
    /// and every member would otherwise repeat the identical displayTitle. The
    /// discriminator is the workspace: the namespace minus the project prefix
    /// ("services/loom-core/fix/hud-ios-spawn-row-polish" → "fix/hud-ios-
    /// spawn-row-polish"), else the branch, else the workspace-hash tag from
    /// the agent id, else the displayTitle as a last resort.
    var workspaceTitle: String {
        let ns = (namespace ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        let proj = (project ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        if !ns.isEmpty {
            if !proj.isEmpty, ns.lowercased() != proj.lowercased(),
               ns.lowercased().hasPrefix(proj.lowercased() + "/") {
                let tail = String(ns.dropFirst(proj.count + 1))
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                if !tail.isEmpty { return tail }
            }
            if proj.isEmpty || ns.lowercased() != proj.lowercased() { return ns }
        }
        if !branch.isEmpty { return branch }
        let parts = agentId.split(separator: "-", omittingEmptySubsequences: false).map(String.init)
        if let ws = parts.first(where: { !$0.isEmpty && $0.allSatisfy(\.isNumber) }) {
            return "ws #" + String(ws.suffix(4))
        }
        return displayTitle
    }

    /// The informative middle of a work branch: drops the conventional type
    /// prefix ("feat/", "fix/", …) and any trailing slice segment, leaving the
    /// item slug — "feat/bl-hud-spawn-split-a-runtime-image-20260808/extract"
    /// → "bl-hud-spawn-split-a-runtime-image-20260808". Unrecognized shapes
    /// pass through unchanged; empty stays empty.
    static func branchSlug(_ branch: String) -> String {
        let trimmed = branch.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return "" }
        var parts = trimmed.split(separator: "/").map(String.init)
        let typePrefixes: Set<String> = [
            "feat", "fix", "refactor", "docs", "ci", "debt", "upgrade", "arch", "hotfix",
        ]
        if parts.count > 1, typePrefixes.contains(parts[0].lowercased()) {
            parts.removeFirst()
        }
        return parts.first ?? ""
    }

    /// The dash-joined agent-id parts before the first numeric segment
    /// ("claude-code-123-456" → "claude-code"). Empty when the id starts with
    /// a number or is empty.
    static func agentIdBase(_ agentId: String) -> String {
        let parts = agentId
            .trimmingCharacters(in: .whitespacesAndNewlines)
            .split(separator: "-", omittingEmptySubsequences: false)
            .map(String.init)
        func isDigits(_ s: String) -> Bool { !s.isEmpty && s.allSatisfy { $0 >= "0" && $0 <= "9" } }
        guard let wsIdx = parts.firstIndex(where: isDigits) else {
            return parts.joined(separator: "-")
        }
        return parts[0..<wsIdx].joined(separator: "-")
    }

    /// "claude-code" → "Claude Code"; unknown harnesses title-case per dash
    /// segment. Known two-letter/branded spellings pinned so they never drift
    /// into "Claude code" shapes.
    static func humanizeHarness(_ raw: String) -> String {
        switch raw.lowercased() {
        case "claude-code": return "Claude Code"
        case "claude-desktop": return "Claude Desktop"
        case "codex": return "Codex"
        case "gemini", "gemini-cli": return "Gemini"
        case "kilocode", "kilo": return "Kilocode"
        case "antigravity": return "Antigravity"
        default:
            return raw
                .split(separator: "-")
                .map { $0.prefix(1).uppercased() + $0.dropFirst() }
                .joined(separator: " ")
        }
    }
}

/// One conversation's members, in the caller's order. Grouping preserves the
/// input ordering (first-seen), so a roster sorted by urgency keeps its
/// urgency order after grouping — the representative is simply the first
/// (highest-ranked) member.
public struct UnifiedConversationGroup: Identifiable, Sendable {
    public let conversationId: String
    public let members: [UnifiedAgent]

    public var id: String { conversationId }
    public var representative: UnifiedAgent { members[0] }
    public var memberCount: Int { members.count }

    /// Group the (pre-sorted) roster one row per conversation. Agents whose
    /// conversation id is empty never collapse together — each keeps its own
    /// row keyed by agent id, because "unknown conversation" is not a
    /// conversation.
    public static func group(_ agents: [UnifiedAgent]) -> [UnifiedConversationGroup] {
        var byKey: [String: [UnifiedAgent]] = [:]
        var order: [String] = []
        for agent in agents {
            let conversation = agent.conversationId
            let key = conversation.isEmpty ? "agent:\(agent.agentId)" : conversation
            if byKey[key] == nil { order.append(key) }
            byKey[key, default: []].append(agent)
        }
        return order.compactMap { key in
            guard let members = byKey[key], !members.isEmpty else { return nil }
            return UnifiedConversationGroup(conversationId: key, members: members)
        }
    }
}
