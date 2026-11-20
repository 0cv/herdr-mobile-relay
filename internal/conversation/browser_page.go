package conversation

import (
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"
)

func (b *Browser) memoryPage(
	request BrowseRequest,
	source fileSource,
	projection recentProjection,
	captured capturedRange,
	initial bool,
) BrowsePage {
	scope := request.Scope
	eligible := make([]projectedEntry, 0, len(projection.Entries))
	for _, entry := range projection.Entries {
		if entry.Offset < captured.boundary {
			eligible = append(eligible, entry)
		}
	}
	start := len(eligible) - request.Limit
	if start < 0 {
		start = 0
	}
	selected := append([]projectedEntry(nil), eligible[start:]...)
	potentialOlder := start > 0 || captured.start > 0
	base := request.withObservation(BrowsePage{
		Available: true, State: BrowseReady, Mode: BrowseRecent,
		SourceRevision: source.revision, Entries: []Entry{}, Diagnostics: projection.Diagnostics, OMOPlan: boundTodoPlan(projection.Plan, browseResponseBytes/4),
	})
	if captured.start == 0 {
		total := len(projection.Entries)
		base.Total = &total
	} else {
		base.Reason = "Showing recent messages. Older history can be loaded from this computer."
	}
	if !potentialOlder && len(selected) == 0 && initial && captured.start > 0 {
		potentialOlder = true
	}

	var next func(int64) (string, error)
	if potentialOlder {
		next = func(nextBoundary int64) (string, error) {
			if start > 0 {
				return b.rangeCursor("recent", scope, source, captured.withBoundary(nextBoundary))
			}
			prepareBoundary := captured.start
			if initial && len(projection.Entries) == 0 {
				prepareBoundary = captured.end
			}
			return b.rangeCursor("prepare", scope, source, captured.withBoundary(prepareBoundary))
		}
	}
	fitted, omitted := b.fitProjected(selected, func(candidate []projectedEntry, omitted int) (BrowsePage, error) {
		if omitted == 0 {
			return cursorCandidatePage(base, next)(candidate, omitted)
		}
		return cursorCandidatePage(base, func(boundary int64) (string, error) {
			return b.rangeCursor("recent", scope, source, captured.withBoundary(boundary))
		})(candidate, omitted)
	})
	if omitted > 0 {
		base.HasMore = true
		cursor, err := b.rangeCursor("recent", scope, source, captured.withBoundary(fitted[0].Offset))
		if err != nil {
			return browseContinuationFailed()
		}
		base.NextCursor = cursor
	} else if potentialOlder && len(fitted) > 0 {
		base.HasMore = true
		cursor, err := next(fitted[0].Offset)
		if err != nil {
			return browseContinuationFailed()
		}
		base.NextCursor = cursor
	} else if potentialOlder {
		base.HasMore = true
		cursor, err := next(captured.boundary)
		if err != nil {
			return browseContinuationFailed()
		}
		base.NextCursor = cursor
	}
	return b.finishFittedPage(base, selected, fitted)
}

func projectedEntries(entries []projectedEntry) []Entry {
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Entry)
	}
	return result
}

func browseOmissions(original, fitted []projectedEntry) (int, int) {
	byID := make(map[string]Entry, len(original))
	for _, entry := range original {
		byID[entry.Entry.ID] = entry.Entry
	}
	tools, payloads := 0, 0
	for _, entry := range fitted {
		before, ok := byID[entry.Entry.ID]
		if !ok {
			continue
		}
		if len(before.Tools) > len(entry.Entry.Tools) {
			tools += len(before.Tools) - len(entry.Entry.Tools)
		}
		if before.Text != entry.Entry.Text || before.Truncated != entry.Entry.Truncated || len(before.Tools) != len(entry.Entry.Tools) {
			payloads++
		}
	}
	return tools, payloads
}

func (page BrowsePage) FitsReply() bool {
	return pageSize(page) <= browseResponseBytes
}

// BoundActivityPayloads shortens an oversized bridge page without dropping entries or changing its cursor.
func (page BrowsePage) BoundActivityPayloads() BrowsePage {
	page.Entries = slices.Clone(page.Entries)
	page.Provisional = slices.Clone(page.Provisional)
	type payload struct {
		entry *Entry
		bytes int
	}
	var payloads []payload
	for _, entries := range [][]Entry{page.Provisional, page.Entries} {
		for i := range entries {
			data, _ := json.Marshal(entries[i])
			payloads = append(payloads, payload{entry: &entries[i], bytes: len(data)})
		}
	}
	slices.SortStableFunc(payloads, func(a, b payload) int { return cmp.Compare(b.bytes, a.bytes) })
	for _, payload := range payloads {
		size := pageSize(page)
		if size <= browseResponseBytes {
			break
		}
		entry := boundBrowseEntry(*payload.entry, browseResponseBytes-size+payload.bytes-64)
		if len(entry.Tools) == 0 && entry.Text == "" {
			entry.Text = "Message content omitted by relay limits."
		}
		page.Diagnostics.OmittedTools += len(payload.entry.Tools) - len(entry.Tools)
		page.Diagnostics.OmittedPayloads++
		*payload.entry = entry
	}
	return page
}

