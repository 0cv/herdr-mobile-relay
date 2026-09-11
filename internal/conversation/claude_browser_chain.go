package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

const maxClaudeChainContexts = 256

var errClaudeChainLineageChanged = errors.New("Claude continuation lineage changed")

type claudeChainLineage struct {
	segments   []claudeSegment
	lastAccess time.Time
}

type claudeChainContext struct {
	id             string
	key            string
	scopeID        string
	publicRevision string
	snapshotID     string
	// chain is a descriptor-only manifest. Projected rows are obtained through
	// recentProjectionCache or a per-segment snapshot job and are never retained
	// by every manifest until CursorTTL.
	chain          claudeChain
	recentComplete []bool
	prepared       []bool
	preparing      []bool
	jobs           []*browseJob
	diagnostics    BrowseDiagnostics
	lastAccess     time.Time
	refs           int
}

func (b *Browser) readClaudeChainPage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	anchor := b.reader.Locate(request.Scope.Provider, request.Scope.CWD, request.Scope.SessionID)
	if anchor.Path == "" {
		return browseUnavailable("invalid_session", "No conversation log is available for this session."), nil
	}
	chain, err := resolveClaudeChain(ctx, anchor, request.Scope.SessionID)
	if err != nil {
		if ctx.Err() != nil {
			return b.recordReadFailure(err), nil
		}
		return browseUnavailable("source_unavailable", "The conversation source could not be read."), nil
	}
	if len(chain.Segments) == 1 && !chain.incomplete() {
		return b.readRecentFilePage(ctx, request, false)
	}
	chainContext, err := b.acquireClaudeChainContext(ctx, request.Scope, chain)
	if err != nil {
		if errors.Is(err, errClaudeChainCapacity) {
			message := "Conversation history browsing is busy; try again shortly."
			return browseFailure(true, "index_capacity_exceeded", message, browseErrorFor("index_capacity_exceeded", message, true)), nil
		}
		if errors.Is(err, errClaudeChainLineageChanged) {
			// The current request reports the replacement against the old
			// snapshot. Retire that scope's unreferenced context and lineage so a
			// subsequent latest request can establish a new identity and recover.
			b.resetClaudeChainScope(browseScopeID(request.Scope))
			message := "The conversation source changed while history was being browsed."
			return browseFailure(true, "source_changed", message, browseErrorFor("source_changed", message, false)), nil
		}
		return b.recordReadFailure(err), nil
	}
	defer b.releaseClaudeChainContext(chainContext)
	return b.claudeChainLatestPage(ctx, request.Scope, request.Limit, chainContext), nil
}

func (b *Browser) readClaudeChainCursorPage(ctx context.Context, request BrowseRequest, cursor browseCursor) (BrowsePage, error) {
	if cursor.ChainID == "" || cursor.Segment == nil || *cursor.Segment < 0 || cursor.SnapshotID == "" {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
	}
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
	}
	chainContext := b.acquireClaudeChainByID(cursor.ChainID)
	if chainContext == nil {
		return browseFailure(true, "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", true)), nil
	}
	defer b.releaseClaudeChainContext(chainContext)
	segmentIndex := *cursor.Segment
	view := b.claudeChainView(chainContext)
	if cursor.Revision != view.publicRevision || cursor.SnapshotID != view.snapshotID || cursor.Scope != browseScopeID(request.Scope) || segmentIndex >= len(view.chain.Segments) {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)), nil
	}
	segment := view.chain.Segments[segmentIndex]
	if err := validateClaudeChainSegment(ctx, segment); err != nil {
		if ctx.Err() != nil {
			return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
		}
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	var recent recentProjection
	var recentBudget *claudeChainRecentBudget
	if !cursor.ChainOffset {
		recentBudget = &claudeChainRecentBudget{remaining: b.options.RecentBytes}
		recent, err = b.loadClaudeChainRecentWithBudget(ctx, request.Scope, chainContext, segmentIndex, recentBudget)
		if err != nil {
			return b.claudeChainSourceFailure(ctx, err), nil
		}
		if boundary > int64(len(recent.Entries)) {
			return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)), nil
		}
	} else if boundary > segment.CapturedEnd {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)), nil
	}
	if !b.claudeChainSegmentPrepared(chainContext, segmentIndex) && cursor.ChainOffset {
		b.mu.Lock()
		alreadyPreparing := chainContext.preparing[segmentIndex]
		if !alreadyPreparing {
			chainContext.preparing[segmentIndex] = true
			b.mu.Unlock()
			return b.claudeChainPreparingPage(request.Scope, request.Limit, chainContext, segmentIndex, boundary, true), nil
		}
		job := chainContext.jobs[segmentIndex]
		recentComplete := chainContext.recentComplete[segmentIndex]
		if job == nil && recentComplete {
			// A byte cursor can only reach this branch after a previously
			// complete recent range was reused. It does not need an index; use
			// the bounded projection and translate the byte boundary below.
			chainContext.preparing[segmentIndex] = false
			b.mu.Unlock()
		} else {
			b.mu.Unlock()
			if job == nil {
				var page *BrowsePage
				job, page = b.startClaudeChainJob(request.Scope, chainContext, segmentIndex)
				if page != nil {
					return b.claudeChainAdmissionFailure(request.Scope, chainContext, segmentIndex, boundary, true, *page), nil
				}
				if job == nil {
					page := browseFailure(true, "index_failed", "History preparation could not be started.", browseErrorFor("index_failed", "History preparation could not be started.", true))
					return b.claudeChainAdmissionFailure(request.Scope, chainContext, segmentIndex, boundary, true, page), nil
				}
			}
			return b.continueClaudeChainJob(ctx, request, chainContext, segmentIndex, boundary, true, job), nil
		}
	}
	if cursor.ChainOffset {
		if b.claudeChainSegmentPrepared(chainContext, segmentIndex) {
			return b.claudeChainPreparedPageAt(ctx, request.Scope, request.Limit, chainContext, segmentIndex, boundary, request.Retry), nil
		}
		recent, recentErr := b.loadClaudeChainRecent(ctx, request.Scope, chainContext, segmentIndex)
		if recentErr != nil {
			return b.claudeChainSourceFailure(ctx, recentErr), nil
		}
		byteBoundary := 0
		for _, entry := range recent.Entries {
			if entry.Offset < boundary {
				byteBoundary++
			}
		}
		return b.claudeChainPageAt(ctx, request.Scope, request.Limit, chainContext, false, segmentIndex, byteBoundary, recent.Entries, nil), nil
	}
	return b.claudeChainPageAt(ctx, request.Scope, request.Limit, chainContext, false, segmentIndex, int(boundary), recent.Entries, recentBudget), nil
}

