import Foundation
import Testing
@testable import LoomCompanionKit

/// Spinning Room + Plans board — control-plane client, wire decoding, and
/// the Kit-side semantics the mobile views lean on.
@Suite("MillsControlAPI")
struct MillsControlAPITests {

    // MARK: - Wire decoding (operator/HUD-shaped bytes)

    @Test("SpinRun decodes operator JSON (lowercase tags, RFC3339 dates, null-ish fields)")
    func decodesSpinRunFromOperatorJSON() throws {
        let json = """
        {
          "id": "spin-7f3a",
          "brief": "Harden the importer.\\nSecond line.",
          "frames": ["jacquard", "warp"],
          "priority": "P1",
          "status": "succeeded",
          "plan_ids": ["plan-1", "plan-2"],
          "competitive": true,
          "started_at": "2026-07-04T09:00:00Z",
          "ended_at": "2026-07-04T09:04:30Z"
        }
        """.data(using: .utf8)!
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let run = try decoder.decode(MillsSpinRun.self, from: json)
        #expect(run.id == "spin-7f3a")
        #expect(run.frames == ["jacquard", "warp"])
        #expect(run.statusKind == .succeeded)
        #expect(run.planIDs.count == 2)
        #expect(run.competitive)
        #expect(run.briefHeadline == "Harden the importer.")
        #expect(run.startedAt != nil)
    }

    @Test("SpinRun tolerates a minimal pending row (no plan_ids yet)")
    func decodesMinimalSpinRun() throws {
        let json = """
        {"id": "spin-min", "status": "pending", "started_at": "2026-07-04T09:00:00Z"}
        """.data(using: .utf8)!
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let run = try decoder.decode(MillsSpinRun.self, from: json)
        #expect(run.statusKind == .pending)
        #expect(run.planIDs.isEmpty)
        #expect(!run.statusKind.isTerminal)
    }

    @Test("SpinningRoom decodes the frames payload and defaults missing keys")
    func decodesSpinningRoom() throws {
        let json = """
        {"frames": [{"name": "jacquard", "model": "claude-opus-4-8", "backend": "anthropic"}]}
        """.data(using: .utf8)!
        let room = try JSONDecoder().decode(MillsSpinningRoom.self, from: json)
        #expect(room.enabled)
        #expect(room.available)
        #expect(room.defaultPriority == "P2")
        #expect(room.frames.count == 1)
        #expect(room.unavailableReason == nil)
    }

    @Test("SpinningRoom surfaces disabled/empty reasons")
    func spinningRoomReasons() throws {
        let disabled = MillsSpinningRoom(enabled: false, frames: [MillsFrame(name: "f", model: "m")])
        #expect(disabled.unavailableReason != nil)
        let empty = MillsSpinningRoom(frames: [])
        #expect(empty.unavailableReason != nil)
    }

    @Test("PlanList decodes the HUD plans payload including respin lineage")
    func decodesPlanList() throws {
        let json = """
        {
          "available": true,
          "plans": [
            {
              "id": "plan-1", "slug": "async-spins", "title": "Async spins",
              "project": "services/loom-core", "phase": "in_progress",
              "priority": "P1",
              "slice_summary": {"merged": 2, "implementing": 1},
              "updated_at": "2026-07-04T09:00:00Z"
            },
            {
              "id": "plan-2", "slug": "respun", "title": "Respun draft",
              "phase": "draft", "respun_from": "plan-1"
            }
          ],
          "count": 2
        }
        """.data(using: .utf8)!
        let list = try JSONDecoder().decode(MillsPlanList.self, from: json)
        #expect(list.available)
        #expect(list.plans.count == 2)
        #expect(list.plans[0].sliceSummary?["merged"] == 2)
        #expect(list.plans[1].respunFrom == "plan-1")
    }

    @Test("PlanList degrades to deploy-pending on available=false")
    func decodesUnavailablePlanList() throws {
        let json = """
        {"available": false, "reason": "plan store not available", "plans": [], "count": 0}
        """.data(using: .utf8)!
        let list = try JSONDecoder().decode(MillsPlanList.self, from: json)
        #expect(!list.available)
        #expect(list.plans.isEmpty)
    }

    // MARK: - Spin request body

    @Test("single-frame spin keeps the legacy {frame} shape")
    func singleFrameBody() throws {
        let body = MillsSpinRequest(brief: "b", frames: ["jacquard"]).bodyJSON()
        #expect(body["frame"] as? String == "jacquard")
        #expect(body["frames"] == nil)
    }

