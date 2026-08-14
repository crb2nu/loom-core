import Testing
import Foundation
@testable import LoomCompanionKit

/// Decoding contract tests for `SpawnConfig` (GET /api/mobile/v1/agent/spawn/config).
///
/// Older HUDs marshal a nil Go slice as JSON `null` for `projects`. The decode
/// must tolerate that (and a missing key) instead of throwing away the
/// agent types and defaults that arrived in the same payload — a null-sunk
/// decode is why the spawn form never showed server-driven picker data.
@Suite("SpawnConfig decoding")
struct SpawnConfigDecodingTests {

    @Test("Decodes fully-populated config")
    func decodesFullConfig() throws {
        let payload = """
        {
          "agent_types": [{ "id": "claude-code", "name": "Claude Code", "available": true }],
          "projects": [{ "name": "loom-core", "path": "services/loom-core" }],
          "defaults": { "agent_type": "claude-code", "base_branch": "main", "memory_mb": 4096, "cpus": 2.0, "timeout_minutes": 60 }
        }
        """
        let config = try JSONDecoder().decode(SpawnConfig.self, from: Data(payload.utf8))
        #expect(config.agentTypes.count == 1)
        #expect(config.projects.count == 1)
        #expect(config.projects.first?.name == "loom-core")
        #expect(config.defaults.agentType == "claude-code")
    }

    @Test("Null projects decodes to empty array, keeping agent types and defaults")
    func decodesNullProjects() throws {
        let payload = """
        {
          "agent_types": [{ "id": "claude-code", "name": "Claude Code", "available": true }],
          "projects": null,
          "defaults": { "agent_type": "claude-code", "base_branch": "main", "memory_mb": 4096, "cpus": 2.0, "timeout_minutes": 60 }
        }
        """
        let config = try JSONDecoder().decode(SpawnConfig.self, from: Data(payload.utf8))
        #expect(config.projects.isEmpty)
        #expect(config.agentTypes.count == 1)
        #expect(config.defaults.baseBranch == "main")
    }

    @Test("Missing projects key decodes to empty array")
    func decodesMissingProjects() throws {
        let payload = """
        {
          "agent_types": [],
          "defaults": { "agent_type": "claude-code", "base_branch": "main", "memory_mb": 4096, "cpus": 2.0, "timeout_minutes": 60 }
        }
        """
        let config = try JSONDecoder().decode(SpawnConfig.self, from: Data(payload.utf8))
        #expect(config.projects.isEmpty)
        #expect(config.agentTypes.isEmpty)
    }
}