func (b *Browser) enforcePageBudget(page BrowsePage) BrowsePage {
	for !page.FitsReply() && len(page.Entries) > 0 {
		index := len(page.Entries) - 1
		entry := page.Entries[index]
		if len(entry.Tools) > 0 {
			entry.Tools = entry.Tools[:len(entry.Tools)-1]
			page.Diagnostics.OmittedTools++
			entry.Truncated = true
		} else if len(entry.Text) > 0 {
			entry.Text, _ = clampText(entry.Text, len(entry.Text)/2)
			page.Diagnostics.OmittedPayloads++
			entry.Truncated = true
		} else {
			page.Entries = page.Entries[:index]
			continue
		}
		page.Entries[index] = entry
	}
	if !page.FitsReply() {
		if page.OMOPlan != nil {
			page.Diagnostics.OmittedPayloads++
		}
		page.OMOPlan = nil
	}
	return page
}

func (b *Browser) finishFittedPage(page BrowsePage, original, fitted []projectedEntry) BrowsePage {
	toolsOmitted, payloadsOmitted := browseOmissions(original, fitted)
	page.Diagnostics.OmittedTools += toolsOmitted
	page.Diagnostics.OmittedPayloads += payloadsOmitted
	page.Entries = projectedEntries(fitted)
	return b.enforcePageBudget(page)
}

func cursorCandidatePage(base BrowsePage, next func(int64) (string, error)) func([]projectedEntry, int) (BrowsePage, error) {
	return func(candidate []projectedEntry, _ int) (BrowsePage, error) {
		page := base
		page.Entries = projectedEntries(candidate)
		if next == nil {
			return page, nil
		}
		cursor, err := next(candidate[0].Offset)
		if err != nil {
			return page, err
		}
		page.NextCursor = cursor
		page.HasMore = true
		return page, nil
	}
}

func (b *Browser) fitProjected(entries []projectedEntry, candidatePage func(candidate []projectedEntry, omitted int) (BrowsePage, error)) ([]projectedEntry, int) {
	if len(entries) == 0 {
		return nil, 0
	}
	for omitted := 0; omitted < len(entries); omitted++ {
		candidate := entries[omitted:]
		page, err := candidatePage(candidate, omitted)
		if err != nil {
			continue
		}
		if page.FitsReply() {
			return candidate, omitted
		}
	}
	last := entries[len(entries)-1]
	page, _ := candidatePage(entries[len(entries)-1:], len(entries)-1)
	last.Entry = boundBrowseEntry(last.Entry, pageEntryBudget(page))
	return []projectedEntry{last}, len(entries) - 1
}

func boundBrowseEntry(entry Entry, budget int) Entry {
	entry.Truncated = true
	for index := len(entry.Tools) - 1; index >= 0; index-- {
		candidate := entry
		candidate.Tools = append([]ToolActivity(nil), entry.Tools...)
		candidate.Tools[index] = boundBrowseTool(candidate.Tools[index])
		if data, _ := json.Marshal(candidate); len(data) <= budget {
			return candidate
		}
	}
	for len(entry.Tools) > 0 {
		candidate := entry
		candidate.Tools = entry.Tools[:len(entry.Tools)-1]
		if data, _ := json.Marshal(candidate); len(data) <= budget {
			return candidate
		}
		entry.Tools = candidate.Tools
	}
	entry.Text, _ = clampText(entry.Text, 4096)
	for len(entry.Text) > 0 {
		if data, _ := json.Marshal(entry); len(data) <= budget {
			return entry
		}
		entry.Text, _ = clampText(entry.Text, len(entry.Text)/2)
	}
	return entry
}

func boundBrowseTool(tool ToolActivity) ToolActivity {
	tool.Truncated = true
	tool.Input, _ = clampText(tool.Input, 1024)
	tool.Output, _ = clampText(tool.Output, 1024)
	return tool
}

func pageEntryBudget(page BrowsePage) int {
	page.Entries = []Entry{}
	return browseResponseBytes - pageSize(page) - 64
}

func pageSize(page BrowsePage) int {
	data, err := json.Marshal(page)
	if err != nil {
		return math.MaxInt
	}
	return len(data)
}