    @Test("multi-frame spin switches to {frames} and carries respun_from + scope")
    func competitiveBody() throws {
        let body = MillsSpinRequest(
            brief: "b", frames: ["a", "b"],
            priority: "P1", project: "services/loom-core",
            namespace: "mills/spun", respunFrom: "plan-1"
        ).bodyJSON()
        #expect(body["frames"] as? [String] == ["a", "b"])
        #expect(body["frame"] == nil)
        #expect(body["priority"] as? String == "P1")
        #expect(body["project"] as? String == "services/loom-core")
        #expect(body["respun_from"] as? String == "plan-1")
    }

    @Test("empty scope fields are omitted from the body")
    func emptyScopeOmitted() throws {
        let body = MillsSpinRequest(brief: "b", frames: ["f"], priority: "  ").bodyJSON()
        #expect(body["priority"] == nil)
        #expect(body["project"] == nil)
        #expect(body["namespace"] == nil)
        #expect(body["respun_from"] == nil)
    }

    // MARK: - Client behavior

    @Test("spinAsync returns the queued spin_id and threads the request")
    func spinAsyncQueues() async throws {
        let mock = MockAPIClient()
        mock.millsSpinQueuedResponse = MillsSpinQueued(spinID: "spin-42")
        let api = MillsControlAPI(client: mock)
        let id = try await api.spinAsync(MillsSpinRequest(brief: "b", frames: ["jacquard"]))
        #expect(id == "spin-42")
        #expect(mock.lastSpinRequest?.frames == ["jacquard"])
    }