var (
	errClaudeChainCapacity     = errors.New("Claude chain context capacity is occupied")
	errClaudeChainRecentBudget = errors.New("Claude chain recent read budget exhausted")
)

type claudeChainRecentBudget struct {
	remaining int64
}

func (budget *claudeChainRecentBudget) take(bytes int64) bool {
	if budget == nil {
		return true
	}
	if bytes < 0 || bytes > budget.remaining {
		return false
	}
	budget.remaining -= bytes
	return true
}

func (b *Browser) acquireClaudeChainContext(ctx context.Context, scope BrowseScope, chain claudeChain) (*claudeChainContext, error) {
	key := claudeChainKey(scope, chain)
	scopeID := browseScopeID(scope)
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, errors.New("conversation browser is closed")
	}
	lineage, hasLineage := b.chainLineage[scopeID]
	lineage.segments = append([]claudeSegment(nil), lineage.segments...)
	b.mu.Unlock()
	if hasLineage && !claudeChainLineageCompatible(ctx, lineage.segments, chain.Segments) {
		return nil, errClaudeChainLineageChanged
	}

	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, errors.New("conversation browser is closed")
	}
	if id := b.chainByKey[key]; id != "" {
		if existing := b.chains[id]; existing != nil {
			existing.refs++
			existing.lastAccess = time.Now()
			b.mu.Unlock()
			return existing, nil
		}
		delete(b.chainByKey, key)
	}
	if !b.reserveClaudeChainContextLocked() {
		b.mu.Unlock()
		return nil, errClaudeChainCapacity
	}
	b.mu.Unlock()

	built, err := b.buildClaudeChainContext(ctx, scope, chain, scopeID, key)
	b.mu.Lock()
	if b.chainReservations > 0 {
		b.chainReservations--
	}
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	// The reservation makes this admission atomic with all other builders. A
	// duplicate may still have won while this builder was scanning; retain the
	// first manifest and discard only the unregistered descriptor copy.
	if id := b.chainByKey[key]; id != "" {
		if existing := b.chains[id]; existing != nil {
			existing.refs++
			existing.lastAccess = time.Now()
			b.mu.Unlock()
			return existing, nil
		}
	}
	if len(b.chains) >= maxClaudeChainContexts {
		// This is only possible if a caller violated the reservation invariant;
		// fail closed rather than growing the registry past its hard cap.
		b.mu.Unlock()
		return nil, errClaudeChainCapacity
	}
	built.refs = 1
	built.lastAccess = time.Now()
	b.chains[built.id] = built
	b.chainByKey[key] = built.id
	b.recordClaudeChainLineageLocked(scopeID, chain.Segments)
	b.mu.Unlock()
	return built, nil
}

func claudeChainLineageCompatible(ctx context.Context, previous, current []claudeSegment) bool {
	for index, old := range previous {
		if index >= len(current) {
			// A temporarily missing descendant is allowed to recover, but retain
			// the longer lineage so a replacement cannot hide behind the gap.
			return true
		}
		candidate := current[index]
		if old.SessionID != candidate.SessionID || old.Location.Path != candidate.Location.Path ||
			old.FileIdentity != candidate.FileIdentity || candidate.CapturedEnd < old.CapturedEnd {
			return false
		}
		if candidate.CapturedEnd == old.CapturedEnd {
			if old.FileRevision != candidate.FileRevision || old.SourceModTime != candidate.SourceModTime {
				return false
			}
			if old.FooterDigest != "" && old.FooterDigest != candidate.FooterDigest {
				return false
			}
			if old.CapturedDigest != "" && candidate.CapturedDigest != "" && old.CapturedDigest != candidate.CapturedDigest {
				return false
			}
			continue
		}
		// A longer descriptor is append-compatible only when the bytes captured
		// by the prior context still form the prefix. This is done lazily, and
		// only for ranges whose digest was already established by a bounded read
		// or a background index job.
		if old.CapturedDigest != "" && !claudeChainPrefixMatches(ctx, old, candidate) {
			return false
		}
	}
	return true
}

func claudeChainPrefixMatches(ctx context.Context, old, candidate claudeSegment) bool {
	if old.CapturedEnd < 0 || candidate.CapturedEnd < old.CapturedEnd {
		return false
	}
	source, err := captureFileSource(candidate.Location)
	if err != nil {
		return false
	}
	defer source.close()
	if source.end < old.CapturedEnd || old.FileIdentity != "" && fileIdentity(source.info) != old.FileIdentity {
		return false
	}
	digest, err := fileRangeDigest(ctx, source.file, 0, old.CapturedEnd)
	return err == nil && digest == old.CapturedDigest
}

