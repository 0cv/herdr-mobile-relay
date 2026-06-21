import SwiftUI

@main
struct HerdiApp: App {
    @State private var relay = RelayConnection()

    var body: some Scene {
        WindowGroup {
            AgentListView()
                .environment(relay)
                .preferredColorScheme(.dark)
                .onAppear {
                    NotificationManager.shared.requestPermission()
                    LiveActivityManager.shared.start()
                }
        }
    }
}
