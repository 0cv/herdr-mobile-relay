import SwiftUI
import ServiceManagement

@main
struct HerdiMacApp: App {
    @State private var relay = RelayConnection()
    @AppStorage("launchAtLogin") private var launchAtLogin = false

    var body: some Scene {
        MenuBarExtra {
            MenuBarPanel(relay: relay, launchAtLogin: $launchAtLogin)
                .frame(width: 360, height: 480)
        } label: {
            let blocked = relay.agents.filter { $0.status == .blocked }.count
            if blocked > 0 {
                Label("\(blocked)", systemImage: "exclamationmark.circle.fill")
            } else {
                Image(systemName: relay.isConnected ? "circle.fill" : "circle")
            }
        }
        .menuBarExtraStyle(.window)
        .onChange(of: launchAtLogin) { _, newValue in
            do {
                if newValue {
                    try SMAppService.mainApp.register()
                } else {
                    try SMAppService.mainApp.unregister()
                }
            } catch {
                launchAtLogin = !newValue
            }
        }
    }
}