func (b *Browser) recordClaudeChainLineageLocked(scopeID string, segments []claudeSegment) {
	merged := append([]claudeSegment(nil), segments...)
	if previous, ok := b.chainLineage[scopeID]; ok {
		for index := range merged {
			if index >= len(previous.segments) {
				break
			}
			old := previous.segments[index]
			if old.SessionID == merged[index].SessionID && old.Location.Path == merged[index].Location.Path &&
				old.FileIdentity == merged[index].FileIdentity && old.CapturedDigest != "" && merged[index].CapturedDigest == "" {
				merged[index].CapturedDigest = old.CapturedDigest
			}
		}
		if len(previous.segments) > len(merged) {
			merged = append([]claudeSegment(nil), previous.segments...)
		}
	}
	b.chainLineage[scopeID] = claudeChainLineage{segments: merged, lastAccess: time.Now()}
	for len(b.chainLineage) > maxClaudeChainContexts {
		oldestScope := ""
		var oldest time.Time
		for scope, lineage := range b.chainLineage {
			if oldestScope == "" || lineage.lastAccess.Before(oldest) {
				oldestScope, oldest = scope, lineage.lastAccess
			}
		}
		if oldestScope == "" {
			break
		}
		delete(b.chainLineage, oldestScope)
	}
}

func (b *Browser) updateClaudeChainLineageEvidenceLocked(chain *claudeChainContext) {
	if chain == nil {
		return
	}
	lineage, ok := b.chainLineage[chain.scopeID]
	if !ok {
		return
	}
	for index := range lineage.segments {
		if index >= len(chain.chain.Segments) {
			break
		}
		current := chain.chain.Segments[index]
		if current.CapturedDigest != "" {
			lineage.segments[index].CapturedDigest = current.CapturedDigest
		}
		if current.RecentDigest != "" {
			lineage.segments[index].RecentStart = current.RecentStart
			lineage.segments[index].RecentEnd = current.RecentEnd
			lineage.segments[index].RecentDigest = current.RecentDigest
		}
	}
	lineage.lastAccess = time.Now()
	b.chainLineage[chain.scopeID] = lineage
}

func (b *Browser) resetClaudeChainScope(scopeID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, chain := range b.chains {
		if chain.scopeID != scopeID || chain.refs != 0 {
			continue
		}
		delete(b.chains, id)
		if b.chainByKey[chain.key] == id {
			delete(b.chainByKey, chain.key)
		}
	}
	delete(b.chainLineage, scopeID)
}

func (b *Browser) reserveClaudeChainContextLocked() bool {
	if len(b.chains)+b.chainReservations < maxClaudeChainContexts {
		b.chainReservations++
		return true
	}
	// Expired contexts are always eligible. At admission pressure, inactive
	// contexts are also eligible for oldest-idle eviction; active readers are
	// protected by refs and are never evicted.
	now := time.Now()
	for len(b.chains)+b.chainReservations >= maxClaudeChainContexts {
		var candidateID string
		var candidate *claudeChainContext
		for id, existing := range b.chains {
			if existing.refs != 0 || (candidate != nil && !existing.lastAccess.Before(candidate.lastAccess)) {
				continue
			}
			if candidate == nil || now.Sub(existing.lastAccess) >= b.options.CursorTTL || existing.lastAccess.Before(candidate.lastAccess) {
				candidateID, candidate = id, existing
			}
		}
		if candidate == nil {
			return false
		}
		delete(b.chains, candidateID)
		if b.chainByKey[candidate.key] == candidateID {
			delete(b.chainByKey, candidate.key)
		}
	}
	b.chainReservations++
	return true
}

func (b *Browser) acquireClaudeChainByID(id string) *claudeChainContext {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.chains[id]
	if chain == nil || b.closing {
		return nil
	}
	if time.Since(chain.lastAccess) >= b.options.CursorTTL && chain.refs == 0 {
		delete(b.chains, id)
		if b.chainByKey[chain.key] == id {
			delete(b.chainByKey, chain.key)
		}
		return nil
	}
	chain.refs++
	chain.lastAccess = time.Now()
	return chain
}

func (b *Browser) releaseClaudeChainContext(chain *claudeChainContext) {
	if chain == nil {
		return
	}
	b.mu.Lock()
	if chain.refs > 0 {
		chain.refs--
	}
	chain.lastAccess = time.Now()
	b.mu.Unlock()
}

type claudeChainView struct {
	id             string
	key            string
	publicRevision string
	snapshotID     string
	scopeID        string
	chain          claudeChain
	recentComplete []bool
	prepared       []bool
	preparing      []bool
	jobs           []*browseJob
	diagnostics    BrowseDiagnostics
}

// claudeChainView is the only way request code reads mutable chain metadata.
// Chain contexts are deliberately shared by identical latest and cursor
// requests, so a read must copy the descriptor and state while holding the
// browser mutex before doing file or index work.
func (b *Browser) claudeChainView(chain *claudeChainContext) claudeChainView {
	if chain == nil {
		return claudeChainView{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return claudeChainView{
		id:             chain.id,
		key:            chain.key,
		publicRevision: chain.publicRevision,
		snapshotID:     chain.snapshotID,
		scopeID:        chain.scopeID,
		chain:          claudeChain{Segments: append([]claudeSegment(nil), chain.chain.Segments...), IncompleteReason: chain.chain.IncompleteReason},
		recentComplete: append([]bool(nil), chain.recentComplete...),
		prepared:       append([]bool(nil), chain.prepared...),
		preparing:      append([]bool(nil), chain.preparing...),
		jobs:           append([]*browseJob(nil), chain.jobs...),
		diagnostics:    chain.diagnostics,
	}
}

func (b *Browser) claudeChainSegmentSnapshot(chain *claudeChainContext, segment int) (claudeSegment, bool) {
	view := b.claudeChainView(chain)
	if segment < 0 || segment >= len(view.chain.Segments) {
		return claudeSegment{}, false
	}
	return view.chain.Segments[segment], true
}

func claudeChainKey(scope BrowseScope, chain claudeChain) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, browseScopeID(scope))
	for _, segment := range chain.Segments {
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, segment.SessionID)
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, segment.Location.Path)
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, segment.FileRevision)
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, browseOffset(segment.SourceModTime))
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, browseOffset(segment.CapturedEnd))
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, browseOffset(segment.FooterStart))
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, browseOffset(segment.FooterEnd))
		_, _ = io.WriteString(hash, "\x00")
		_, _ = io.WriteString(hash, segment.FooterDigest)
	}
	_, _ = io.WriteString(hash, "\x00")
	_, _ = io.WriteString(hash, chain.IncompleteReason)
	return hex.EncodeToString(hash.Sum(nil))
}

