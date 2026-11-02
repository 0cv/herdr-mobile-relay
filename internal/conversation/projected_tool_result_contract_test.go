package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestProjectedToolResultApplicationContract(t *testing.T) {
	boundary := strings.Repeat("b", 131072)
	for _, initiallyTruncated := range []bool{false, true} {
		for _, output := range []struct {
			name, raw, want string
			truncated       bool
		}{
			{"empty", "", "", false},
			{"sanitized-short", " \x1b[31mshort\x1b[0m\x00 \r\n\t", "short", false},
			// Raw bytes exceed the limit; sanitized bytes land exactly on it.
			{"inclusive-boundary", " \x1b[31m\x00" + boundary + "\x1b[0m\x00 \n", boundary, false},
			{"utf8-overflow", " \x1b[31mx" + strings.Repeat("界", 43691) + "\x1b[0m\x00 ", "x" + strings.Repeat("界", 43690), true},
		} {
			for _, failed := range []bool{false, true} {
				t.Run(fmt.Sprintf("initially-truncated=%t/%s/failed=%t", initiallyTruncated, output.name, failed), func(t *testing.T) {
					name, wantName := "Read", "Read"
					if initiallyTruncated {
						name, wantName = strings.Repeat("界", 54), strings.Repeat("界", 53)
					}
					call := pmBlocks(t, 7, map[string]any{"type": "text", "text": "retained body"}, pmCall("match", name), pmCall("unrelated", "Other"))
					result := pmBlocks(t, 10000, pmResult("match", output.raw, failed))
					prepared := pmExpected(call, "assistant", "retained body", pmTool("match", wantName, "{}"), pmTool("unrelated", "Other", "{}"))
					prepared.Entry.Entry.Tools[0].Truncated = initiallyTruncated
					prepared.Entry.Entry.Truncated = initiallyTruncated
					if initiallyTruncated {
						prepared.Diagnostics.OmittedPayloads = 1
					}
					final := *prepared.Entry
					final.Entry.Tools = append([]ToolActivity(nil), prepared.Entry.Entry.Tools...)
					final.Entry.Tools[0].Output = output.want
					final.Entry.Tools[0].Error = failed
					final.Entry.Tools[0].Truncated = initiallyTruncated || output.truncated
					// Independent full-field expectations for memory, sequential index and
					// batched index; later output truncation does not mark the entry.
					pmSequence(t, "claude", []JSONLRecord{call, result}, []projectionResult{prepared, {}}, []projectedEntry{final})
				})
			}
		}
	}

	// Synthetic valid consumer state, NOT a claim that a native provider emitted
	// prior Output/Error on a still-pending call. Proves clearing, not OR/early return.
	for _, initiallyTruncated := range []bool{false, true} {
		t.Run(fmt.Sprintf("synthetic-consumer-state/clear-empty-success/truncated=%t", initiallyTruncated), func(t *testing.T) {
			call := pmBlocks(t, 9, pmCall("clear", "Read"))
			result := pmBlocks(t, 1000, pmResult("clear", "", false))
			memory, index := newMemoryProjector("claude", "revision"), pmIndex(t)
			prepared := pmExpected(call, "assistant", "", pmTool("clear", "Read", "{}"))
			pmCheck(t, memory.apply(call), prepared)
			got, err := index.applyRecord("claude", "revision", call)
			if err != nil {
				t.Fatal(err)
			}
			pmCheck(t, got, prepared)
			seed := *pmExpected(call, "assistant", "retained", ToolActivity{
				ID: "clear", Name: "Read", State: "running", ParentID: "parent",
				associationID: "clear", Input: "{\"kept\":true}", Output: "prior", Error: true, Truncated: initiallyTruncated,
			}).Entry
			seed.Entry.Kind, seed.Entry.Timestamp, seed.Entry.Truncated = "synthetic-kind", "synthetic-time", initiallyTruncated
			memory.entries[0] = seed
			memory.entries[0].Entry.Tools = append([]ToolActivity(nil), seed.Entry.Tools...)
			pmUpdate(t, index, func(tx *bolt.Tx) error {
				bucket := tx.Bucket(entriesBucket)
				var stored indexedEntry
				if err := json.Unmarshal(bucket.Get(entryOffsetKey(call.Start)), &stored); err != nil {
					return err
				}
				stored.Entry = pmWire(seed).Entry
				encoded, err := json.Marshal(stored)
				if err != nil {
					return err
				}
				return bucket.Put(entryOffsetKey(call.Start), encoded)
			})
			pmCheckEntry(t, memory.entries[0], seed)
			pmStored(t, index, []JSONLRecord{call}, []projectedEntry{seed})
			want := *pmExpected(call, "assistant", "retained", ToolActivity{
				ID: "clear", Name: "Read", State: "running", ParentID: "parent",
				associationID: "clear", Input: "{\"kept\":true}", Truncated: initiallyTruncated,
			}).Entry
			want.Entry.Kind, want.Entry.Timestamp, want.Entry.Truncated = "synthetic-kind", "synthetic-time", initiallyTruncated
			pmCheck(t, memory.apply(result), projectionResult{})
			pmCheckEntry(t, memory.entries[0], want)
			// Applying to memory alone must not mutate the serialized index state.
			pmStored(t, index, []JSONLRecord{call}, []projectedEntry{seed})
			got, err = index.applyRecord("claude", "revision", result)
			if err != nil {
				t.Fatal(err)
			}
			pmCheck(t, got, projectionResult{})
			pmStored(t, index, []JSONLRecord{call}, []projectedEntry{want})
			if len(memory.pending) != 0 || len(pmPending(t, index, "claude", "clear")) != 0 {
				t.Fatal("successful empty result did not consume the association")
			}
		})
	}
}
