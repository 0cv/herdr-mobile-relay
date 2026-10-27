package conversation

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"

	panehistory "github.com/0cv/herdr-mobile-relay/internal/history"
)

type kimiToolPlan struct {
	callRecord int
	callIndex  int
	result     int
	failed     bool
}

type kimiEntryPlan struct {
	id          string
	record      int
	role        string
	step        bool
	visible     bool
	textRecords []int
	tools       []kimiToolPlan
}

type kimiPageProjection struct {
	metadata kimiProjection
	records  []kimiRecord
	entries  []kimiEntryPlan
}

type kimiMaterializationWork struct {
	Records      int
	RecordBytes  int
	PayloadBytes int
	Entries      int
}

func kimiTextParts(value any) []string {
	if text, ok := value.(string); ok {
		return []string{text}
	}
	parts, _ := value.([]any)
	var texts []string
	for _, value := range parts {
		part, _ := value.(map[string]any)
		meta, _ := part["meta"].(map[string]any)
		if stringValue(part["type"]) != "text" || stringValue(meta["source"]) == "user prompt submit hook" {
			continue
		}
		if len(texts) > 0 {
			texts = append(texts, "\n")
		}
		texts = append(texts, stringValue(part["text"]))
	}
	return texts
}

func kimiPartsBytes(parts []string) int {
	n := 0
	for _, part := range parts {
		n += len(part)
	}
	return n
}

type kimiPartsReader struct {
	parts []string
	part  int
	byte  int
}

func (r *kimiPartsReader) advance(n int) {
	for r.part < len(r.parts) {
		left := len(r.parts[r.part]) - r.byte
		if n < left {
			r.byte += n
			return
		}
		n -= left
		r.part++
		r.byte = 0
	}
}

func (r *kimiPartsReader) ReadRune() (rune, int, error) {
	r.advance(0)
	if r.part >= len(r.parts) {
		return 0, 0, io.EOF
	}
	value, size := utf8.DecodeRuneInString(r.parts[r.part][r.byte:])
	r.advance(size)
	return value, size, nil
}

func kimiTrimParts(parts []string) []string {
	for len(parts) > 0 {
		parts[0] = strings.TrimLeftFunc(parts[0], unicode.IsSpace)
		if parts[0] != "" {
			break
		}
		parts = parts[1:]
	}
	for len(parts) > 0 {
		last := len(parts) - 1
		parts[last] = strings.TrimRightFunc(parts[last], unicode.IsSpace)
		if parts[last] != "" {
			break
		}
		parts = parts[:last]
	}
	return parts
}

func kimiTextShape(parts []string) []string {
	var out []string
	r := kimiPartsReader{parts: parts}
	remainingBytes := kimiPartsBytes(parts)
	appendPrefix := func(n int) {
		for n > 0 {
			r.advance(0)
			text := parts[r.part][r.byte:]
			length := min(n, len(text))
			text = text[:length]
			for len(text) > 0 {
				cut := strings.IndexAny(text, "\r\x00")
				if cut < 0 {
					out = append(out, text)
					break
				}
				if cut > 0 {
					out = append(out, text[:cut])
				}
				text = text[cut+1:]
			}
			r.advance(length)
			n -= length
			remainingBytes -= length
		}
	}
	hasEscape := false
	for _, part := range parts {
		if strings.ContainsRune(part, '\x1b') {
			hasEscape = true
			break
		}
	}
	for remainingBytes > 0 {
		start, end := -1, -1
		if hasEscape {
			probe := r
			start, end = panehistory.ANSISequenceRange(&probe)
		}
		if start < 0 {
			appendPrefix(remainingBytes)
			break
		}
		appendPrefix(start)
		r.advance(end - start)
		remainingBytes -= end - start
	}
	out = kimiTrimParts(out)
	remaining := maxEntryBytes
	for i, text := range out {
		if len(text) <= remaining {
			remaining -= len(text)
			continue
		}
		clipped := text[:remaining]
		for !utf8.ValidString(clipped) {
			clipped = clipped[:len(clipped)-1]
		}
		out[i] = clipped
		return kimiTrimParts(out[:i+1])
	}
	return out
}