func (b *Browser) buildClaudeChainContext(ctx context.Context, _ BrowseScope, chain claudeChain, scopeID, key string) (*claudeChainContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := randomIdentifier()
	if err != nil {
		return nil, err
	}
	snapshotID, err := randomIdentifier()
	if err != nil {
		return nil, err
	}
	context := &claudeChainContext{
		id: id, key: key, scopeID: scopeID, snapshotID: snapshotID,
		publicRevision: chain.Segments[0].FileRevision, chain: chain,
		recentComplete: make([]bool, len(chain.Segments)), prepared: make([]bool, len(chain.Segments)),
		preparing: make([]bool, len(chain.Segments)), jobs: make([]*browseJob, len(chain.Segments)),
		lastAccess: time.Now(),
	}
	if chain.incomplete() {
		context.diagnostics.ContinuationIncomplete = true
		context.diagnostics.ContinuationReason = chain.IncompleteReason
	}
	// Only descriptor metadata is retained here. A segment whose captured range
	// fits the configured recent window can be read from the shared bounded
	// recent cache on demand; a larger range is promoted to the normal snapshot
	// worker. In particular, do not project every segment while admitting a
	// manifest or multiply the 16 MiB budget by chain length.
	for index, segment := range chain.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		context.recentComplete[index] = segment.CapturedEnd <= b.options.RecentBytes
		if !context.recentComplete[index] {
			context.diagnostics.SourceTruncated = true
		}
	}
	return context, nil
}

func (b *Browser) loadClaudeChainRecent(ctx context.Context, scope BrowseScope, chain *claudeChainContext, segmentIndex int) (recentProjection, error) {
	return b.loadClaudeChainRecentWithBudget(ctx, scope, chain, segmentIndex, nil)
}

func (b *Browser) loadClaudeChainRecentWithBudget(ctx context.Context, scope BrowseScope, chain *claudeChainContext, segmentIndex int, budget *claudeChainRecentBudget) (recentProjection, error) {
	segment, ok := b.claudeChainSegmentSnapshot(chain, segmentIndex)
	if !ok {
		return recentProjection{}, errors.New("invalid Claude chain segment")
	}
	source, err := captureFileSource(segment.Location)
	if err != nil {
		return recentProjection{}, err
	}
	defer source.close()
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd ||
		segment.FileIdentity != "" && fileIdentity(source.info) != segment.FileIdentity {
		return recentProjection{}, errors.New("Claude chain source changed")
	}
	if source.end == segment.CapturedEnd && segment.SourceModTime != 0 && source.info.ModTime().UnixNano() != segment.SourceModTime {
		return recentProjection{}, errors.New("Claude chain source changed")
	}
	source.end = segment.CapturedEnd
	window := b.options.RecentBytes
	if segment.RecentDigest != "" && segment.RecentStart >= 0 && segment.RecentEnd == segment.CapturedEnd {
		// Reuse the established frozen range. It is still bounded by the
		// original request, and changing it would invalidate the cursor.
		window = segment.RecentEnd - segment.RecentStart
	}
	if window < 1 {
		window = 1
	}
	if window > segment.CapturedEnd {
		window = segment.CapturedEnd
	}
	if budget != nil && !budget.take(window) {
		return recentProjection{}, errClaudeChainRecentBudget
	}
	b.observeSourceRead(window)
	start := segment.CapturedEnd - window
	if start < 0 {
		start = 0
	}
	digest, err := fileRangeDigest(ctx, source.file, start, segment.CapturedEnd)
	if err != nil {
		return recentProjection{}, err
	}
	if segment.RecentDigest != "" && (segment.RecentStart != start || segment.RecentEnd != segment.CapturedEnd || segment.RecentDigest != digest) {
		return recentProjection{}, errors.New("Claude chain selected range changed")
	}
	projection, err := b.projectRecentRange(ctx, scope, source, start, segment.CapturedEnd, digest, false)
	if err != nil {
		return recentProjection{}, err
	}
	projection.Entries = namespaceClaudeProjectedEntries(projection.Entries, segment, segmentIndex == 0)
	// If the selected recent range starts at zero, its digest is already the
	// captured-range evidence. Never recompute an entire transcript merely to
	// populate a manifest field, and never replace evidence established by a
	// preparation job.
	capturedDigest := ""
	if start == 0 {
		capturedDigest = digest
		if segment.CapturedDigest != "" && segment.CapturedDigest != capturedDigest {
			return recentProjection{}, errors.New("Claude chain captured range changed")
		}
	}
	b.mu.Lock()
	if segmentIndex < len(chain.chain.Segments) {
		current := &chain.chain.Segments[segmentIndex]
		if current.RecentDigest != "" && (current.RecentStart != start || current.RecentEnd != segment.CapturedEnd || current.RecentDigest != digest) {
			b.mu.Unlock()
			return recentProjection{}, errors.New("Claude chain selected range changed")
		}
		current.RecentStart = start
		current.RecentEnd = segment.CapturedEnd
		current.RecentDigest = digest
		if capturedDigest != "" {
			if current.CapturedDigest != "" && current.CapturedDigest != capturedDigest {
				b.mu.Unlock()
				return recentProjection{}, errors.New("Claude chain captured range changed")
			}
			current.CapturedDigest = capturedDigest
		}
		chain.diagnostics = mergeBrowseDiagnostics(chain.diagnostics, projection.Diagnostics)
		b.updateClaudeChainLineageEvidenceLocked(chain)
	}
	b.mu.Unlock()
	return projection, nil
}

