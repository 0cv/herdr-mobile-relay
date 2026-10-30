package conversation

import (
	"context"
	"os"
)

func (r *Reader) ReadKimiSource(ctx context.Context, scope BrowseScope, before string, limit int) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if !IsKimi(scope.Provider) || !scope.InferredSession || scope.SessionID == "" || scope.SelectionReason != "" || scope.SelectionSource == "" {
		return unavailableCode("source_changed", kimiReason("source_changed")), nil
	}
	r.kimiMu.Lock()
	defer r.kimiMu.Unlock()
	id, location, code := r.SelectKimiSession(ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, "")
	if code != "" || id != scope.SessionID || KimiSourceIdentity(location) != scope.SelectionSource {
		return unavailableCode("source_changed", kimiReason("source_changed")), nil
	}
	info, err := os.Stat(location.Path)
	if err != nil {
		return unavailableCode("source_unavailable", kimiReason("source_unavailable")), nil
	}
	projection, _, code := r.readKimiLocation(ctx, location)
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	after, err := os.Stat(location.Path)
	if err != nil || !os.SameFile(info, after) || kimiStatToken(after, false) != kimiStatToken(info, false) || !r.KimiSelectionCurrent(scope) {
		return unavailableCode("source_changed", kimiReason("source_changed")), nil
	}
	if code != "" {
		return unavailableCode(code, kimiReason(code)), nil
	}
	return kimiPage(projection, before, limit), nil
}

func kimiPage(projection kimiProjection, before string, limit int) Page {
	entries := projection.entries
	end := len(entries)
	if before != "" {
		for i := range entries {
			if entries[i].ID == before {
				end = i
				break
			}
		}
	}
	if limit < 1 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	start := max(0, end-limit)
	return Page{Available: true, Entries: entries[start:end], Total: len(entries), HasMore: start > 0, ContinuationIncomplete: projection.diagnostics.ContinuationIncomplete, ContinuationReason: projection.diagnostics.ContinuationReason}
}
