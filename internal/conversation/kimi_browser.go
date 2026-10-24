package conversation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
)

type kimiSourceState struct {
	identity string
	digest   [32]byte
	end      int
	mutation string
	revision string
}

func (r *Reader) kimiRevision(location Location, info os.FileInfo, projection kimiProjection) string {
	identity := fileIdentity(info)
	if r.kimiSources == nil {
		r.kimiSources = make(map[string]kimiSourceState)
	}
	prior, exists := r.kimiSources[location.Path]
	digest := sha256.Sum256(projection.complete)
	revision := prior.revision
	if !exists || prior.identity != identity || len(projection.complete) < prior.end || sha256.Sum256(projection.complete[:min(len(projection.complete), prior.end)]) != prior.digest || prior.mutation != projection.mutation {
		revision = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%x:%s", identity, prior.revision, digest, projection.mutation))))
	}
	if len(r.kimiSources) >= 8 && !exists {
		clear(r.kimiSources)
	}
	r.kimiSources[location.Path] = kimiSourceState{identity: identity, digest: digest, end: len(projection.complete), mutation: projection.mutation, revision: revision}
	return revision
}

func (b *Browser) readKimiPage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	if !b.reader.KimiSelectionCurrent(request.Scope) {
		return browseUnavailable("source_changed", "Kimi session changed. Start again from the latest messages."), nil
	}
	if request.Scope.SelectionReason != "" {
		return browseUnavailable(request.Scope.SelectionReason, kimiReason(request.Scope.SelectionReason)), nil
	}
	if request.Scope.SessionID == "" {
		return browseUnavailable("invalid_session", kimiReason("invalid_session")), nil
	}
	b.reader.kimiMu.Lock()
	defer b.reader.kimiMu.Unlock()
	reported := request.Scope.SessionID
	if request.Scope.InferredSession {
		reported = ""
	}
	id, location, code := b.reader.SelectKimiSession(ProjectContext{CWD: request.Scope.CWD, ForegroundCWD: request.Scope.ForegroundCWD}, reported)
	if request.Scope.InferredSession && (id != request.Scope.SessionID || code != request.Scope.SelectionReason || KimiSourceIdentity(location) != request.Scope.SelectionSource) {
		return browseUnavailable("source_changed", "Kimi session changed. Start again from the latest messages."), nil
	}
	if code != "" {
		return browseUnavailable(code, kimiReason(code)), nil
	}
	projection, revision, code := b.projectKimiSource(ctx, location, request.Scope)
	if code != "" {
		return browseUnavailable(code, kimiReason(code)), nil
	}
	if revision == "" {
		return browseUnavailable("source_unavailable", kimiReason("source_unavailable")), nil
	}
	var cursor browseCursor
	if request.Cursor != "" {
		var err error
		cursor, err = decodeBrowseCursor(b.key, request.Cursor, request.Scope)
		if err != nil || cursor.Mode != "native" || cursor.NativeBefore == "" {
			return browseFailure(true, "invalid_cursor", "This Kimi history cursor is invalid.", browseErrorFor("invalid_cursor", "Start again from the latest messages.", false)), nil
		}
		if cursor.Revision != revision {
			return browseFailure(true, "source_changed", "Kimi history changed. Start again from the latest messages.", browseErrorFor("source_changed", "Kimi history changed.", false)), nil
		}
	}
	end := len(projection.entries)
	if cursor.NativeBefore != "" {
		found := false
		for i, entry := range projection.entries {
			if entry.id == cursor.NativeBefore {
				end, found = i, true
				break
			}
		}
		if !found {
			return browseUnavailable("source_changed", "Kimi history changed. Start again from the latest messages."), nil
		}
	}
	start := max(0, end-request.Limit)
	entries, work, code := projection.materialize(ctx, start, end)
	if b.kimiMaterializationObserver != nil {
		b.kimiMaterializationObserver(work)
	}
	if code != "" {
		return browseUnavailable(code, kimiReason(code)), nil
	}
	page := b.nativePage(request.Scope, entries, start > 0, false, len(projection.entries), "", revision, cursor)
	page.Diagnostics.ContinuationIncomplete = projection.metadata.diagnostics.ContinuationIncomplete
	page.Diagnostics.ContinuationReason = projection.metadata.diagnostics.ContinuationReason
	page.Capabilities = &SourceCapabilities{Source: "recorded_history", ActiveBranch: true}
	if !b.reader.KimiSelectionCurrent(request.Scope) {
		return browseUnavailable("source_changed", "Kimi session changed. Start again from the latest messages."), nil
	}
	return b.enforcePageBudget(page), nil
}

func (r *Reader) kimiChangeToken(ctx context.Context, scope BrowseScope) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	reported := scope.SessionID
	if scope.InferredSession {
		reported = ""
	}
	id, location, code := r.SelectKimiSession(ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, reported)
	if scope.InferredSession && (id != scope.SessionID || code != scope.SelectionReason || KimiSourceIdentity(location) != scope.SelectionSource) {
		return "source_changed", nil
	}
	if code != "" || scope.SessionID == "" {
		return code, nil
	}
	info, err := os.Stat(location.Path)
	if err != nil {
		return "source_unavailable", nil
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%d:%s", location.Path, fileIdentity(info), info.Size(), info.ModTime().UnixNano(), fileChangeToken(info))))), nil
}
