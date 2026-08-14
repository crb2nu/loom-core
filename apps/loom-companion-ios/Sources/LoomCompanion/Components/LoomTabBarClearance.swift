import SwiftUI

extension View {
    /// Keeps the final actionable content above the floating iOS tab bar.
    func loomTabBarClearance() -> some View {
        safeAreaInset(edge: .bottom) {
            Color.clear
                .frame(height: 96)
                .allowsHitTesting(false)
                .accessibilityHidden(true)
        }
    }
}
