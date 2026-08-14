import AppIntents
import LoomCompanionKit

struct GetLoomBriefingIntent: AppIntent {
    static var title: LocalizedStringResource = "Get Loom Briefing"
    static var description: IntentDescription = "Create a private, on-device briefing from the current Loom fleet snapshot."
    static var openAppWhenRun = false

    func perform() async throws -> some IntentResult & ProvidesDialog {
        guard let data = SharedDataStore.load() else {
            return .result(dialog: "Unable to load Loom data. Open the Loom app to refresh.")
        }

        let snapshot = LoomBriefingSnapshot(widgetData: data)
        let service = AppleIntelligenceBriefingService()

        guard service.availability == .available else {
            return .result(dialog: "\(snapshot.factualSummary)")
        }

        do {
            let briefing = try await service.generate(from: snapshot)
            return .result(dialog: "\(briefing)")
        } catch {
            return .result(dialog: "\(snapshot.factualSummary)")
        }
    }
}