    @Test("spinAsync surfaces a 401 instead of swallowing it")
    func spinAsyncSurfacesAuthFailure() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/spin/async"] =
            .apiError(code: .unauthorized, message: "invalid admin token", requestId: "")
        let api = MillsControlAPI(client: mock)
        await #expect(throws: LoomAPIError.self) {
            _ = try await api.spinAsync(MillsSpinRequest(brief: "b", frames: ["f"]))
        }
    }

    @Test("spinRuns degrades to [] when the operator is absent")
    func spinRunsDegrades() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/mills/spin/runs"] =
            .apiError(code: .upstreamError, message: "502", requestId: "")
        let api = MillsControlAPI(client: mock)
        let runs = try await api.spinRuns(limit: 10)
        #expect(runs.isEmpty)
    }

    @Test("plans degrades to deploy-pending on an older HUD (404)")
    func plansDegradesOn404() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/plans"] =
            .apiError(code: .notFound, message: "Not found", requestId: "")
        let api = MillsControlAPI(client: mock)
        let list = try await api.plans()
        #expect(!list.available)
    }

    @Test("advancePlan surfaces an illegal transition (422)")
    func advanceSurfacesIllegalTransition() async throws {
        let mock = MockAPIClient()
        mock.endpointFailures["/api/plans/plan-1/advance"] =
            .apiError(code: .unknown, message: "advance failed", requestId: "")
        let api = MillsControlAPI(client: mock)
        await #expect(throws: LoomAPIError.self) {
            try await api.advancePlan(id: "plan-1", toPhase: "done")
        }
    }

    @Test("mutation failure copy folds the admin-token case into actionable text")
    func mutationFailureCopy() {
        let msg = millsMutationFailureMessage(
            LoomAPIError.apiError(code: .unauthorized, message: "invalid admin token", requestId: "")
        )
        #expect(msg.contains("admin token"))
    }

    // MARK: - Board semantics

    @Test("visibleRuns pins live spins first, drops stale terminal spins")
    func spinBoardOrdering() {
        let now = Date()
        let runs = [
            MillsSpinRun(id: "old-done", brief: "", frames: ["f"], status: "succeeded",
                         startedAt: now.addingTimeInterval(-90000),
                         endedAt: now.addingTimeInterval(-90000)),
            MillsSpinRun(id: "fresh-done", brief: "", frames: ["f"], status: "succeeded",
                         startedAt: now.addingTimeInterval(-600),
                         endedAt: now.addingTimeInterval(-500)),
            MillsSpinRun(id: "live", brief: "", frames: ["f"], status: "running",
                         startedAt: now.addingTimeInterval(-60)),
        ]
        let visible = MillsSpinBoard.visibleRuns(runs, now: now)
        #expect(visible.map(\.id) == ["live", "fresh-done"])
        #expect(MillsSpinBoard.hasLiveSpin(runs))
        #expect(!MillsSpinBoard.hasLiveSpin([runs[0], runs[1]]))
    }

    @Test("plan phase semantics: tones, terminality, ordering")
    func planPhaseSemantics() {
        #expect(MillsPlanPhases.tone(for: "draft") == .draft)
        #expect(MillsPlanPhases.tone(for: "in_review") == .review)
        #expect(MillsPlanPhases.tone(for: "deployed") == .shipped)
        #expect(MillsPlanPhases.tone(for: "abandoned") == .abandoned)
        #expect(MillsPlanPhases.isTerminal("merged"))
        #expect(!MillsPlanPhases.isTerminal("in_progress"))
        #expect(MillsPlanPhases.sortIndex("draft") < MillsPlanPhases.sortIndex("in_review"))
        // abandoned sorts after everything, including unknown phases
        #expect(MillsPlanPhases.sortIndex("abandoned") > MillsPlanPhases.sortIndex("someday"))
        #expect(MillsPlanPhases.displayName("in_review") == "in review")
    }

    @Test("slice progress rolls up the summary in pipeline order")
    func sliceProgressRollup() {
        let progress = MillsSliceProgress.build(from: [
            "merged": 2, "pending": 1, "implementing": 1, "weird_phase": 1,
        ])
        #expect(progress != nil)
        #expect(progress?.total == 5)
        #expect(progress?.merged == 2)
        // Canonical order first (pending → implementing → merged), unknown last.
        #expect(progress?.segments.map(\.phase) == ["pending", "implementing", "merged", "weird_phase"])
        #expect(MillsSliceProgress.build(from: nil) == nil)
        #expect(MillsSliceProgress.build(from: [:]) == nil)
    }

    // MARK: - Respin briefs

    @Test("plan respin brief carries title, scope, and existing slices")
    func planRespinBrief() {
        let plan = MillsPlan(
            id: "plan-1", title: "Async spins", project: "services/loom-core",
            phase: "draft", priority: "P1",
            slices: [MillsPlanSlice(id: "s1", name: "Backend 202", phase: "merged",
                                    files: ["a.go", "b.go"])]
        )
        let brief = MillsRespinBrief.forPlan(plan)
        #expect(brief.contains("Plan: Async spins"))
        #expect(brief.contains("Priority: P1"))
        #expect(brief.contains("- Backend 202 (files: a.go, b.go)"))
    }

    @Test("sliceless plan respin brief asks for decomposition")
    func slicelessRespinBrief() {
        let plan = MillsPlan(id: "p", title: "Sparse", phase: "draft")
        #expect(MillsRespinBrief.forPlan(plan).contains("no slices"))
    }

    @Test("slice respin brief carries plan + slice context")
    func sliceRespinBrief() {
        let plan = MillsPlan(id: "p", title: "Parent", phase: "planned")
        let slice = MillsPlanSlice(id: "s", name: "One slice", phase: "pending",
                                   files: ["x.go"], decisions: ["use flock"])
        let brief = MillsRespinBrief.forSlice(slice, of: plan)
        #expect(brief.contains("From plan: Parent"))
        #expect(brief.contains("Slice: One slice"))
        #expect(brief.contains("Files: x.go"))
        #expect(brief.contains("- use flock"))
    }

    // MARK: - Endpoint wiring

    @Test("new endpoints carry the right method, path, and mutation flag")
    func endpointWiring() throws {
        #expect(Endpoint.millsSpinningRoomFrames.path == "/api/mills/spinning-room/frames")
        #expect(Endpoint.millsSpinRuns(limit: nil).path == "/api/mills/spin/runs")
        #expect(Endpoint.millsSpinRun(id: "x").path == "/api/mills/spin/runs/x")
        #expect(Endpoint.plans.path == "/api/plans")
        #expect(Endpoint.planDetail(id: "p").path == "/api/plans/p")
        #expect(Endpoint.planAdvance(id: "p", toPhase: "done").path == "/api/plans/p/advance")
        #expect(Endpoint.millsSpinningRoomFrames.method == "GET")
        let spin = Endpoint.millsSpinAsync(request: MillsSpinRequest(brief: "b", frames: ["f"]))
        #expect(spin.method == "POST")
        #expect(spin.isMutation)
        #expect(Endpoint.planAdvance(id: "p", toPhase: "done").isMutation)
    }

    @Test("planAdvance body carries to_phase")
    func planAdvanceBody() throws {
        let req = try Endpoint.planAdvance(id: "p", toPhase: "planned")
            .urlRequest(baseURL: URL(string: "https://hud.local")!)
        let body = try JSONSerialization.jsonObject(with: req.httpBody ?? Data()) as? [String: Any]
        #expect(body?["to_phase"] as? String == "planned")
    }

    @Test("millsSpinRuns threads the limit query")
    func spinRunsLimitQuery() throws {
        let req = try Endpoint.millsSpinRuns(limit: 20)
            .urlRequest(baseURL: URL(string: "https://hud.local")!)
        #expect(req.url?.query == "limit=20")
    }
}