func (b *Browser) claudeChainSourceFailure(ctx context.Context, err error) BrowsePage {
	if ctx != nil && ctx.Err() != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true))
	}
	if errors.Is(err, errRecentProjectionChanged) || err != nil {
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false))
	}
	return browseFailure(true, "source_unavailable", "The conversation source could not be read.", browseErrorFor("source_unavailable", "The conversation source could not be read.", true))
}

func namespaceClaudeProjectedEntries(entries []projectedEntry, segment claudeSegment, anchor bool) []projectedEntry {
	if anchor || len(entries) == 0 {
		return entries
	}
	hash := sha256.Sum256([]byte(segment.SessionID + "\x00" + segment.FileRevision))
	prefix := hex.EncodeToString(hash[:])[:12]
	for index := range entries {
		entries[index].Entry.ID = prefix + "-" + entries[index].Entry.ID
	}
	return entries
}

func validateClaudeChainSegment(ctx context.Context, segment claudeSegment) error {
	source, err := captureFileSource(segment.Location)
	if err != nil {
		return err
	}
	defer source.close()
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd {
		return errors.New("Claude chain source changed")
	}
	if source.end == segment.CapturedEnd && segment.SourceModTime != 0 && source.info.ModTime().UnixNano() != segment.SourceModTime {
		return errors.New("Claude chain source changed")
	}
	digest, err := fileRangeDigest(ctx, source.file, segment.FooterStart, segment.FooterEnd)
	if err != nil || digest != segment.FooterDigest {
		if err == nil {
			err = errors.New("Claude chain footer changed")
		}
		return err
	}
	if segment.RecentDigest != "" {
		if segment.RecentStart < 0 || segment.RecentEnd < segment.RecentStart || segment.RecentEnd > segment.CapturedEnd {
			return errors.New("Claude chain selected range is invalid")
		}
		digest, err = fileRangeDigest(ctx, source.file, segment.RecentStart, segment.RecentEnd)
		if err != nil || digest != segment.RecentDigest {
			if err == nil {
				err = errors.New("Claude chain selected range changed")
			}
			return err
		}
	}
	if segment.CapturedDigest != "" {
		digest, err = fileRangeDigest(ctx, source.file, 0, segment.CapturedEnd)
		if err != nil || digest != segment.CapturedDigest {
			if err == nil {
				err = errors.New("Claude chain captured range changed")
			}
			return err
		}
	}
	return nil
}

func (b *Browser) claudeChainSegmentPrepared(chain *claudeChainContext, segment int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return segment >= 0 && segment < len(chain.prepared) && chain.prepared[segment]
}

func (b *Browser) claudeChainPreparingPage(scope BrowseScope, _ int, chain *claudeChainContext, segment int, boundary int64, byteBoundary bool) BrowsePage {
	view := b.claudeChainView(chain)
	cursor, err := b.claudeChainCursor(scope, chain, segment, boundary, byteBoundary)
	if err != nil {
		return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
	}
	return BrowsePage{
		Available: true, State: BrowsePreparing, Mode: BrowseSnapshot, Entries: []Entry{},
		SourceRevision: view.publicRevision, SnapshotID: view.snapshotID,
		NextCursor: cursor, HasMore: cursor != "",
		Reason: "Preparing older history on this computer…", Diagnostics: view.diagnostics,
	}
}

func (b *Browser) claudeChainPreparingPageForJob(scope BrowseScope, limit int, chain *claudeChainContext, segment int, boundary int64, byteBoundary bool, job *browseJob) BrowsePage {
	page := b.claudeChainPreparingPage(scope, limit, chain, segment, boundary, byteBoundary)
	job.mu.RLock()
	page.Progress = &BrowseProgress{Phase: job.phase, ScannedBytes: job.scannedBytes, SourceBytes: job.sourceBytes}
	job.mu.RUnlock()
	return page
}

// claudeChainAdmissionFailure keeps the logical chain identity on queue,
// storage, and capacity failures. Without it the frontend sees a generic
// recent-mode error with no snapshot and incorrectly classifies a retryable
// preparation failure as source_changed.
func (b *Browser) claudeChainAdmissionFailure(scope BrowseScope, chain *claudeChainContext, segment int, boundary int64, byteBoundary bool, page BrowsePage) BrowsePage {
	b.mu.Lock()
	if segment >= 0 && segment < len(chain.preparing) {
		chain.preparing[segment] = false
	}
	b.mu.Unlock()
	page.Available = true
	page.Mode = BrowseSnapshot
	page.SourceRevision = b.claudeChainView(chain).publicRevision
	page.SnapshotID = b.claudeChainView(chain).snapshotID
	page.Diagnostics = b.claudeChainView(chain).diagnostics
	page.State = BrowseFailed
	if page.ReasonCode == "" {
		page.ReasonCode = "index_failed"
	}
	if page.Error == nil {
		page.Error = browseErrorFor(page.ReasonCode, page.Reason, true)
	}
	cursor, err := b.claudeChainCursor(scope, chain, segment, boundary, byteBoundary)
	if err == nil {
		page.NextCursor = cursor
		page.HasMore = cursor != ""
	}
	return page
}

