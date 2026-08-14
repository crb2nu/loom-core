import UIKit
import UserNotifications
import LoomCompanionKit

/// UIKit application delegate for push notification registration.
final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {

    /// Shared API client, injected from the SwiftUI app lifecycle
    /// (`LoomCompanionApp` assigns this on launch and whenever the pairing
    /// state changes). Setting it flushes any device token that arrived before
    /// the app was paired — APNs typically delivers the token within a second
    /// of launch, long before an unpaired install has a client to register it
    /// with, so without the flush the token was dropped forever.
    var apiClient: (any LoomAPIClientProtocol)? {
        didSet { registerPendingTokenIfNeeded() }
    }

    /// Last device token handed to us by APNs, held until a client exists and
    /// the registration POST succeeds.
    private var pendingDeviceToken: String?

    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        UNUserNotificationCenter.current().delegate = self

        UNUserNotificationCenter.current().requestAuthorization(
            options: [.alert, .badge, .sound]
        ) { granted, error in
            if let error {
                print("[AppDelegate] Push authorization error: \(error.localizedDescription)")
                return
            }
            guard granted else { return }
            DispatchQueue.main.async {
                application.registerForRemoteNotifications()
            }
        }
        return true
    }

    func application(
        _ application: UIApplication,
        didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data
    ) {
        pendingDeviceToken = deviceToken.map { String(format: "%02x", $0) }.joined()
        registerPendingTokenIfNeeded()
    }

    /// Register the held device token with the HUD once a client is available.
    /// Both call sites run on the main thread (UIKit delegate callback and the
    /// SwiftUI lifecycle), so no extra synchronisation is needed.
    ///
    /// On failure the token is kept so the next client injection retries; the
    /// manual token field in Settings → Push Notifications remains available
    /// as a fallback.
    private func registerPendingTokenIfNeeded() {
        guard let client = apiClient, let tokenHex = pendingDeviceToken else { return }

        Task { @MainActor in
            do {
                let response: PushRegistrationResponse = try await client.request(
                    .pushRegister(token: tokenHex, platform: .apns)
                )
                if response.registered {
                    self.pendingDeviceToken = nil
                }
            } catch {
                print("[AppDelegate] Push token registration failed: \(error)")
            }
        }
    }

    func application(
        _ application: UIApplication,
        didFailToRegisterForRemoteNotificationsWithError error: Error
    ) {
        print("[AppDelegate] Remote notification registration failed: \(error.localizedDescription)")
    }

    // MARK: - UNUserNotificationCenterDelegate

    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification
    ) async -> UNNotificationPresentationOptions {
        [.banner, .sound, .badge]
    }

    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse
    ) async {
        let userInfo = response.notification.request.content.userInfo
        guard let urlString = userInfo["deep_link"] as? String,
              let url = URL(string: urlString) else { return }

        await MainActor.run {
            // Post notification for the SwiftUI app to handle via onOpenURL.
            UIApplication.shared.open(url)
        }
    }
}
