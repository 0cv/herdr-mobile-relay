package conversation

import (
	"context"
	"errors"
	"strconv"
)

func (b *Browser) readGrokNative(ctx context.Context, request BrowseRequest, cursor browseCursor, before string) (BrowsePage, error) {
	const unavailable = "Grok conversation history is unavailable."
	const changed = "The Grok session log changed while history was being browsed."
	if !safeSessionID(request.Scope.SessionID) {
		return nativeFailure("invalid_session", unavailable), nil
	}
	if !validGrokCursor(before) {
		return nativeFailure("invalid_cursor", "This history cursor is invalid for the requested conversation."), nil
	}
	location := b.reader.LocateWithProject("grok", ProjectContext{CWD: request.Scope.CWD}, request.Scope.SessionID)
	if location.Path == "" {
		return nativeFailure("invalid_session", unavailable), nil
	}
	projection, revision, digest, end, err := captureGrokUpdates(ctx, location, cursor)
	if ctx.Err() != nil {
		return browseFailure("request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
	}
	if errors.Is(err, errGrokSourceChanged) || err == nil && cursor.Revision != "" && cursor.Revision != revision {
		return nativeFailure("source_changed", changed), nil
	}
	if err != nil {
		return nativeFailure("source_unavailable", unavailable), nil
	}
	entries, hasMore, total, ok := grokPage(projection.Entries, before, request.Limit)
	if !ok {
		return nativeFailure("invalid_cursor", "This history cursor is invalid for the requested conversation."), nil
	}
	cursor.RangeEnd, cursor.RangeDigest = strconv.FormatInt(end, 10), digest
	page := b.nativePage(request.Scope, entries, hasMore, projection.Corrupt > 0, total, "", revision, cursor)
	if page.Available {
		page.Diagnostics.CorruptRecords = max(page.Diagnostics.CorruptRecords, projection.Corrupt)
		page.Diagnostics.OversizedRecords += projection.Oversized
	}
	return page, nil
}