func (b *Browser) startClaudeChainJob(scope BrowseScope, chain *claudeChainContext, segmentIndex int) (*browseJob, *BrowsePage) {
	b.mu.Lock()
	if segmentIndex < 0 || segmentIndex >= len(chain.jobs) {
		b.mu.Unlock()
		return nil, nil
	}
	if existing := chain.jobs[segmentIndex]; existing != nil {
		b.mu.Unlock()
		return existing, nil
	}
	segment := chain.chain.Segments[segmentIndex]
	chainID := chain.id
	b.mu.Unlock()

	source, err := captureFileSource(segment.Location)
	if err != nil {
		page := browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false))
		return nil, &page
	}
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd ||
		source.end == segment.CapturedEnd && segment.SourceModTime != 0 && source.info.ModTime().UnixNano() != segment.SourceModTime {
		source.close()
		page := browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false))
		return nil, &page
	}
	// A chain job indexes only the descriptor's captured end. The file may have
	// grown since discovery, but those later records belong to a new latest
	// context and must not leak into this cursor.
	source.end = segment.CapturedEnd
	// Do not hash an arbitrarily large transcript on the request goroutine.
	// The cancellable worker computes and records the captured digest while it
	// builds the per-segment index.
	fullDigest := segment.CapturedDigest
	job, page := b.createJob(scope, source, 0, segment.CapturedEnd, segment.CapturedEnd, fullDigest, false, chainID, segmentIndex)
	if page != nil || job == nil {
		if job == nil {
			source.close()
		}
		return job, page
	}
	job.mu.Lock()
	job.chainContextID = chainID
	job.chainSegment = segmentIndex
	job.chainFooterStart = segment.FooterStart
	job.chainFooterEnd = segment.FooterEnd
	job.chainFooterDigest = segment.FooterDigest
	ownedByJob := job.source.file == source.file
	job.mu.Unlock()
	if !ownedByJob {
		// createJob returned an already registered job. It owns a different
		// source handle; do not leak the handle opened by this request.
		source.close()
	}
	b.mu.Lock()
	if segmentIndex < len(chain.jobs) && chain.jobs[segmentIndex] == nil {
		chain.jobs[segmentIndex] = job
		b.mu.Unlock()
		return job, nil
	}
	b.mu.Unlock()
	return job, nil
}

func (b *Browser) continueClaudeChainJob(ctx context.Context, request BrowseRequest, chain *claudeChainContext, segmentIndex int, boundary int64, byteBoundary bool, job *browseJob) BrowsePage {
	if !b.retainJob(job) {
		return browseFailure(true, "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", true))
	}
	defer b.releaseJob(job)
	job.mu.RLock()
	state, failure := job.state, job.err
	job.mu.RUnlock()
	if state == BrowsePreparing {
		return b.claudeChainPreparingPageForJob(request.Scope, request.Limit, chain, segmentIndex, boundary, byteBoundary, job)
	}
	if state == BrowseFailed {
		if failure == nil {
			failure = browseErrorFor("index_failed", "History preparation failed.", true)
		}
		job.mu.RLock()
		autoRetried := job.autoRetried
		job.mu.RUnlock()
		if failure.Retryable && (request.Retry || !autoRetried) {
			if !request.Retry {
				job.mu.Lock()
				job.autoRetried = true
				job.mu.Unlock()
			}
			if b.retryJob(ctx, job) {
				return b.claudeChainPreparingPageForJob(request.Scope, request.Limit, chain, segmentIndex, boundary, byteBoundary, job)
			}
		}
		view := b.claudeChainView(chain)
		cursor, _ := b.claudeChainCursor(request.Scope, chain, segmentIndex, boundary, byteBoundary)
		return BrowsePage{Available: true, State: BrowseFailed, Mode: BrowseSnapshot,
			SourceRevision: view.publicRevision, SnapshotID: view.snapshotID,
			Diagnostics: view.diagnostics, ReasonCode: failure.Code, Reason: failure.Message,
			Error: failure, NextCursor: cursor, HasMore: cursor != ""}
	}
	b.mu.Lock()
	if segmentIndex >= 0 && segmentIndex < len(chain.prepared) {
		chain.preparing[segmentIndex] = false
		chain.prepared[segmentIndex] = true
	}
	b.mu.Unlock()
	// The worker established the full captured digest in the background. Keep
	// that evidence in the shared lineage before serving the first page.
	b.mu.Lock()
	b.updateClaudeChainLineageEvidenceLocked(chain)
	b.mu.Unlock()
	return b.claudeChainPreparedPageAt(ctx, request.Scope, request.Limit, chain, segmentIndex, boundary, request.Retry)
}