func (p kimiPageProjection) retainedBytes() int64 {
	n := int64(unsafe.Sizeof(p) + uintptr(cap(p.metadata.complete)) + uintptr(cap(p.records))*unsafe.Sizeof(kimiRecord{}) + uintptr(cap(p.entries))*unsafe.Sizeof(kimiEntryPlan{}))
	n += int64(len(p.metadata.mutation) + len(p.metadata.code) + len(p.metadata.diagnostics.ContinuationReason))
	for _, entry := range p.entries {
		n += int64(len(entry.id) + len(entry.role))
		n += int64(uintptr(cap(entry.textRecords))*unsafe.Sizeof(int(0)) + uintptr(cap(entry.tools))*unsafe.Sizeof(kimiToolPlan{}))
	}
	return n
}

func (p kimiPageProjection) materialize(ctx context.Context, start, end int) ([]Entry, kimiMaterializationWork, string) {
	work := kimiMaterializationWork{}
	decoded := make(map[int]map[string]any)
	load := func(index int) map[string]any {
		if value, ok := decoded[index]; ok {
			return value
		}
		var value any
		_ = json.Unmarshal(p.records[index].bytes, &value)
		object, _ := value.(map[string]any)
		decoded[index] = object
		work.Records++
		work.RecordBytes += len(p.records[index].bytes)
		return object
	}
	clamp := func(text string, limit int) (string, bool) {
		work.PayloadBytes += len(text)
		return clampText(sanitizeText(text), limit)
	}
	entries := make([]Entry, 0, end-start)
	for _, plan := range p.entries[start:end] {
		if ctx.Err() != nil {
			return nil, work, "request_cancelled"
		}
		raw := load(plan.record)
		entry := Entry{ID: plan.id, Role: plan.role, Timestamp: kimiTimestamp(raw)}
		if !plan.step {
			message, _ := raw["message"].(map[string]any)
			text := kimiText(message["content"])
			if entry.Role == "user" && text == "" {
				text = "[Non-text input omitted]"
			}
			entry.Text, entry.Truncated = clamp(text, maxEntryBytes)
		}
		for _, index := range plan.textRecords {
			if ctx.Err() != nil {
				return nil, work, "request_cancelled"
			}
			event, _ := load(index)["event"].(map[string]any)
			text := kimiText([]any{event["part"]})
			body := entry.Text
			if body != "" {
				body += "\n"
			}
			var clipped bool
			entry.Text, clipped = clamp(body+text, maxEntryBytes)
			entry.Truncated = entry.Truncated || clipped
		}
		for _, tool := range plan.tools {
			if ctx.Err() != nil {
				return nil, work, "request_cancelled"
			}
			callRecord := load(tool.callRecord)
			var item map[string]any
			input := ""
			id := ""
			if tool.callIndex >= 0 {
				message, _ := callRecord["message"].(map[string]any)
				calls, _ := message["toolCalls"].([]any)
				item, _ = calls[tool.callIndex].(map[string]any)
				input, id = stringValue(item["arguments"]), stringValue(item["id"])
			} else {
				item, _ = callRecord["event"].(map[string]any)
				if args, exists := item["args"]; exists {
					encoded, _ := json.Marshal(args)
					input = string(encoded)
				}
				id = stringValue(item["toolCallId"])
			}
			name, truncated := clamp(stringValue(item["name"]), maxToolNameBytes)
			input, clipped := clamp(input, maxEntryBytes)
			activity := ToolActivity{ID: browserEntryID(entry.ID, int64(len(entry.Tools)), []byte(id)), Name: name, Input: input, Truncated: truncated || clipped, State: "running"}
			if tool.result >= 0 {
				raw := load(tool.result)
				value, _ := raw["message"].(map[string]any)
				output := value["content"]
				if stringValue(raw["type"]) == "context.append_loop_event" {
					event, _ := raw["event"].(map[string]any)
					value, _ = event["result"].(map[string]any)
					output = value["output"]
				}
				activity.Output, clipped = clamp(kimiText(output), maxEntryBytes)
				activity.Truncated = activity.Truncated || clipped
				activity.Error, activity.State = tool.failed, "completed"
			}
			entry.Tools = append(entry.Tools, activity)
		}
		entries = append(entries, entry)
		work.Entries++
	}
	return entries, work, ""
}

func projectKimi(ctx context.Context, data []byte) kimiProjection {
	plan := validateKimi(ctx, data)
	result := plan.metadata
	if result.code != "" {
		return result
	}
	result.entries, _, result.code = plan.materialize(ctx, 0, len(plan.entries))
	return result
}