func cloneTodo(plan *OMOTodoState) *OMOTodoState {
	if plan == nil {
		return nil
	}
	copyPlan := *plan
	copyPlan.Phases = make([]OMOTodoPhase, len(plan.Phases))
	for phaseIndex, phase := range plan.Phases {
		copyPlan.Phases[phaseIndex] = OMOTodoPhase{Name: phase.Name, Tasks: append([]OMOTodoTask(nil), phase.Tasks...)}
	}
	return &copyPlan
}

func boundTodoPlan(plan *OMOTodoState, budget int) *OMOTodoState {
	copyPlan := cloneTodo(plan)
	if copyPlan == nil {
		return nil
	}
	for phaseIndex := range copyPlan.Phases {
		copyPlan.Phases[phaseIndex].Name, _ = clampText(copyPlan.Phases[phaseIndex].Name, 512)
		for taskIndex := range copyPlan.Phases[phaseIndex].Tasks {
			content, truncated := clampText(copyPlan.Phases[phaseIndex].Tasks[taskIndex].Content, 1024)
			copyPlan.Phases[phaseIndex].Tasks[taskIndex].Content = content
			copyPlan.Truncated = copyPlan.Truncated || truncated
		}
	}
	for {
		data, _ := json.Marshal(copyPlan)
		if len(data) <= budget {
			return copyPlan
		}
		last := len(copyPlan.Phases) - 1
		if last < 0 {
			return copyPlan
		}
		if len(copyPlan.Phases[last].Tasks) > 0 {
			copyPlan.Phases[last].Tasks = copyPlan.Phases[last].Tasks[:len(copyPlan.Phases[last].Tasks)-1]
		} else {
			copyPlan.Phases = copyPlan.Phases[:last]
		}
		copyPlan.Truncated = true
	}
}

func nativeFailure(code, reason string) BrowsePage {
	if code == "invalid_session" || code == "source_unavailable" {
		return browseUnavailable(code, reason)
	}
	if code == "source_corrupt" {
		return failPage(code, reason, false)
	}
	return failPage(code, reason, code == "output_limit" || code == "query_failed")
}

func (b *Browser) nativePage(request BrowseRequest, entries []Entry, hasMore bool, corrupt bool, diagnostics BrowseDiagnostics, total int, rawBefore, revision string, cursor browseCursor) BrowsePage {
	if corrupt {
		diagnostics.CorruptRecords = max(1, diagnostics.CorruptRecords)
	}
	diagnostics = mergeBrowseDiagnostics(diagnostics, normalizeEntriesForResponse(entries))
	base := BrowsePage{
		Available: true, State: BrowseReady, Mode: BrowseNative, Entries: []Entry{},
		HasMore: hasMore, SourceRevision: revision, Diagnostics: diagnostics,
	}
	if IsKimi(request.Scope.Provider) {
		base.Capabilities = &SourceCapabilities{Source: "recorded_history", ActiveBranch: true}
	}
	base = request.withObservation(base)
	if total != UnknownTotal {
		baseTotal := total
		base.Total = &baseTotal
	}
	projected := make([]projectedEntry, 0, len(entries))
	for index, entry := range entries {
		projected = append(projected, projectedEntry{Offset: int64(index), Entry: entry})
	}
	potentialOlder := hasMore
	cursorFor := func(entry Entry) (string, error) {
		before := entry.ID
		if before == "" {
			before = rawBefore
		}
		if before == "" {
			before = cursor.NativeBefore
		}
		if before == "" {
			return "", errors.New("native conversation cursor is empty")
		}
		return encodeBrowseCursor(b.key, browseCursor{
			Mode: "native", Scope: browseScopeID(request.Scope), Revision: revision, NativeBefore: before,
			RangeEnd: cursor.RangeEnd, RangeDigest: cursor.RangeDigest,
			ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
		})
	}
	selected, omitted := b.fitProjected(projected, func(candidate []projectedEntry, omitted int) (BrowsePage, error) {
		page := base
		page.Entries = projectedEntries(candidate)
		page.HasMore = potentialOlder || omitted > 0
		if page.HasMore {
			page.NextCursor, _ = cursorFor(candidate[0].Entry)
		}
		return page, nil
	})
	if omitted > 0 || potentialOlder {
		base.HasMore = true
		anchor := Entry{}
		if len(selected) > 0 {
			anchor = selected[0].Entry
		}
		var err error
		base.NextCursor, err = cursorFor(anchor)
		if err != nil {
			return failPage("source_corrupt", "The native conversation cursor was not available.", false)
		}
	}
	return b.finishFittedPage(base, projected, selected)
}
