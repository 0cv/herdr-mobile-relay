package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type kimiProjection struct {
	entries     []Entry
	code        string
	diagnostics BrowseDiagnostics
	mutation    string
	complete    []byte
}

type kimiRecord struct {
	raw    map[string]any
	bytes  []byte
	offset int
}

func kimiJSONBounded(raw []byte) bool {
	depth, quoted, escaped := 0, false, false
	for _, c := range raw {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > 32 {
				return false
			}
		case '}', ']':
			depth--
		}
	}
	return true
}

func kimiInteger(value any) (int, bool) {
	n, ok := value.(float64)
	return int(n), ok && n >= 0 && n <= maxKimiRecords && n == float64(int(n))
}

func kimiSwitchTimes(records ...map[string]any) bool {
	var expected float64
	for i, record := range records {
		stamp, exists := record["time"]
		if !exists {
			if i > 0 && expected != 0 {
				return false
			}
			continue
		}
		n, ok := stamp.(float64)
		if !ok || n <= 0 || n > 253402300799999 || n != float64(int64(n)) {
			return false
		}
		if i == 0 {
			expected = n
		} else if n != expected {
			return false
		}
	}
	return true
}

func kimiKnownRecord(raw map[string]any) bool {
	switch stringValue(raw["type"]) {
	case "", "metadata", "context.append_message", "context.undo", "context.undone", "context.clear", "context.apply_compaction", "agent.switched", "agent.message.appended", "agent.turn.started", "agent.turn.ended", "human.message.appended", "human.turn.started", "human.turn.ended", "turn.prompt", "turn.steer", "turn.ended", "forked":
		return true
	case "context.append_loop_event":
		event, _ := raw["event"].(map[string]any)
		switch stringValue(event["type"]) {
		case "step.begin", "content.part", "tool.call", "tool.result", "step.end":
			return true
		}
	}
	return false
}

func kimiChain(records []kimiRecord) ([]int, string) {
	type branch struct {
		base  string
		cut   int
		start int
		end   int
	}
	branches := map[string]branch{"main": {start: 1, end: len(records)}}
	active := "main"
	activeLines := []int{}
	for i := 0; i < len(records); i++ {
		r := records[i].raw
		if stringValue(r["type"]) != "agent.switched" {
			activeLines = append(activeLines, i)
			continue
		}
		if len(branches) >= 128 || i+2 >= len(records) {
			return nil, "source_corrupt"
		}
		name := stringValue(r["branch"])
		base, _ := r["base"].(map[string]any)
		baseName := stringValue(base["branch"])
		prior, exists := branches[baseName]
		cut, cutOK := kimiInteger(base["line"])
		legacy, legacyOK := kimiInteger(r["legacyUndoLine"])
		turns, turnsOK := kimiInteger(r["turns"])
		_, duplicate := branches[name]
		undo, undone := records[i+1].raw, records[i+2].raw
		count, countOK := kimiInteger(undo["count"])
		undoneTurns, undoneOK := kimiInteger(undone["turns"])
		_, fromOK := kimiInteger(undone["fromTurnId"])
		if name == "" || len(name) > 128 || duplicate || !exists || !cutOK || cut < prior.start || cut > min(prior.end, i) || !legacyOK || legacy != i+2 || !turnsOK || turns == 0 || !countOK || count != turns || !undoneOK || undoneTurns != turns || !fromOK || stringValue(r["reason"]) != "undo" || stringValue(r["agentId"]) != "main" || stringValue(undo["type"]) != "context.undo" || stringValue(undone["type"]) != "context.undone" || stringValue(undo["agentId"]) != "main" || stringValue(undone["agentId"]) != "main" || !kimiSwitchTimes(r, undo, undone) {
			return nil, "source_corrupt"
		}
		ancestor, ceiling, onChain := active, i, false
		for depth := 0; depth < 128; depth++ {
			if ancestor == baseName {
				onChain = cut <= ceiling
				break
			}
			parent := branches[ancestor]
			if parent.base == "" {
				break
			}
			ancestor, ceiling = parent.base, parent.cut
		}
		anchors, valid := kimiAnchorLines(records, activeLines)
		removed := 0
		for _, line := range anchors {
			if line+1 > cut {
				removed++
			}
		}
		if !onChain || !valid || removed != turns {
			return nil, "source_corrupt"
		}
		kept := activeLines[:0]
		for _, line := range activeLines {
			if line+1 <= cut {
				kept = append(kept, line)
			}
		}
		activeLines = kept
		old := branches[active]
		old.end = i
		branches[active] = old
		branches[name] = branch{base: baseName, cut: cut, start: i + 3, end: len(records)}
		active = name
		i += 2
	}
	chain := []int{}
	var walk func(string, int, int) bool
	walk = func(name string, end, depth int) bool {
		if depth > 128 {
			return false
		}
		b, ok := branches[name]
		if !ok || end > b.end || end < b.start {
			return false
		}
		if b.base != "" && !walk(b.base, b.cut, depth+1) {
			return false
		}
		for line := b.start; line <= end; line++ {
			typeName := stringValue(records[line-1].raw["type"])
			if typeName != "agent.switched" && !(line == b.start && name != "main") {
				chain = append(chain, line-1)
			}
		}
		return true
	}
	if !walk(active, len(records), 0) {
		return nil, "source_corrupt"
	}
	return chain, ""
}

