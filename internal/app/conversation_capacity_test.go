package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func TestConversationCapacityLoggerFields(t *testing.T) {
	var output bytes.Buffer
	observe := conversationCapacityLogger(slog.New(slog.NewJSONHandler(&output, nil)))
	observe(conversation.CapacityEvent{Stage: "batch", Kind: "index_capacity_exceeded", Quota: "aggregate", AggregateBytes: 123, SnapshotBytes: 45, AggregateQuota: 100, SnapshotQuota: 50, Files: 3, Protected: 2, Skipped: 5, Evicted: 1, ReclaimedBytes: 25, CleanupFailures: 1, Suppressed: 4})
	var fields map[string]any
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"level": "WARN", "msg": "conversation history cache capacity denied", "stage": "batch", "kind": "index_capacity_exceeded", "quota": "aggregate", "aggregate_bytes": float64(123), "snapshot_bytes": float64(45), "aggregate_quota": float64(100), "snapshot_quota": float64(50), "files": float64(3), "eligible": float64(0), "protected": float64(2), "evicted": float64(1), "reclaimed_bytes": float64(25), "cleanup_failures": float64(1), "skipped": float64(5), "suppressed": float64(4)}
	delete(fields, "time")
	if len(fields) != len(expected) {
		t.Fatalf("unexpected log fields: %v", fields)
	}
	for key, want := range expected {
		if fields[key] != want {
			t.Errorf("%s=%v want %v", key, fields[key], want)
		}
	}
}