func (b *Browser) claudeChainPreparedPageAt(ctx context.Context, scope BrowseScope, limit int, chain *claudeChainContext, segmentIndex int, boundary int64, retry bool) BrowsePage {
	b.mu.Lock()
	if segmentIndex < 0 || segmentIndex >= len(chain.jobs) {
		b.mu.Unlock()
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false))
	}
	job := chain.jobs[segmentIndex]
	b.mu.Unlock()
	if job == nil {
		recent, err := b.loadClaudeChainRecent(ctx, scope, chain, segmentIndex)
		if err != nil {
			return b.claudeChainSourceFailure(ctx, err)
		}
		return b.claudeChainPageAt(ctx, scope, limit, chain, false, segmentIndex, len(recent.Entries), recent.Entries, nil)
	}
	if !b.retainJob(job) {
		return browseFailure(true, "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", true))
	}
	defer b.releaseJob(job)
	job.mu.RLock()
	state := job.state
	index := job.index
	source := job.source
	diagnostics := job.diagnostics
	job.mu.RUnlock()
	if state != BrowseReady || index == nil {
		if state == BrowseFailed {
			return b.continueClaudeChainJob(ctx, BrowseRequest{Scope: scope, Limit: limit, Retry: retry}, chain, segmentIndex, boundary, true, job)
		}
		return b.claudeChainPreparingPageForJob(scope, limit, chain, segmentIndex, boundary, true, job)
	}
	if err := b.validateSnapshotJob(ctx, job, source); err != nil {
		if ctx.Err() != nil {
			return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true))
		}
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false))
	}
	entries, hasMore, err := index.entriesBefore(boundary, limit)
	if err != nil {
		return browseFailure(true, "index_failed", "The prepared conversation history could not be read.", browseErrorFor("index_failed", "The prepared conversation history could not be read.", true))
	}
	view := b.claudeChainView(chain)
	if segmentIndex < 0 || segmentIndex >= len(view.chain.Segments) {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false))
	}
	entries = namespaceClaudeProjectedEntries(entries, view.chain.Segments[segmentIndex], segmentIndex == 0)
	base := BrowsePage{Available: true, State: BrowseReady, Mode: BrowseSnapshot,
		SourceRevision: view.publicRevision, SnapshotID: view.snapshotID, Entries: []Entry{}, Total: nil,
		Diagnostics: mergeBrowseDiagnostics(view.diagnostics, diagnostics)}
	selected, omitted := b.fitProjected(base, entries, func(nextBoundary int64) (string, error) {
		return b.claudeChainCursor(scope, chain, segmentIndex, nextBoundary, true)
	})
	toolsOmitted, payloadsOmitted := browseOmissions(entries, selected)
	base.Diagnostics.OmittedTools += toolsOmitted
	base.Diagnostics.OmittedPayloads += payloadsOmitted
	base.Entries = projectedEntries(selected)
	if omitted > 0 || hasMore {
		base.HasMore = true
		if len(selected) > 0 {
			base.NextCursor, err = b.claudeChainCursor(scope, chain, segmentIndex, selected[0].Offset, true)
		} else {
			base.NextCursor, err = b.claudeChainCursor(scope, chain, segmentIndex, boundary, true)
		}
		if err != nil {
			return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
		}
	} else {
		nextSegment, nextBoundary, hasNext, nextIsOffset, nextErr := b.claudeChainOlderPosition(ctx, scope, chain, segmentIndex, 0, nil)
		if nextErr != nil {
			return b.claudeChainSourceFailure(ctx, nextErr)
		}
		if hasNext {
			base.NextCursor, err = b.claudeChainCursor(scope, chain, nextSegment, nextBoundary, nextIsOffset)
			if err != nil {
				return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
			}
			base.HasMore = true
		}
	}
	return b.enforcePageBudget(base)
}

func mergeBrowseDiagnostics(left, right BrowseDiagnostics) BrowseDiagnostics {
	left.OversizedRecords = maxInt(left.OversizedRecords, right.OversizedRecords)
	left.CorruptRecords = maxInt(left.CorruptRecords, right.CorruptRecords)
	left.OmittedTools = maxInt(left.OmittedTools, right.OmittedTools)
	left.OmittedPayloads = maxInt(left.OmittedPayloads, right.OmittedPayloads)
	left.PlanCorrupt = left.PlanCorrupt || right.PlanCorrupt
	left.SourceTruncated = left.SourceTruncated || right.SourceTruncated
	left.ContinuationIncomplete = left.ContinuationIncomplete || right.ContinuationIncomplete
	if left.ContinuationReason == "" {
		left.ContinuationReason = right.ContinuationReason
	}
	return left
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func (b *Browser) claudeChainLatestPage(ctx context.Context, scope BrowseScope, limit int, chain *claudeChainContext) BrowsePage {
	if chain == nil {
		return browseFailure(true, "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", true))
	}
	budget := &claudeChainRecentBudget{remaining: b.options.RecentBytes}
	view := b.claudeChainView(chain)
	for index := len(view.chain.Segments) - 1; index >= 0; index-- {
		view = b.claudeChainView(chain)
		if index >= len(view.chain.Segments) {
			continue
		}
		if index < len(view.prepared) && view.prepared[index] {
			// Once a clipped segment has been prepared, latest must use the
			// byte-indexed snapshot. Returning to the tail projection here would
			// create an entry-count cursor that jumps over the prepared prefix.
			return b.claudeChainPreparedPageAt(ctx, scope, limit, chain, index, view.chain.Segments[index].CapturedEnd, false)
		}
		recent, err := b.loadClaudeChainRecentWithBudget(ctx, scope, chain, index, budget)
		if errors.Is(err, errClaudeChainRecentBudget) {
			return b.claudeChainBudgetPage(scope, chain, index)
		}
		if err != nil {
			return b.claudeChainSourceFailure(ctx, err)
		}
		if len(recent.Entries) > 0 {
			return b.claudeChainPageAt(ctx, scope, limit, chain, true, index, len(recent.Entries), recent.Entries, budget)
		}
		view = b.claudeChainView(chain)
		if index < len(view.recentComplete) && !view.recentComplete[index] && !view.prepared[index] {
			segment := view.chain.Segments[index]
			page := BrowsePage{Available: true, State: BrowseReady, Mode: BrowseRecent,
				Entries: []Entry{}, SourceRevision: view.publicRevision, Diagnostics: mergeBrowseDiagnostics(view.diagnostics, recent.Diagnostics)}
			cursor, err := b.claudeChainCursor(scope, chain, index, segment.RecentStart, true)
			if err != nil {
				return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
			}
			page.NextCursor, page.HasMore = cursor, cursor != ""
			return page
		}
	}
	view = b.claudeChainView(chain)
	return BrowsePage{Available: true, State: BrowseReady, Mode: BrowseRecent, Entries: []Entry{}, Total: nil, Diagnostics: view.diagnostics}
}

