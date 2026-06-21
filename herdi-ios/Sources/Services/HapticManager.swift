import UIKit

final class HapticManager {
    static let shared = HapticManager()
    private init() {}

    func blocked() {
        UINotificationFeedbackGenerator().notificationOccurred(.warning)
    }

    func sent() {
        UIImpactFeedbackGenerator(style: .medium).impactOccurred()
    }
}