func kimiAnchorLines(records []kimiRecord, lines []int) ([]int, bool) {
	anchors := []int{}
	for _, line := range lines {
		raw := records[line].raw
		switch stringValue(raw["type"]) {
		case "context.append_message":
			message, _ := raw["message"].(map[string]any)
			visible, anchor := kimiHuman(message)
			if stringValue(message["role"]) == "user" && visible && anchor {
				anchors = append(anchors, line)
			}
		case "context.undo":
			count, ok := kimiInteger(raw["count"])
			if !ok || count == 0 || count > len(anchors) {
				return nil, false
			}
			anchors = anchors[:len(anchors)-count]
		case "context.clear":
			anchors = nil
		case "context.apply_compaction":
			kept := 0
			if value, exists := raw["keptUserMessageCount"]; exists {
				var ok bool
				kept, ok = kimiInteger(value)
				if !ok || kept > len(anchors) {
					return nil, false
				}
			}
			anchors = anchors[len(anchors)-kept:]
		}
	}
	return anchors, true
}

func kimiText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	parts, _ := value.([]any)
	texts := []string{}
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		meta, _ := part["meta"].(map[string]any)
		if stringValue(part["type"]) == "text" && stringValue(meta["source"]) != "user prompt submit hook" {
			texts = append(texts, stringValue(part["text"]))
		}
	}
	return strings.Join(texts, "\n")
}

func kimiHuman(message map[string]any) (bool, bool) {
	origin, exists := message["origin"]
	if !exists || origin == nil {
		return true, true
	}
	object, ok := origin.(map[string]any)
	if !ok || stringValue(object["kind"]) != "user" {
		return false, false
	}
	inTurn, _ := object["inTurn"].(bool)
	return true, !inTurn
}

func kimiTimestamp(raw map[string]any) string {
	stamp, ok := raw["time"].(float64)
	if !ok || stamp <= 0 || stamp > 253402300799999 {
		return ""
	}
	return time.UnixMilli(int64(stamp)).UTC().Format(time.RFC3339Nano)
}

