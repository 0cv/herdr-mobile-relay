import ActivityKit

struct HerdiWidgetAttributes: ActivityAttributes {
    public struct ContentState: Codable, Hashable {
        var blockedCount: Int
        var workingCount: Int
        var idleCount: Int
    }
}
