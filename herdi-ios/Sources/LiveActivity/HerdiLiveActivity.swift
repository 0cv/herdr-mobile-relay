import ActivityKit
import SwiftUI
import WidgetKit

struct HerdiLiveActivity: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: HerdiWidgetAttributes.self) { context in
            HStack(spacing: 16) {
                Label("\(context.state.blockedCount)", systemImage: "circle.fill")
                    .foregroundStyle(.red)
                Label("\(context.state.workingCount)", systemImage: "circle.fill")
                    .foregroundStyle(.green)
                Label("\(context.state.idleCount)", systemImage: "circle.fill")
                    .foregroundStyle(.gray)
            }
            .padding()
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.center) {
                    HStack(spacing: 12) {
                        Label("\(context.state.blockedCount)", systemImage: "circle.fill")
                            .foregroundStyle(.red)
                        Label("\(context.state.workingCount)", systemImage: "circle.fill")
                            .foregroundStyle(.green)
                        Label("\(context.state.idleCount)", systemImage: "circle.fill")
                            .foregroundStyle(.gray)
                    }
                }
            } compactLeading: {
                HStack(spacing: 2) {
                    Image(systemName: "exclamationmark.circle.fill").foregroundStyle(.red)
                    Text("\(context.state.blockedCount)")
                }
            } compactTrailing: {
                HStack(spacing: 2) {
                    Image(systemName: "circle.fill").foregroundStyle(.green)
                    Text("\(context.state.workingCount)")
                }
            } minimal: {
                Image(systemName: "circle.fill")
                    .foregroundStyle(context.state.blockedCount > 0 ? .red : .green)
            }
        }
    }
}
