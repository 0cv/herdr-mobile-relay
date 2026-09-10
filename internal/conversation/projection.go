package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type projectedEntry struct {
	Offset int64
	Entry  Entry
}

type projectionDiagnostics struct {
	OversizedRecords int
	CorruptRecords   int
	OmittedTools     int
	OmittedPayloads  int
	PlanCorrupt      bool
}

type projectionResult struct {
	Entry       *projectedEntry
	Diagnostics projectionDiagnostics
	Plan        *OMOTodoState
	TodoSeen    bool
	TodoInvalid bool
}

type memoryProjector struct {
	agent     string
	revision  string
	entries   []projectedEntry
	byOffset  map[int64]int
	pending   map[string]toolLocation
	plan      *OMOTodoState
	todoSeen  bool
	todoValid bool
}

func newMemoryProjector(agent, revision string) *memoryProjector {
	return &memoryProjector{
		agent: agent, revision: revision, entries: make([]projectedEntry, 0),
		byOffset: make(map[int64]int), pending: make(map[string]toolLocation),
	}
}

func (p *memoryProjector) apply(record JSONLRecord) projectionResult {
	result := projectionResult{}
	if record.Oversized {
		result.Diagnostics.OversizedRecords = 1
		return result
	}
	if !record.Complete {
		return result
	}
	var raw map[string]any
	if err := json.Unmarshal(record.Raw, &raw); err != nil {
		result.Diagnostics.CorruptRecords = 1
		return result
	}
	result.Plan, result.TodoSeen, result.TodoInvalid = observeOMORecord(raw, p.agent)
	if result.Plan != nil {
		p.plan = result.Plan
		p.todoValid = true
	}
	p.todoSeen = p.todoSeen || result.TodoSeen
	if result.TodoInvalid {
		result.Diagnostics.PlanCorrupt = true
	}

	calls, results := parseToolActivity(normalizedAgent(p.agent), raw)
	toolEntry := Entry{Tools: calls}
	omittedTools, omittedPayloads := normalizeEntryTools(&toolEntry)
	calls = toolEntry.Tools
	result.Diagnostics.OmittedTools += omittedTools
	result.Diagnostics.OmittedPayloads += omittedPayloads
	for _, toolResult := range results {
		location, ok := p.pending[toolAssociationKey(p.agent, toolResult.id)]
		if !ok || location.entry < 0 || location.entry >= len(p.entries) {
			continue
		}
		if location.tool < 0 || location.tool >= len(p.entries[location.entry].Entry.Tools) {
			continue
		}
		tool := &p.entries[location.entry].Entry.Tools[location.tool]
		output, truncated := clampText(sanitizeText(toolResult.output), maxEntryBytes)
		tool.Output = output
		tool.Error = toolResult.failed
		tool.Truncated = tool.Truncated || truncated
		delete(p.pending, toolAssociationKey(p.agent, toolResult.id))
	}

	role, timestamp, body := visibleRecord(p.agent, raw)
	body = sanitizeText(body)
	if role == "" && len(calls) > 0 {
		role = "assistant"
	}
	if role == "" || (body == "" && len(calls) == 0) {
		return result
	}
	body, truncated := clampText(body, maxEntryBytes)
	entry := projectedEntry{
		Offset: record.Start,
		Entry: Entry{
			ID: browserEntryID(p.revision, record.Start, record.Raw), Timestamp: timestamp,
			Role: role, Text: body, Tools: calls, Truncated: truncated || omittedTools > 0 || omittedPayloads > 0,
		},
	}
	p.byOffset[record.Start] = len(p.entries)
	p.entries = append(p.entries, entry)
	for toolIndex := range calls {
		if id := toolAssociationID(calls[toolIndex]); id != "" {
			p.pending[toolAssociationKey(p.agent, id)] = toolLocation{entry: len(p.entries) - 1, tool: toolIndex}
		}
	}
	result.Entry = &entry
	return result
}

func visibleRecord(agent string, record map[string]any) (string, string, string) {
	normalized := normalizedAgent(agent)
	role, timestamp, body := "", stringValue(record["timestamp"]), ""
	switch normalized {
	case "claude", "claudecode", "qoder", "qodercli":
		role, body = parseClaudeRecord(record)
	case "codex", "openaicodex":
		role, body = parseCodexRecord(record)
	case "pi", "picodingagent", "omp", "ohmypi", "omo", "ohmyopencode":
		role, body = parsePiRecord(record)
	}
	return role, timestamp, body
}

func observeOMORecord(record map[string]any, agent string) (*OMOTodoState, bool, bool) {
	if normalizedAgent(agent) != "omo" && normalizedAgent(agent) != "ohmyopencode" {
		return nil, false, false
	}
	if stringValue(record["type"]) != "custom" || stringValue(record["customType"]) != "senpi.todo-state" {
		return nil, false, false
	}
	data, ok := record["data"]
	if !ok {
		return nil, true, true
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, true, true
	}
	state, valid := decodeOMOTodo(encoded, "", stringValue(record["timestamp"]))
	if !valid {
		return nil, true, true
	}
	return &state, true, false
}

func browserEntryID(revision string, offset int64, raw []byte) string {
	hash := sha256.New()
	hash.Write([]byte(revision))
	hash.Write([]byte{0})
	var offsetBytes [8]byte
	for index := range offsetBytes {
		offsetBytes[7-index] = byte(offset >> (index * 8))
	}
	hash.Write(offsetBytes[:])
	hash.Write([]byte{0})
	recordDigest := sha256.Sum256(raw)
	hash.Write(recordDigest[:])
	return hex.EncodeToString(hash.Sum(nil)[:16])
}
