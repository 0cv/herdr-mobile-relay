import ActivityKit

final class LiveActivityManager {
    static let shared = LiveActivityManager()
    private var activity: Activity<HerdiWidgetAttributes>?
    private init() {}

    func start() {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return }
        let state = HerdiWidgetAttributes.ContentState(blockedCount: 0, workingCount: 0, idleCount: 0)
        activity = try? Activity.request(attributes: HerdiWidgetAttributes(), content: .init(state: state, staleDate: nil))
    }

    func update(blocked: Int, working: Int, idle: Int) {
        let state = HerdiWidgetAttributes.ContentState(blockedCount: blocked, workingCount: working, idleCount: idle)
        Task { await activity?.update(.init(state: state, staleDate: nil)) }
    }

    func end() {
        let state = HerdiWidgetAttributes.ContentState(blockedCount: 0, workingCount: 0, idleCount: 0)
        Task { await activity?.end(.init(state: state, staleDate: nil), dismissalPolicy: .immediate) }
    }
}