func validateKimi(ctx context.Context, data []byte) kimiPageProjection {
	plan := kimiPageProjection{metadata: kimiProjection{entries: []Entry{}}}
	result := &plan.metadata
	if len(data) > maxConversationBytes {
		result.code = "output_limit"
		return plan
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
			return plan
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
			return plan
		}
		if !utf8.Valid(raw) || !kimiJSONBounded(raw) || !json.Valid(raw) {
			result.code = "source_corrupt"
			return plan
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			result.code = "source_corrupt"
			return plan
		}
		object, _ := value.(map[string]any)
		if !kimiKnownRecord(object) {
			result.code = "unsupported_structure"
			return plan
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
		return plan
	}
	header := records[0].raw
	protocol := stringValue(header["protocol_version"])
	if protocol == "1.0" {
		result.code = "kimi_imported"
		return plan
	}
	if stringValue(header["type"]) != "metadata" || protocol != "1.5" && protocol != "1.4" {
		result.code = "unsupported_structure"
		return plan
	}
	chain, code := kimiChain(records)
	if code != "" {
		result.code = code
		return plan
	}
	steps := make(map[string]int)
	closed := make(map[string]bool)
	pending := make(map[string]toolLocation)
	anchors := []int{}
	contextSeen, humanSeen := false, false
	shapes := make(map[int][]string)
	add := func(index int, role string, visible, step bool) int {
		plan.entries = append(plan.entries, kimiEntryPlan{record: index, role: role, visible: visible, step: step})
		return len(plan.entries) - 1
	}
	call := func(entry, record, callIndex int, id, name string) bool {
		if id == "" || len(id) > maxToolIDBytes || name == "" || len(pending) >= maxToolCount || len(plan.entries[entry].tools) >= maxToolCount {
			return false
		}
		if _, exists := pending[id]; exists {
			return false
		}
		pending[id] = toolLocation{entry: entry, tool: len(plan.entries[entry].tools)}
		plan.entries[entry].tools = append(plan.entries[entry].tools, kimiToolPlan{callRecord: record, callIndex: callIndex, result: -1})
		return true
	}
	settle := func(id string, index int, failed bool) {
		location, exists := pending[id]
		if !exists {
			result.diagnostics.ContinuationIncomplete = true
			return
		}
		tool := &plan.entries[location.entry].tools[location.tool]
		tool.result, tool.failed = index, failed
		delete(pending, id)
	}
	for _, index := range chain {
		if ctx.Err() != nil {
			result.code = "request_cancelled"
			break
		}
		raw := records[index].raw
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
				settle(stringValue(message["toolCallId"]), index, failed)
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
				anchors = append(anchors, len(plan.entries))
				clear(pending)
			}
			parts := kimiTextParts(message["content"])
			visibleText := len(kimiTextShape(parts)) > 0 || role == "user" && kimiPartsBytes(parts) == 0
			entry := add(index, role, visibleText, false)
			calls, _ := message["toolCalls"].([]any)
			for callIndex, value := range calls {
				item, _ := value.(map[string]any)
				if role != "assistant" || stringValue(item["type"]) != "function" || !call(entry, index, callIndex, stringValue(item["id"]), stringValue(item["name"])) {
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
				steps[id] = add(index, "assistant", false, true)
			case "content.part", "tool.call":
				id := stringValue(event["stepUuid"])
				entry, exists := steps[id]
				if !exists || closed[id] {
					result.diagnostics.ContinuationIncomplete = true
					continue
				}
				if stringValue(event["type"]) == "content.part" {
					parts := kimiTextParts([]any{event["part"]})
					if kimiPartsBytes(parts) > 0 {
						body := shapes[entry]
						if len(body) > 0 {
							body = append(body, "\n")
						}
						shapes[entry] = kimiTextShape(append(body, parts...))
						plan.entries[entry].visible = len(shapes[entry]) > 0
						plan.entries[entry].textRecords = append(plan.entries[entry].textRecords, index)
					}
				} else if !call(entry, index, -1, stringValue(event["toolCallId"]), stringValue(event["name"])) {
					result.code = "source_corrupt"
				}
			case "tool.result":
				value, ok := event["result"].(map[string]any)
				if !ok {
					result.code = "source_corrupt"
					break
				}
				failed, _ := value["isError"].(bool)
				settle(stringValue(event["toolCallId"]), index, failed)
			case "step.end":
				id := stringValue(event["uuid"])
				if _, exists := steps[id]; !exists || closed[id] {
					result.code = "source_corrupt"
					break
				}
				closed[id] = true
				delete(shapes, steps[id])
			default:
				result.code = "unsupported_structure"
			}
		case "context.undo":
			count, valid := kimiInteger(raw["count"])
			if !valid || count == 0 || count > len(anchors) {
				result.code = "source_corrupt"
				break
			}
			plan.entries = plan.entries[:anchors[len(anchors)-count]]
			anchors = anchors[:len(anchors)-count]
			clear(shapes)
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
			clear(shapes)
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
		plan.entries = nil
		return plan
	}
	if len(steps) != len(closed) || len(pending) > 0 {
		result.diagnostics.ContinuationIncomplete = true
	}
	if result.diagnostics.ContinuationIncomplete && result.diagnostics.ContinuationReason == "" {
		result.diagnostics.ContinuationReason = "Some Kimi activity is incomplete or could not be associated with a recorded step."
	}
	visible := make([]kimiEntryPlan, 0, len(plan.entries))
	for _, entry := range plan.entries {
		if entry.visible || len(entry.tools) > 0 {
			record := records[entry.record]
			entry.id = browserEntryID("kimi", int64(record.offset), record.bytes)
			visible = append(visible, entry)
		}
	}
	plan.entries = visible
	for i := range records {
		records[i].raw = nil
	}
	plan.records = records
	return plan
}
