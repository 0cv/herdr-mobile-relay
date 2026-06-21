import SwiftUI
import WidgetKit

struct AgentStatusEntry: TimelineEntry {
    let date: Date
    let blocked: Int
    let working: Int
    let idle: Int
}

struct AgentStatusProvider: TimelineProvider {
    func placeholder(in context: Context) -> AgentStatusEntry {
        AgentStatusEntry(date: .now, blocked: 0, working: 2, idle: 1)
    }

    func getSnapshot(in context: Context, completion: @escaping (AgentStatusEntry) -> Void) {
        completion(AgentStatusEntry(date: .now, blocked: 0, working: 0, idle: 0))
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<AgentStatusEntry>) -> Void) {
        // Read from shared UserDefaults (written by the main app)
        let defaults = UserDefaults(suiteName: "group.com.dcolinmorgan.herdi")
        let b = defaults?.integer(forKey: "blocked_count") ?? 0
        let w = defaults?.integer(forKey: "working_count") ?? 0
        let i = defaults?.integer(forKey: "idle_count") ?? 0
        let entry = AgentStatusEntry(date: .now, blocked: b, working: w, idle: i)
        let next = Calendar.current.date(byAdding: .second, value: 30, to: .now)!
        completion(Timeline(entries: [entry], policy: .after(next)))
    }
}

struct HerdiWidgetView: View {
    let entry: AgentStatusEntry

    var body: some View {
        VStack(spacing: 8) {
            Text("herdi").font(.caption2).foregroundStyle(.secondary)
            HStack(spacing: 12) {
                statBadge(count: entry.blocked, color: .red, icon: "exclamationmark.circle.fill")
                statBadge(count: entry.working, color: .green, icon: "circle.fill")
                statBadge(count: entry.idle, color: .gray, icon: "circle.fill")
            }
        }
        .containerBackground(.fill.tertiary, for: .widget)
    }

    private func statBadge(count: Int, color: Color, icon: String) -> some View {
        VStack(spacing: 2) {
            Image(systemName: icon).foregroundStyle(color)
            Text("\(count)").font(.title3.bold())
        }
    }
}

struct HerdiWidget: Widget {
    let kind = "HerdiWidget"

    var body: some WidgetConfiguration {
        StaticConfiguration(kind: kind, provider: AgentStatusProvider()) { entry in
            HerdiWidgetView(entry: entry)
        }
        .configurationDisplayName("Agent Status")
        .description("See your herdr agent status at a glance.")
        .supportedFamilies([.systemSmall])
    }
}