func (b *Browser) claudeChainBudgetPage(scope BrowseScope, chain *claudeChainContext, segmentIndex int) BrowsePage {
	view := b.claudeChainView(chain)
	if segmentIndex < 0 || segmentIndex >= len(view.chain.Segments) {
		return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
	}
	segment := view.chain.Segments[segmentIndex]
	boundary := segment.CapturedEnd
	if segment.RecentDigest != "" && segment.RecentStart >= 0 {
		boundary = segment.RecentStart
	}
	cursor, err := b.claudeChainCursor(scope, chain, segmentIndex, boundary, true)
	if err != nil {
		return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
	}
	return BrowsePage{Available: true, State: BrowseReady, Mode: BrowseRecent, Entries: []Entry{},
		SourceRevision: view.publicRevision, NextCursor: cursor, HasMore: cursor != "", Diagnostics: view.diagnostics,
		Reason: "Older history can be prepared on this computer…"}
}

func (b *Browser) claudeChainPageAt(ctx context.Context, scope BrowseScope, limit int, chain *claudeChainContext, latest bool, segmentIndex, boundary int, entries []projectedEntry, budget *claudeChainRecentBudget) BrowsePage {
	view := b.claudeChainView(chain)
	if chain == nil || segmentIndex < 0 || segmentIndex >= len(view.chain.Segments) {
		return browseFailure(true, "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", true))
	}
	if boundary < 0 {
		boundary = len(entries)
	}
	if boundary > len(entries) {
		boundary = len(entries)
	}
	start := boundary - limit
	if start < 0 {
		start = 0
	}
	base := BrowsePage{Available: true, State: BrowseReady, Mode: BrowseSnapshot, Entries: []Entry{},
		SourceRevision: view.publicRevision, SnapshotID: view.snapshotID,
		Diagnostics: mergeBrowseDiagnostics(view.diagnostics, BrowseDiagnostics{}), Total: nil}
	if latest {
		base.Mode = BrowseRecent
		base.SnapshotID = ""
	}
	for {
		selected := entries[start:boundary]
		nextSegment, nextBoundary, hasNext, nextIsOffset, nextErr := b.claudeChainOlderPosition(ctx, scope, chain, segmentIndex, start, budget)
		page := base
		page.Entries = projectedEntries(selected)
		if hasNext && (nextErr == nil || errors.Is(nextErr, errClaudeChainRecentBudget)) {
			cursor, cursorErr := b.claudeChainCursor(scope, chain, nextSegment, nextBoundary, nextIsOffset)
			if cursorErr == nil {
				page.NextCursor = cursor
				page.HasMore = true
			}
		}
		if nextErr != nil && !errors.Is(nextErr, errClaudeChainRecentBudget) {
			return b.claudeChainSourceFailure(ctx, nextErr)
		}
		if !hasNext {
			page.HasMore = false
			page.NextCursor = ""
		}
		if b.pageSize(page) <= b.options.ResponseBytes || start >= boundary-1 {
			if b.pageSize(page) > b.options.ResponseBytes && len(page.Entries) == 1 {
				page.Entries[0] = boundBrowseEntry(page.Entries[0], b.options.ResponseBytes)
			}
			return b.enforcePageBudget(page)
		}
		start++
	}
}

func (b *Browser) claudeChainOlderPosition(ctx context.Context, scope BrowseScope, chain *claudeChainContext, segmentIndex, start int, budget *claudeChainRecentBudget) (int, int64, bool, bool, error) {
	view := b.claudeChainView(chain)
	if segmentIndex < 0 || segmentIndex >= len(view.chain.Segments) {
		return 0, 0, false, false, errors.New("invalid Claude chain segment")
	}
	if start > 0 {
		return segmentIndex, int64(start), true, false, nil
	}
	if !view.prepared[segmentIndex] && !view.recentComplete[segmentIndex] {
		// The current segment was only read from its bounded tail. Do not jump
		// over its unscanned prefix: offer a same-segment preparation cursor at
		// the first byte represented by the recent page.
		return segmentIndex, view.chain.Segments[segmentIndex].RecentStart, true, true, nil
	}
	for index := segmentIndex - 1; index >= 0; index-- {
		view = b.claudeChainView(chain)
		if view.prepared[index] {
			return index, view.chain.Segments[index].CapturedEnd, true, true, nil
		}
		if !view.recentComplete[index] {
			return index, view.chain.Segments[index].CapturedEnd, true, true, nil
		}
		recent, err := b.loadClaudeChainRecentWithBudget(ctx, scope, chain, index, budget)
		if errors.Is(err, errClaudeChainRecentBudget) {
			return index, view.chain.Segments[index].CapturedEnd, true, true, err
		}
		if err != nil {
			return 0, 0, false, false, err
		}
		if len(recent.Entries) > 0 {
			return index, int64(len(recent.Entries)), true, false, nil
		}
	}
	return 0, 0, false, false, nil
}

func (b *Browser) claudeChainCursor(scope BrowseScope, chain *claudeChainContext, segment int, boundary int64, byteBoundary bool) (string, error) {
	view := b.claudeChainView(chain)
	if chain == nil || segment < 0 || segment >= len(view.chain.Segments) {
		return "", errors.New("invalid Claude chain cursor")
	}
	segmentIndex := segment
	return encodeBrowseCursor(b.key, browseCursor{
		Mode: "chain", Scope: browseScopeID(scope), Revision: view.publicRevision,
		SnapshotID: view.snapshotID, ChainID: view.id, Segment: &segmentIndex,
		ChainOffset: byteBoundary, Boundary: browseOffset(boundary), ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
	})
}
