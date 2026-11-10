package app

import (
	"log/slog"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func conversationCapacityLogger(logger *slog.Logger) func(conversation.CapacityEvent) {
	return func(event conversation.CapacityEvent) {
		logger.Warn("conversation history cache capacity denied",
			"stage", event.Stage, "kind", event.Kind, "quota", event.Quota,
			"aggregate_bytes", event.AggregateBytes, "snapshot_bytes", event.SnapshotBytes,
			"aggregate_quota", event.AggregateQuota, "snapshot_quota", event.SnapshotQuota,
			"files", event.Files, "eligible", event.Eligible, "protected", event.Protected, "skipped", event.Skipped,
			"evicted", event.Evicted, "reclaimed_bytes", event.ReclaimedBytes,
			"cleanup_failures", event.CleanupFailures,
			"suppressed", event.Suppressed)
	}
}
