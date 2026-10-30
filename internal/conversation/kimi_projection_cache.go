package conversation

import (
	"bytes"
	"context"
	"os"
	"time"
)

type kimiProjectionSnapshot struct {
	path       string
	root       string
	scope      string
	identity   string
	size       int64
	modified   int64
	changed    string
	projection kimiPageProjection
	revision   string
	lastUsed   time.Time
}

func kimiSnapshotMatches(snapshot *kimiProjectionSnapshot, location Location, scope BrowseScope, info os.FileInfo) bool {
	return snapshot != nil && snapshot.path == location.Path && snapshot.root == location.Root && snapshot.scope == browseScopeID(scope) && snapshot.identity == fileIdentity(info) && snapshot.size == info.Size() && snapshot.modified == info.ModTime().UnixNano() && snapshot.changed == fileChangeToken(info)
}

func (b *Browser) projectKimiSource(ctx context.Context, location Location, scope BrowseScope) (kimiPageProjection, string, string) {
	if ctx.Err() != nil {
		return kimiPageProjection{}, "", "request_cancelled"
	}
	file, info, err := openContainedConversationFile(location)
	if err != nil {
		return kimiPageProjection{}, "", "source_unavailable"
	}
	defer file.Close()
	if info.Size() > maxConversationBytes {
		b.kimiProjection = nil
		return kimiPageProjection{}, "", "output_limit"
	}
	current := func() bool {
		after, err := file.Stat()
		if err != nil || after.Size() != info.Size() || after.ModTime() != info.ModTime() || fileChangeToken(after) != fileChangeToken(info) {
			return false
		}
		check, pathInfo, err := openContainedConversationFile(location)
		if err != nil {
			return false
		}
		check.Close()
		return os.SameFile(info, pathInfo) && pathInfo.Size() == info.Size() && pathInfo.ModTime() == info.ModTime() && fileChangeToken(pathInfo) == fileChangeToken(info)
	}
	if kimiSnapshotMatches(b.kimiProjection, location, scope, info) && time.Since(b.kimiProjection.lastUsed) < b.options.RecentCacheTTL && current() {
		b.kimiProjection.lastUsed = time.Now()
		return b.kimiProjection.projection, b.kimiProjection.revision, ""
	}
	b.kimiProjection = nil
	var data bytes.Buffer
	err = writeFileRangeObserved(ctx, file, 0, info.Size(), &data, nil, func(n int64) error {
		b.observePhysicalRead(n)
		return nil
	})
	if ctx.Err() != nil {
		return kimiPageProjection{}, "", "request_cancelled"
	}
	if err != nil || !current() {
		return kimiPageProjection{}, "", "source_changed"
	}
	if b.kimiProjectionObserver != nil {
		b.kimiProjectionObserver()
	}
	projection := validateKimi(ctx, data.Bytes())
	if projection.metadata.code != "" {
		return projection, "", projection.metadata.code
	}
	if !current() {
		return kimiPageProjection{}, "", "source_changed"
	}
	revision := b.reader.kimiRevision(location, info, projection.metadata)
	accounted := projection.retainedBytes() + int64(len(location.Path)+len(location.Root)+len(browseScopeID(scope))+len(fileIdentity(info))+len(fileChangeToken(info))+len(revision)) + 512
	if accounted <= b.options.RecentCacheItemBytes && accounted <= b.options.RecentCacheBytes {
		b.kimiProjection = &kimiProjectionSnapshot{
			path: location.Path, root: location.Root, scope: browseScopeID(scope), identity: fileIdentity(info),
			size: info.Size(), modified: info.ModTime().UnixNano(), changed: fileChangeToken(info),
			projection: projection, revision: revision, lastUsed: time.Now(),
		}
	}
	return projection, revision, ""
}
