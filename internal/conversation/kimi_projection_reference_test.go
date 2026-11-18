package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

func referenceProjectKimi(ctx context.Context, data []byte) kimiProjection {
	result := kimiProjection{entries: []Entry{}}
	if len(data) > maxConversationBytes {
		result.code = "output_limit"
		return result
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	result.complete = data[:end]
	if end != len(data) {
		result.diagnostics.ContinuationIncomplete = true
		result.diagnostics.ContinuationReason = "Kimi is persisting an incomplete journal record."
	}
	records := []kimiRecord{}
	mutation := sha256.New()
	for offset := 0; offset < end; {
		if ctx.Err() != nil {
			result.code = "request_cancelled"
			return result
		}
		next := bytes.IndexByte(data[offset:end], '\n') + offset
		raw := bytes.TrimSpace(data[offset:next])
		start := offset
		offset = next + 1
		if len(raw) == 0 {
			continue
		}
		if len(raw) > maxKimiRecord || len(records) >= maxKimiRecords {
			result.code = "output_limit"
			return result
		}
		if !utf8.Valid(raw) || !kimiJSONBounded(raw) || !json.Valid(raw) {
			result.code = "source_corrupt"
			return result
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			result.code = "source_corrupt"
			return result
		}
		object, _ := value.(map[string]any)
		if !kimiKnownRecord(object) {
			result.code = "unsupported_structure"
			return result
		}
		records = append(records, kimiRecord{object, raw, start})
		switch stringValue(object["type"]) {
		case "context.undo", "context.undone", "agent.switched", "context.clear", "context.apply_compaction":
			fmt.Fprintln(mutation, start, string(raw))
		}
	}
	result.mutation = fmt.Sprintf("%x", mutation.Sum(nil))
	if len(records) == 0 {
		result.code = "source_corrupt"
		return result
	}
	header := records[0].raw
	protocol := stringValue(header["protocol_version"])
	if protocol == "1.0" {
		result.code = "kimi_imported"
		return result
	}
	if stringValue(header["type"]) != "metadata" || protocol != "1.5" && protocol != "1.4" {
		result.code = "unsupported_structure"
		return result
	}
	chain, code := kimiChain(records)
	if code != "" {
		result.code = code
		return result
	}
	steps := make(map[string]int)
	closed := make(map[string]bool)
	pending := make(map[string]toolLocation)
	anchors := []int{}
	contextSeen, humanSeen := false, false
	add := func(record kimiRecord, role, text string) int {
		text, truncated := clampText(sanitizeText(text), maxEntryBytes)
		entry := Entry{ID: browserEntryID("kimi", int64(record.offset), record.bytes), Role: role, Text: text, Timestamp: kimiTimestamp(record.raw), Truncated: truncated}
		result.entries = append(result.entries, entry)
		return len(result.entries) - 1
	}
	call := func(entry int, id, name, input string) bool {
		if id == "" || len(id) > maxToolIDBytes || name == "" || len(pending) >= maxToolCount || len(result.entries[entry].Tools) >= maxToolCount {
			return false
		}
		if _, exists := pending[id]; exists {
			return false
		}
		name, truncated := clampText(sanitizeText(name), maxToolNameBytes)
		input, clipped := clampText(sanitizeText(input), maxEntryBytes)
		tool := ToolActivity{ID: browserEntryID(result.entries[entry].ID, int64(len(result.entries[entry].Tools)), []byte(id)), Name: name, Input: input, State: "running", Truncated: truncated || clipped}
		pending[id] = toolLocation{entry: entry, tool: len(result.entries[entry].Tools)}
		result.entries[entry].Tools = append(result.entries[entry].Tools, tool)
		return true
	}
	settle := func(id, output string, failed bool) {
		location, exists := pending[id]
		if !exists {
			result.diagnostics.ContinuationIncomplete = true
			return
		}
		tool := &result.entries[location.entry].Tools[location.tool]
		var clipped bool
		tool.Output, clipped = clampText(sanitizeText(output), maxEntryBytes)
		tool.Truncated = tool.Truncated || clipped
		tool.Error, tool.State = failed, "completed"
		delete(pending, id)
	}
	for _, index := range chain {
		record := records[index]
		raw := record.raw
		typeName := stringValue(raw["type"])
		if agent := stringValue(raw["agentId"]); agent != "" && agent != "main" {
			result.code = "unsupported_structure"
			break
		}
		switch typeName {
		case "":
			continue
		case "metadata":
			if index != 0 {
				result.code = "source_corrupt"
			}
		case "context.append_message":
			contextSeen = true
			message, ok := raw["message"].(map[string]any)
			if !ok {
				result.code = "source_corrupt"
				break
			}
			role := stringValue(message["role"])
			if role == "tool" {
				failed, _ := message["isError"].(bool)
				settle(stringValue(message["toolCallId"]), kimiText(message["content"]), failed)
				continue
			}
			if role != "user" && role != "assistant" {
				continue
			}
			visible, anchor := kimiHuman(message)
			if !visible {
				continue
			}
			if role == "user" && anchor {
				anchors = append(anchors, len(result.entries))
				clear(pending)
			}
			text := kimiText(message["content"])
			if role == "user" && text == "" {
				text = "[Non-text input omitted]"
			}
			entry := add(record, role, text)
			calls, _ := message["toolCalls"].([]any)
			for _, value := range calls {
				item, _ := value.(map[string]any)
				if role != "assistant" || stringValue(item["type"]) != "function" || !call(entry, stringValue(item["id"]), stringValue(item["name"]), stringValue(item["arguments"])) {
					result.code = "source_corrupt"
					break
				}
			}
		case "context.append_loop_event":
			contextSeen = true
			event, ok := raw["event"].(map[string]any)
			if !ok {
				result.code = "source_corrupt"
				break
			}
			switch stringValue(event["type"]) {
			case "step.begin":
				id := stringValue(event["uuid"])
				if _, exists := steps[id]; exists || id == "" || len(steps)-len(closed) >= maxToolCount {
					result.code = "source_corrupt"
					break
				}
				steps[id] = add(record, "assistant", "")
			case "content.part", "tool.call":
				id := stringValue(event["stepUuid"])
				entry, exists := steps[id]
				if !exists || closed[id] {
					result.diagnostics.ContinuationIncomplete = true
					continue
				}
				if stringValue(event["type"]) == "content.part" {
					text := kimiText([]any{event["part"]})
					if text != "" {
						body := result.entries[entry].Text
						if body != "" {
							body += "\n"
						}
						var clipped bool
						result.entries[entry].Text, clipped = clampText(sanitizeText(body+text), maxEntryBytes)
						result.entries[entry].Truncated = result.entries[entry].Truncated || clipped
					}
				} else {
					input := ""
					if args, exists := event["args"]; exists {
						encoded, _ := json.Marshal(args)
						input = string(encoded)
					}
					if !call(entry, stringValue(event["toolCallId"]), stringValue(event["name"]), input) {
						result.code = "source_corrupt"
					}
				}
			case "tool.result":
				value, ok := event["result"].(map[string]any)
				if !ok {
					result.code = "source_corrupt"
					break
				}
				failed, _ := value["isError"].(bool)
				settle(stringValue(event["toolCallId"]), kimiText(value["output"]), failed)
			case "step.end":
				id := stringValue(event["uuid"])
				if _, exists := steps[id]; !exists || closed[id] {
					result.code = "source_corrupt"
					break
				}
				closed[id] = true
			default:
				result.code = "unsupported_structure"
			}
		case "context.undo":
			count, valid := kimiInteger(raw["count"])
			if !valid || count == 0 || count > len(anchors) {
				result.code = "source_corrupt"
				break
			}
			result.entries = result.entries[:anchors[len(anchors)-count]]
			anchors = anchors[:len(anchors)-count]
			clear(steps)
			clear(closed)
			clear(pending)
		case "context.clear", "context.apply_compaction":
			kept := 0
			if typeName == "context.apply_compaction" {
				if value, exists := raw["keptUserMessageCount"]; exists {
					var valid bool
					kept, valid = kimiInteger(value)
					if !valid || kept > len(anchors) {
						result.code = "source_corrupt"
						break
					}
				}
			}
			anchors = anchors[len(anchors)-kept:]
			clear(steps)
			clear(closed)
			clear(pending)
		case "context.undone", "agent.switched":
			result.code = "source_corrupt"
		case "agent.message.appended", "agent.turn.started", "agent.turn.ended", "human.message.appended", "human.turn.started", "human.turn.ended":
			humanSeen = true
		case "turn.prompt", "turn.steer", "turn.ended", "forked":
		default:
			result.code = "unsupported_structure"
		}
		if result.code != "" {
			break
		}
	}
	if !contextSeen && humanSeen {
		result.code = "kimi_human_engine"
	}
	if result.code != "" {
		result.entries = []Entry{}
		return result
	}
	if len(steps) != len(closed) || len(pending) > 0 {
		result.diagnostics.ContinuationIncomplete = true
	}
	if result.diagnostics.ContinuationIncomplete && result.diagnostics.ContinuationReason == "" {
		result.diagnostics.ContinuationReason = "Some Kimi activity is incomplete or could not be associated with a recorded step."
	}
	visible := result.entries[:0]
	for _, entry := range result.entries {
		if entry.Text != "" || len(entry.Tools) > 0 {
			visible = append(visible, entry)
		}
	}
	result.entries = visible
	return result
}
