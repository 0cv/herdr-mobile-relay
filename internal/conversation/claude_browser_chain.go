package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

const (
	maxClaudeChainContexts       = 256
	maxClaudeChainEvidenceRanges = 16
)

var (
	errClaudeChainLineageChanged   = errors.New("Claude continuation lineage changed")
	errClaudeChainDiscoveryBudget  = errors.New("Claude continuation discovery budget exhausted")
	errClaudeChainForegroundBudget = errors.New("Claude continuation foreground budget exhausted")
	errClaudeChainValidationStale  = errors.New("Claude continuation validation became stale")
)

type claudeChainLineage struct {
	segments   []claudeSegment
	lastAccess time.Time
}

func cloneClaudeSegments(segments []claudeSegment) []claudeSegment {
	cloned := make([]claudeSegment, len(segments))
	for index, segment := range segments {
		cloned[index] = segment
		cloned[index].ObservedRanges = append([]claudeRangeEvidence(nil), segment.ObservedRanges...)
		cloned[index].EvidenceCompactionRanges = append([]claudeRangeEvidence(nil), segment.EvidenceCompactionRanges...)
	}
	return cloned
}

type claudeChainLineageIdentity struct {
	id         string
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
	requestBudget := newClaudeChainRequestBudget(b.options.RecentBytes)
	chain, err := resolveClaudeChainWithReadHooks(ctx, anchor, request.Scope.SessionID,
		func(bytes int64) error {
			if err := requestBudget.takeDiscovery(bytes); err != nil {
				return err
			}
			return nil
		},
		func(bytes int64) error {
			b.observeDiscoveryRead(bytes)
			return nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			message := "History request was cancelled."
			return browseFailure(true, "request_cancelled", message, browseErrorFor("request_cancelled", message, true)), nil
		}
		return browseUnavailable("source_unavailable", "The conversation source could not be read."), nil
	}
	if len(chain.Segments) == 0 {
		return browseUnavailable("source_unavailable", "The conversation source could not be read."), nil
	}
	if len(chain.Segments) == 1 && !chain.incomplete() {
		return b.readRecentFilePage(ctx, request, false)
	}
	chainContext, err := b.acquireClaudeChainContext(ctx, request.Scope, chain, requestBudget.validation)
	if err != nil {
		if errors.Is(err, errClaudeChainCapacity) {
			message := "Conversation history browsing is busy; try again shortly."
			return browseFailure(true, "index_capacity_exceeded", message, browseErrorFor("index_capacity_exceeded", message, true)), nil
		}
		if errors.Is(err, errClaudeChainValidationPending) || errors.Is(err, errClaudeChainValidationStale) {
			message := "Conversation history is being validated; try again shortly."
			return browseFailure(true, "validation_pending", message, browseErrorFor("validation_pending", message, true)), nil
		}
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			message := "History request was cancelled."
			return browseFailure(true, "request_cancelled", message, browseErrorFor("request_cancelled", message, true)), nil
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
	// The digests authenticate the resolver's candidate, not any later file
	// state. Fence both admission and the latest projection with fresh metadata
	// so a concurrent append requests rediscovery rather than blessing a changed
	// prefix or retiring an otherwise healthy public lineage.
	for _, segment := range chain.Segments {
		if err := b.revalidateClaudeChainCandidate(ctx, segment); err != nil {
			return b.claudeChainLatestValidationFailure(ctx, err), nil
		}
	}
	page := b.claudeChainLatestPage(ctx, request.Scope, request.Limit, chainContext, requestBudget)
	for _, segment := range chain.Segments {
		if err := b.revalidateClaudeChainCandidate(ctx, segment); err != nil {
			return b.claudeChainLatestValidationFailure(ctx, err), nil
		}
	}
	return page, nil
}

func (b *Browser) readClaudeChainCursorPage(ctx context.Context, request BrowseRequest, cursor browseCursor) (BrowsePage, error) {
	if cursor.ChainID == "" || cursor.Segment == nil || *cursor.Segment < 0 || cursor.SnapshotID == "" {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
	}
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
	}
	requestBudget := newClaudeChainRequestBudget(b.options.RecentBytes)
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
	b.mu.Lock()
	lineage, hasLineage := b.chainLineage[chainContext.scopeID]
	lineagePending := hasLineage && segmentIndex < len(lineage.segments) && lineage.segments[segmentIndex].EvidenceCompactionPending
	lineageFailed := hasLineage && segmentIndex < len(lineage.segments) && lineage.segments[segmentIndex].EvidenceCompactionError != ""
	b.mu.Unlock()
	if lineagePending {
		return b.claudeChainValidationPendingPage(request.Scope, chainContext, request.Cursor), nil
	}
	if lineageFailed {
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	preparedSegment := segmentIndex < len(view.prepared) && view.prepared[segmentIndex]
	if !preparedSegment {
		if err := b.validateClaudeChainSegment(ctx, segment, requestBudget.validation); err != nil {
			if ctx.Err() != nil {
				return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
			}
			if errors.Is(err, errClaudeChainForegroundBudget) {
				// The bounded recent projection below or the preparation worker
				// authenticates the selected range before serving it. Do not turn a
				// second range that cannot fit in this request's foreground allowance
				// into a status page with no owner: retrying that page would repeat
				// the same footer-plus-range reservation forever.
			} else if errors.Is(err, errClaudeChainValidationPending) {
				return b.claudeChainValidationPendingPage(request.Scope, chainContext, request.Cursor), nil
			} else {
				return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
			}
		}
	}
	if preparedSegment && ctx.Err() != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
	}
	var recent recentProjection
	recentTailExhausted := false
	if !cursor.ChainOffset {
		view = b.claudeChainView(chainContext)
		recentTailExhausted = segmentIndex < len(view.recentComplete) && !view.recentComplete[segmentIndex]
		recent, err = b.loadClaudeChainRecentWithBudget(ctx, request.Scope, chainContext, segmentIndex, requestBudget)
		if errors.Is(err, errClaudeChainRecentBudget) {
			return b.claudeChainBudgetPage(request.Scope, chainContext, segmentIndex), nil
		}
		if err != nil {
			return b.claudeChainSourceFailure(ctx, err), nil
		}
		if boundary > int64(len(recent.Entries)) {
			return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)), nil
		}
	} else if boundary > segment.CapturedEnd {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)), nil
	}
	if !cursor.ChainOffset && b.claudeChainSegmentPrepared(chainContext, segmentIndex) {
		// Older cursors issued by a recent page use an entry-count boundary.
		// If another request prepared this segment in the meantime, translate
		// that saved tail boundary into the frozen index instead of exhausting
		// the tail a second time and jumping over its prefix.
		byteBoundary := claudeChainRecentBoundary(recent.Entries, boundary, segment.CapturedEnd,
			recentTailExhausted, segment.RecentStart)
		return b.claudeChainPreparedPageAt(ctx, request.Scope, request.Limit, chainContext, segmentIndex, byteBoundary, request.Retry, requestBudget), nil
	}
	if !b.claudeChainSegmentPrepared(chainContext, segmentIndex) && cursor.ChainOffset {
		b.mu.Lock()
		alreadyPreparing := chainContext.preparing[segmentIndex]
		recentComplete := chainContext.recentComplete[segmentIndex]
		recentTailExhausted = !recentComplete
		if !alreadyPreparing && recentComplete {
			// A byte cursor into a segment that fits the recent window is already
			// directly readable. Do not manufacture a preparation round merely to
			// translate an older count-based cursor.
			b.mu.Unlock()
		} else if !alreadyPreparing {
			chainContext.preparing[segmentIndex] = true
			b.mu.Unlock()
			return b.claudeChainPreparingPage(request.Scope, request.Limit, chainContext, segmentIndex, boundary, true), nil
		} else {
			job := chainContext.jobs[segmentIndex]
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
					job, page = b.startClaudeChainJob(request.Scope, chainContext, segmentIndex, budgetForeground(requestBudget))
					if page != nil {
						return b.claudeChainAdmissionFailure(request.Scope, chainContext, segmentIndex, boundary, true, *page), nil
					}
					if job == nil {
						page := browseFailure(true, "index_failed", "History preparation could not be started.", browseErrorFor("index_failed", "History preparation could not be started.", true))
						return b.claudeChainAdmissionFailure(request.Scope, chainContext, segmentIndex, boundary, true, page), nil
					}
				}
				return b.continueClaudeChainJob(ctx, request, chainContext, segmentIndex, boundary, true, job, requestBudget), nil
			}
		}
	}
	if cursor.ChainOffset {
		if b.claudeChainSegmentPrepared(chainContext, segmentIndex) {
			return b.claudeChainPreparedPageAt(ctx, request.Scope, request.Limit, chainContext, segmentIndex, boundary, request.Retry, requestBudget), nil
		}
		recent, recentErr := b.loadClaudeChainRecentWithBudget(ctx, request.Scope, chainContext, segmentIndex, requestBudget)
		if errors.Is(recentErr, errClaudeChainRecentBudget) {
			return b.claudeChainBudgetPage(request.Scope, chainContext, segmentIndex), nil
		}
		if recentErr != nil {
			return b.claudeChainSourceFailure(ctx, recentErr), nil
		}
		byteBoundary := 0
		for _, entry := range recent.Entries {
			if entry.Offset < boundary {
				byteBoundary++
			}
		}
		return b.claudeChainPageAt(ctx, request.Scope, request.Limit, chainContext, false, segmentIndex, byteBoundary, recent.Entries, requestBudget, recentTailExhausted), nil
	}
	return b.claudeChainPageAt(ctx, request.Scope, request.Limit, chainContext, false, segmentIndex, int(boundary), recent.Entries, requestBudget, recentTailExhausted), nil
}

var (
	errClaudeChainCapacity          = errors.New("Claude chain context capacity is occupied")
	errClaudeChainRecentBudget      = errors.New("Claude chain recent read budget exhausted")
	errClaudeChainValidationPending = errors.New("Claude chain source validation is pending")
)

// claudeChainRequestBudget is created once for each foreground request. The
// recent allowance covers projected source bytes and the validation allowance
// is shared by every established range examined by lineage/cursor checks; no
// segment gets a fresh 128 KiB grant while a request crosses a chain boundary.
type claudeChainRequestBudget struct {
	recent     *claudeChainRecentBudget
	validation *claudeChainForegroundBudget
	discovery  int64
}

type claudeChainForegroundBudget struct {
	validationRemaining int64
	physicalRemaining   int64
}

const claudeChainForegroundPhysicalBytes = claudeChainForegroundValidationBytes + int64(claudeContinuationMaxSegments+1)*claudeContinuationFooterBytes

func budgetForeground(budget *claudeChainRequestBudget) *claudeChainForegroundBudget {
	if budget == nil {
		return nil
	}
	return budget.validation
}

func newClaudeChainRequestBudget(recentBytes int64) *claudeChainRequestBudget {
	return &claudeChainRequestBudget{
		recent: &claudeChainRecentBudget{remaining: recentBytes},
		validation: &claudeChainForegroundBudget{
			validationRemaining: claudeChainForegroundValidationBytes,
			// A chain may authenticate one bounded range per segment before it
			// reaches the worker. Reserve one 64 KiB source anchor for each
			// bounded segment plus the final latest projection anchor.
			physicalRemaining: claudeChainForegroundPhysicalBytes,
		},
		// Discovery reads one bounded source anchor, one footer, and at most
		// one boundary byte per segment. Keep the allowance explicit so the
		// post-read accounting hook cannot report a budget overshoot on the
		// final boundary check.
		discovery: claudeContinuationFooterBudget + int64(claudeContinuationMaxSegments)*(claudeContinuationFooterBytes+1),
	}
}

func (budget *claudeChainForegroundBudget) takeValidation(bytes int64) bool {
	if budget == nil || bytes < 0 || bytes > budget.validationRemaining {
		return false
	}
	budget.validationRemaining -= bytes
	return true
}

func (budget *claudeChainForegroundBudget) takePhysical(bytes int64) bool {
	if budget == nil {
		return true
	}
	if bytes < 0 || bytes > budget.physicalRemaining {
		return false
	}
	budget.physicalRemaining -= bytes
	return true
}

func (budget *claudeChainRequestBudget) takeDiscovery(bytes int64) error {
	if budget == nil || bytes < 0 || bytes > budget.discovery {
		return errClaudeChainDiscoveryBudget
	}
	budget.discovery -= bytes
	return nil
}

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

func (b *Browser) acquireClaudeChainContext(ctx context.Context, scope BrowseScope, chain claudeChain, foreground ...*claudeChainForegroundBudget) (*claudeChainContext, error) {
	key := claudeChainKey(scope, chain)
	var foregroundBudget *claudeChainForegroundBudget
	if len(foreground) > 0 {
		foregroundBudget = foreground[0]
	}
	if foregroundBudget == nil {
		foregroundBudget = &claudeChainForegroundBudget{validationRemaining: claudeChainForegroundValidationBytes, physicalRemaining: claudeChainForegroundPhysicalBytes}
	}
	scopeID := browseScopeID(scope)
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, errors.New("conversation browser is closed")
	}
	lineage, hasLineage := b.chainLineage[scopeID]
	lineageIdentity := b.chainLineageIdentity[scopeID].id
	if hasLineage {
		b.schedulePendingClaudeChainCompactionsLocked(scopeID, &lineage)
	}
	lineage.segments = cloneClaudeSegments(lineage.segments)
	b.mu.Unlock()
	if hasLineage {
		compatible, compatibilityErr := b.claudeChainLineageCompatible(ctx, lineage.segments, chain.Segments, foregroundBudget)
		if !compatible {
			if compatibilityErr == nil {
				compatibilityErr = errClaudeChainLineageChanged
			}
			return nil, compatibilityErr
		}
	}
	if b.chainContextAdmissionObserver != nil {
		b.chainContextAdmissionObserver(chain, false)
	}

	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, errors.New("conversation browser is closed")
	}
	if !b.claudeChainLineageUnchangedLocked(scopeID, lineage, hasLineage, lineageIdentity) {
		b.mu.Unlock()
		return nil, errClaudeChainValidationStale
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
	if b.chainContextAdmissionObserver != nil {
		b.chainContextAdmissionObserver(chain, true)
	}
	b.mu.Lock()
	if b.chainReservations > 0 {
		b.chainReservations--
	}
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	// Building the manifest released the mutex again. Authenticate publication
	// against the SAME complete lineage generation checked above, before even
	// reusing a context. Otherwise an older latest request can overwrite newer
	// obligations and only report stale after damaging the shared ledger.
	if !b.claudeChainLineageUnchangedLocked(scopeID, lineage, hasLineage, lineageIdentity) {
		b.mu.Unlock()
		return nil, errClaudeChainValidationStale
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
	// Discovery/validation ran outside the mutex. A competing request may
	// since have started compaction. Backpressure new manifests at publication
	// too, leaving capacity for obligations from already admitted contexts and
	// preparations (at most maxClaudeChainContexts fixed descriptors).
	if lineage, ok := b.chainLineage[scopeID]; ok {
		for _, segment := range lineage.segments {
			if segment.EvidenceCompactionPending {
				b.mu.Unlock()
				return nil, errClaudeChainValidationPending
			}
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

// Compatibility checks authenticate a copied lineage outside Browser.mu.
// Compare the full descriptor/evidence union, not only captured ends: an older
// saved preparation can publish a new obligation at the same end. Identity also
// fences reset/recreation. Duplicate evidence and last-access updates are inert.
// All comparisons are bounded metadata work; no file I/O runs under the mutex.
func (b *Browser) claudeChainLineageUnchangedLocked(scopeID string, previous claudeChainLineage, hadPrevious bool, identity string) bool {
	current, hasCurrent := b.chainLineage[scopeID]
	if hasCurrent != hadPrevious || b.chainLineageIdentity[scopeID].id != identity || len(current.segments) != len(previous.segments) {
		return false
	}
	for index, old := range previous.segments {
		segment := current.segments[index]
		if old.EvidenceCompactionPending != segment.EvidenceCompactionPending || old.EvidenceCompactionError != segment.EvidenceCompactionError ||
			claudeChainEvidenceGeneration(old) != claudeChainEvidenceGeneration(segment) {
			return false
		}
	}
	return true
}

const (
	claudeChainForegroundValidationBytes = int64(128 * 1024)
	// Lineage validation is not a second preparation queue. A small fixed
	// number of browser-owned jobs may authenticate established large ranges;
	// callers that arrive while those slots are occupied receive a retryable
	// pending result instead of doing an unbounded scan themselves.
	maxClaudeChainValidationTasks   = 2
	maxClaudeChainValidationResults = maxClaudeChainContexts * 2
)

type claudeChainValidation struct {
	done       chan struct{}
	running    bool
	err        error
	lastAccess time.Time
}

type claudeChainEvidenceCompaction struct {
	key       string
	scopeID   string
	segment   int
	candidate claudeSegment
	start     int64
	end       int64
	digest    string
	evidence  []claudeRangeEvidence
	// Content-addressed generation of the COMPLETE descriptor/evidence union.
	// Exact duplicate publication does not change it; a new obligation does.
	generation string
}

func (b *Browser) claudeChainLineageCompatible(ctx context.Context, previous, current []claudeSegment, foreground ...*claudeChainForegroundBudget) (bool, error) {
	var foregroundBudget *claudeChainForegroundBudget
	if len(foreground) > 0 {
		foregroundBudget = foreground[0]
	}
	if foregroundBudget == nil {
		foregroundBudget = &claudeChainForegroundBudget{validationRemaining: claudeChainForegroundValidationBytes, physicalRemaining: claudeChainForegroundPhysicalBytes}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for index, old := range previous {
		if old.EvidenceCompactionPending {
			return false, errClaudeChainValidationPending
		}
		if old.EvidenceCompactionError != "" {
			return false, errClaudeChainLineageChanged
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if index >= len(current) {
			// A temporarily missing descendant is allowed to recover, but retain
			// the longer lineage so a replacement cannot hide behind the gap.
			return true, nil
		}
		candidate := current[index]
		if old.SessionID != candidate.SessionID || old.Location.Path != candidate.Location.Path ||
			old.FileIdentity != candidate.FileIdentity || candidate.CapturedEnd < old.CapturedEnd {
			return false, errClaudeChainLineageChanged
		}
		if candidate.CapturedEnd == old.CapturedEnd {
			if old.FileRevision != candidate.FileRevision || old.SourceModTime != candidate.SourceModTime ||
				old.SourceChangeToken != "" && candidate.SourceChangeToken != old.SourceChangeToken {
				return false, errClaudeChainLineageChanged
			}
			if old.FooterDigest != "" && old.FooterDigest != candidate.FooterDigest {
				return false, errClaudeChainLineageChanged
			}
			if old.CapturedDigest != "" && candidate.CapturedDigest != "" && old.CapturedDigest != candidate.CapturedDigest {
				return false, errClaudeChainLineageChanged
			}
			// Equal captured ends are authenticated by descriptor metadata and
			// footer evidence. Large established ranges are not reread on every
			// ordinary latest poll; they are rechecked when the end grows.
			continue
		}
		// A longer descriptor is append-compatible only when every immutable
		// range observed by the prior context still matches. Small evidence is
		// checked in a bounded foreground allowance. Larger or numerous ranges
		// are handed to the deduplicated browser-owned validator and remain
		// pending until that validator has authenticated them.
		if err := b.claudeChainObservedRangesMatch(ctx, old, candidate, foregroundBudget); err != nil {
			return false, err
		}
	}
	return true, nil
}

func claudeChainEvidence(segment claudeSegment) []claudeRangeEvidence {
	evidence := claudeEvidenceWithinEnd(segment.ObservedRanges, segment.CapturedEnd)
	// Pending ranges remain obligations, including for overlapping saved jobs
	// and retention accounting; moving them into a task never authenticates them.
	evidence = append(evidence, claudeEvidenceWithinEnd(segment.EvidenceCompactionRanges, segment.CapturedEnd)...)
	if segment.FooterDigest != "" && segment.FooterEnd > segment.FooterStart && segment.FooterEnd <= segment.CapturedEnd {
		evidence = append(evidence, claudeRangeEvidence{Start: segment.FooterStart, End: segment.FooterEnd, Digest: segment.FooterDigest})
	}
	if segment.RecentDigest != "" && segment.RecentEnd > segment.RecentStart && segment.RecentEnd <= segment.CapturedEnd {
		evidence = append(evidence, claudeRangeEvidence{Start: segment.RecentStart, End: segment.RecentEnd, Digest: segment.RecentDigest})
	}
	if segment.CapturedDigest != "" && segment.CapturedEnd > 0 {
		// CapturedDigest is implicitly [0, CapturedEnd]. Put the explicit end
		// in the immutable ledger before a later append changes that field.
		evidence = append(evidence, claudeRangeEvidence{Start: 0, End: segment.CapturedEnd, Digest: segment.CapturedDigest})
	}
	return uniqueClaudeRangeEvidence(evidence)
}

func claudeEvidenceWithinEnd(evidence []claudeRangeEvidence, end int64) []claudeRangeEvidence {
	filtered := make([]claudeRangeEvidence, 0, len(evidence))
	for _, item := range evidence {
		if item.Start < 0 || item.End <= item.Start || item.End > end || item.Digest == "" {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func claudeChainValidationKey(candidate claudeSegment, evidence []claudeRangeEvidence) string {
	hash := sha256.New()
	// Every descriptor field that can change while a deferred result is in
	// flight is bound into the key. In particular, FileRevision intentionally
	// permits append growth, so SourceModTime and all footer/range metadata must
	// also participate in cache identity.
	for _, value := range []string{
		candidate.SessionID, candidate.Location.Root, candidate.Location.Path,
		candidate.FileIdentity, candidate.FileRevision, candidate.SourceChangeToken,
		browseOffset(candidate.SourceModTime), browseOffset(candidate.CapturedEnd),
		browseOffset(candidate.RecentStart), browseOffset(candidate.RecentEnd), candidate.RecentDigest,
		browseOffset(candidate.FooterStart), browseOffset(candidate.FooterEnd), candidate.FooterDigest,
		candidate.CapturedDigest,
	} {
		_, _ = io.WriteString(hash, value)
		_, _ = io.WriteString(hash, "\x00")
	}
	for _, item := range evidence {
		_, _ = io.WriteString(hash, fmt.Sprintf("%d:%d:%s", item.Start, item.End, item.Digest))
		_, _ = io.WriteString(hash, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (b *Browser) claudeChainObservedRangesMatch(ctx context.Context, old, candidate claudeSegment, foreground ...*claudeChainForegroundBudget) error {
	if old.CapturedEnd < 0 || candidate.CapturedEnd < old.CapturedEnd {
		return errClaudeChainLineageChanged
	}
	var foregroundBudget *claudeChainForegroundBudget
	if len(foreground) > 0 {
		foregroundBudget = foreground[0]
	}
	if foregroundBudget == nil {
		foregroundBudget = &claudeChainForegroundBudget{validationRemaining: claudeChainForegroundValidationBytes, physicalRemaining: claudeChainForegroundPhysicalBytes}
	}
	evidence := claudeChainEvidence(old)
	if len(evidence) == 0 {
		return nil
	}
	var total int64
	for _, item := range evidence {
		if item.Start < 0 || item.End <= item.Start || item.End > old.CapturedEnd {
			return errClaudeChainLineageChanged
		}
		total += item.End - item.Start
		if total < 0 { // overflow is not a valid bounded request
			return errClaudeChainLineageChanged
		}
	}
	// This allowance is shared across the complete lineage loop. A chain with
	// many individually small observed ranges therefore defers as one request,
	// rather than receiving a fresh per-segment foreground scan.
	if !foregroundBudget.takeValidation(total) {
		return b.deferClaudeChainValidation(ctx, candidate, old.FileIdentity, evidence, foregroundBudget)
	}
	source, err := captureFileSourceWithReserve(candidate.Location, reserveClaudeChainPhysical(foregroundBudget), b.observeValidationRead)
	if err != nil {
		if errors.Is(err, errClaudeChainForegroundBudget) {
			return b.deferClaudeChainValidation(ctx, candidate, old.FileIdentity, evidence, foregroundBudget)
		}
		return errClaudeChainLineageChanged
	}
	defer source.close()
	if source.end < old.CapturedEnd || source.end < candidate.CapturedEnd ||
		old.FileIdentity != "" && fileIdentity(source.info) != old.FileIdentity ||
		candidate.FileIdentity != "" && fileIdentity(source.info) != candidate.FileIdentity ||
		source.revision != candidate.FileRevision {
		return errClaudeChainLineageChanged
	}
	// A token changes on ordinary append too. An open newer than discovery is
	// not the candidate we can authenticate; rediscover before considering the
	// append-sensitive metadata. The retry must still check every old digest.
	if source.end > candidate.CapturedEnd {
		return errClaudeChainValidationStale
	}
	if candidate.SourceChangeToken != "" && fileChangeToken(source.info) != candidate.SourceChangeToken {
		return errClaudeChainLineageChanged
	}
	for _, item := range evidence {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest, digestErr := b.digestClaudeChainValidationRangeObserved(ctx, source.file, item.Start, item.End, foregroundBudget)
		if errors.Is(digestErr, errClaudeChainForegroundBudget) {
			return b.deferClaudeChainValidation(ctx, candidate, old.FileIdentity, evidence, foregroundBudget)
		}
		if digestErr != nil {
			return digestErr
		}
		if digest != item.Digest {
			return errClaudeChainLineageChanged
		}
	}
	if err := b.validateClaudeChainCandidateMetadata(source, candidate); err != nil {
		return err
	}
	return nil
}

func (b *Browser) deferClaudeChainValidation(ctx context.Context, candidate claudeSegment, identity string, evidence []claudeRangeEvidence, foreground ...*claudeChainForegroundBudget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s", claudeChainValidationKey(candidate, evidence), identity)
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return errors.New("conversation browser is closed")
	}
	if existing := b.chainValidations[key]; existing != nil {
		existing.lastAccess = time.Now()
		running := existing.running
		result := existing.err
		b.mu.Unlock()
		if running {
			return errClaudeChainValidationPending
		}
		if result != nil {
			if errors.Is(result, errClaudeChainValidationStale) {
				return errClaudeChainValidationPending
			}
			return result
		}
		// A completed success is only a commitment to the exact descriptor that
		// was checked. Reopen and compare source metadata before admitting it;
		// this closes the completion-before-retry rewrite window.
		var budget *claudeChainForegroundBudget
		if len(foreground) > 0 {
			budget = foreground[0]
		}
		revalidatedErr := b.revalidateClaudeChainCandidate(ctx, candidate, budget)
		if errors.Is(revalidatedErr, errClaudeChainValidationStale) {
			return errClaudeChainValidationPending
		}
		return revalidatedErr
	}
	if b.chainValidationActive >= maxClaudeChainValidationTasks {
		b.mu.Unlock()
		return errClaudeChainValidationPending
	}
	for len(b.chainValidations) >= maxClaudeChainValidationResults {
		oldestKey := ""
		var oldest time.Time
		for itemKey, item := range b.chainValidations {
			if item.running {
				continue
			}
			if oldestKey == "" || item.lastAccess.Before(oldest) {
				oldestKey, oldest = itemKey, item.lastAccess
			}
		}
		if oldestKey == "" {
			b.mu.Unlock()
			return errClaudeChainValidationPending
		}
		delete(b.chainValidations, oldestKey)
	}
	task := &claudeChainValidation{done: make(chan struct{}), running: true, lastAccess: time.Now()}
	b.chainValidations[key] = task
	b.chainValidationActive++
	b.validationWG.Add(1)
	b.mu.Unlock()

	go func() {
		defer b.validationWG.Done()
		validationErr := b.runClaudeChainValidation(candidate, identity, evidence)
		b.mu.Lock()
		task.running = false
		task.err = validationErr
		task.lastAccess = time.Now()
		if b.chainValidationActive > 0 {
			b.chainValidationActive--
		}
		close(task.done)
		b.mu.Unlock()
	}()
	return errClaudeChainValidationPending
}

func (b *Browser) runClaudeChainValidation(candidate claudeSegment, identity string, evidence []claudeRangeEvidence) error {
	source, err := captureFileSourceWithObserver(candidate.Location, b.observeBackgroundRead)
	if err != nil {
		return errClaudeChainLineageChanged
	}
	defer source.close()
	if err := b.validateClaudeChainCandidateMetadata(source, candidate); err != nil {
		return err
	}
	for _, item := range evidence {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if identity != "" && fileIdentity(source.info) != identity || item.End > source.end {
			return errClaudeChainLineageChanged
		}
		b.observeRangeRead(item.End - item.Start)
		digest, digestErr := fileRangeDigestObserved(b.ctx, source.file, item.Start, item.End, nil, b.observeBackgroundRead)
		if digestErr != nil {
			return digestErr
		}
		if digest != item.Digest {
			return errClaudeChainLineageChanged
		}
	}
	// A growth or equal-length metadata change while the digest was running
	// invalidates this task's candidate. The next resolver pass will create a
	// distinct key instead of reusing this result.
	latest, statErr := source.file.Stat()
	if statErr != nil {
		return errClaudeChainLineageChanged
	}
	if latest.Size() > candidate.CapturedEnd {
		return errClaudeChainValidationStale
	}
	if latest.Size() < candidate.CapturedEnd ||
		(candidate.SourceModTime != 0 && latest.ModTime().UnixNano() != candidate.SourceModTime) ||
		(candidate.SourceChangeToken != "" && fileChangeToken(latest) != candidate.SourceChangeToken) {
		return errClaudeChainLineageChanged
	}
	return nil
}

func (b *Browser) validateClaudeChainCandidateMetadata(source fileSource, candidate claudeSegment) error {
	if source.file == nil {
		return errClaudeChainLineageChanged
	}
	// FileInfo from open is immutable. Refresh it after evidence reads too;
	// otherwise the apparent post-scan fence just compares the old snapshot.
	info, err := source.file.Stat()
	if err != nil {
		return errClaudeChainLineageChanged
	}
	if info.Size() > candidate.CapturedEnd {
		return errClaudeChainValidationStale
	}
	if !info.Mode().IsRegular() || info.Size() < candidate.CapturedEnd ||
		candidate.FileIdentity != "" && fileIdentity(info) != candidate.FileIdentity ||
		candidate.SourceModTime != 0 && info.ModTime().UnixNano() != candidate.SourceModTime ||
		candidate.SourceChangeToken != "" && fileChangeToken(info) != candidate.SourceChangeToken {
		return errClaudeChainLineageChanged
	}
	if !source.info.Mode().IsRegular() ||
		candidate.FileIdentity != "" && fileIdentity(source.info) != candidate.FileIdentity ||
		source.revision != candidate.FileRevision || source.end < candidate.CapturedEnd ||
		candidate.SourceChangeToken != "" && fileChangeToken(source.info) != candidate.SourceChangeToken {
		return errClaudeChainLineageChanged
	}
	if source.end > candidate.CapturedEnd {
		return errClaudeChainValidationStale
	}
	if candidate.SourceModTime != 0 && source.info.ModTime().UnixNano() != candidate.SourceModTime {
		return errClaudeChainLineageChanged
	}
	return nil
}

func (b *Browser) revalidateClaudeChainCandidate(ctx context.Context, candidate claudeSegment, _ ...*claudeChainForegroundBudget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, info, err := openContainedConversationFile(candidate.Location)
	if err != nil {
		return errClaudeChainLineageChanged
	}
	defer file.Close()
	if info.Size() > candidate.CapturedEnd {
		return errClaudeChainValidationStale
	}
	if info.Size() < candidate.CapturedEnd ||
		candidate.FileIdentity != "" && fileIdentity(info) != candidate.FileIdentity ||
		candidate.SourceModTime != 0 && info.ModTime().UnixNano() != candidate.SourceModTime ||
		candidate.SourceChangeToken != "" && fileChangeToken(info) != candidate.SourceChangeToken {
		return errClaudeChainLineageChanged
	}
	// The completed task's key already binds FileRevision and every content
	// range. The metadata reopen above detects a replacement, append, or
	// same-size mutation token change without spending another first-record
	// read from the request's aggregate foreground allowance.
	return nil
}

func (b *Browser) digestClaudeChainValidationRange(ctx context.Context, file *os.File, start, end int64, foreground ...*claudeChainForegroundBudget) (string, error) {
	var budget *claudeChainForegroundBudget
	if len(foreground) > 0 {
		budget = foreground[0]
	}
	if budget != nil && !budget.takeValidation(end-start) {
		return "", errClaudeChainForegroundBudget
	}
	return b.digestClaudeChainValidationRangeObserved(ctx, file, start, end, budget)
}

func (b *Browser) digestClaudeChainValidationRangeObserved(ctx context.Context, file *os.File, start, end int64, budget *claudeChainForegroundBudget) (string, error) {
	length := end - start
	if end < start || length < 0 || length > claudeChainForegroundValidationBytes {
		return "", errors.New("Claude chain validation range is too large")
	}
	// writeFileRangeObserved reports chunks after ReadAt has completed. Reserve
	// the complete bounded range first so a final chunk cannot overshoot the
	// aggregate foreground allowance before the observer rejects it.
	if budget != nil && !budget.takePhysical(length) {
		return "", errClaudeChainForegroundBudget
	}
	b.observeRangeRead(length)
	return fileRangeDigestObserved(ctx, file, start, end, nil, func(bytes int64) error {
		b.observeValidationPhysicalRead(bytes)
		return nil
	})
}

// validateClaudeChainJobEvidence authenticates ranges captured by a recent
// page before a prepared index can become visible. It runs on the normal
// preparation worker, not the request goroutine, so a clipped large range is
// never silently replaced by a digest of the already-rewritten source.
func (b *Browser) validateClaudeChainJobEvidence(file *os.File, capturedEnd int64, identity string, evidence []claudeRangeEvidence) error {
	if file == nil {
		return errors.New("conversation source is closed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < capturedEnd {
		if err == nil {
			err = errors.New("Claude chain source changed")
		}
		return err
	}
	if identity != "" && fileIdentity(info) != identity {
		return errors.New("Claude chain source changed")
	}
	for _, item := range uniqueClaudeRangeEvidence(evidence) {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if item.Start < 0 || item.End <= item.Start || item.End > capturedEnd {
			return errors.New("Claude chain source changed")
		}
		// Preparation already scans the complete captured source. Keep the
		// logical obligation visible and measure the actual selected-range pass
		// separately from recent projection reads.
		b.observeRangeRead(item.End - item.Start)
		digest, digestErr := fileRangeDigestObserved(b.ctx, file, item.Start, item.End, nil, b.observeBackgroundRead)
		if digestErr != nil {
			return digestErr
		}
		if digest != item.Digest {
			return errors.New("Claude chain selected range changed")
		}
	}
	return nil
}

func mergeClaudeSegmentEvidence(current, previous claudeSegment) claudeSegment {
	merged := current
	if !claudeChainSameDescriptor(current, previous) {
		return merged
	}
	// Evidence is bound to the descriptor whose captured end it authenticates.
	// A saved context may be older than the scope lineage, so never copy a
	// newer footer/recent/captured range into that context's shorter prefix.
	appendEvidence := func(evidence ...claudeRangeEvidence) {
		merged.ObservedRanges = append(merged.ObservedRanges, claudeEvidenceWithinEnd(evidence, current.CapturedEnd)...)
	}
	if previous.CapturedDigest != "" {
		appendEvidence(claudeRangeEvidence{Start: 0, End: previous.CapturedEnd, Digest: previous.CapturedDigest})
		if previous.CapturedEnd == current.CapturedEnd && merged.CapturedDigest == "" {
			merged.CapturedDigest = previous.CapturedDigest
		}
	}
	if previous.FooterDigest != "" {
		appendEvidence(claudeRangeEvidence{Start: previous.FooterStart, End: previous.FooterEnd, Digest: previous.FooterDigest})
	}
	if previous.RecentDigest != "" {
		appendEvidence(claudeRangeEvidence{Start: previous.RecentStart, End: previous.RecentEnd, Digest: previous.RecentDigest})
		if previous.CapturedEnd == current.CapturedEnd && merged.RecentDigest == "" {
			merged.RecentStart = previous.RecentStart
			merged.RecentEnd = previous.RecentEnd
			merged.RecentDigest = previous.RecentDigest
		}
	}
	merged.ObservedRanges = append(merged.ObservedRanges, claudeEvidenceWithinEnd(previous.ObservedRanges, current.CapturedEnd)...)
	if previous.CapturedEnd <= current.CapturedEnd && previous.EvidenceCompactionPending &&
		previous.EvidenceCompactionStart >= 0 && previous.EvidenceCompactionEnd > previous.EvidenceCompactionStart &&
		previous.EvidenceCompactionEnd <= current.CapturedEnd {
		merged.EvidenceCompactionPending = true
		merged.EvidenceCompactionStart = previous.EvidenceCompactionStart
		merged.EvidenceCompactionEnd = previous.EvidenceCompactionEnd
		merged.EvidenceCompactionRanges = append(merged.EvidenceCompactionRanges,
			claudeEvidenceWithinEnd(previous.EvidenceCompactionRanges, current.CapturedEnd)...)
	}
	if previous.CapturedEnd <= current.CapturedEnd && previous.EvidenceCompactionError != "" {
		merged.EvidenceCompactionError = previous.EvidenceCompactionError
	}
	merged.ObservedRanges = uniqueClaudeRangeEvidence(merged.ObservedRanges)
	merged.EvidenceCompactionRanges = uniqueClaudeRangeEvidence(merged.EvidenceCompactionRanges)
	return merged
}

func (b *Browser) recordClaudeChainLineageLocked(scopeID string, segments []claudeSegment) {
	merged := cloneClaudeSegments(segments)
	if previous, ok := b.chainLineage[scopeID]; ok {
		for index := range merged {
			if index >= len(previous.segments) {
				break
			}
			merged[index] = mergeClaudeSegmentEvidence(merged[index], previous.segments[index])
		}
		if len(previous.segments) > len(merged) {
			// Keep a previously discovered suffix while a descendant is
			// temporarily missing. The next latest discovery can still compare
			// it instead of silently shortening the public lineage.
			merged = append(merged, cloneClaudeSegments(previous.segments[len(merged):])...)
		}
	}
	b.compactClaudeChainEvidenceLocked(merged)
	now := time.Now()
	lineage := claudeChainLineage{segments: merged, lastAccess: now}
	b.chainLineage[scopeID] = lineage
	b.schedulePendingClaudeChainCompactionsLocked(scopeID, &lineage)
	if identity, ok := b.chainLineageIdentity[scopeID]; ok {
		identity.lastAccess = now
		b.chainLineageIdentity[scopeID] = identity
	}
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
		delete(b.chainLineageIdentity, oldestScope)
		for key, task := range b.chainCompactions {
			if task.scopeID == oldestScope {
				delete(b.chainCompactions, key)
			}
		}
	}
}

func (b *Browser) updateClaudeChainLineageEvidenceLocked(chain *claudeChainContext) {
	if chain == nil {
		return
	}
	lineage, ok := b.chainLineage[chain.scopeID]
	if !ok {
		lineage.segments = cloneClaudeSegments(chain.chain.Segments)
	}
	updated := cloneClaudeSegments(chain.chain.Segments)
	for index := range updated {
		if index < len(lineage.segments) && claudeChainSameDescriptor(updated[index], lineage.segments[index]) {
			previous := lineage.segments[index]
			if previous.CapturedEnd > updated[index].CapturedEnd {
				// A saved cursor may update evidence after a newer latest
				// context has already observed append growth. Never move the
				// lineage descriptor backwards; add the old-context evidence
				// to its explicit ledger instead.
				preserved := previous
				preserved.ObservedRanges = append(preserved.ObservedRanges, claudeChainEvidence(updated[index])...)
				preserved.ObservedRanges = uniqueClaudeRangeEvidence(preserved.ObservedRanges)
				updated[index] = preserved
			} else {
				updated[index] = mergeClaudeSegmentEvidence(updated[index], previous)
			}
		}
		if !updated[index].EvidenceCompactionPending {
			updated[index].ObservedRanges = uniqueClaudeRangeEvidence(updated[index].ObservedRanges)
		}
	}
	b.compactClaudeChainEvidenceLocked(updated)
	if len(lineage.segments) > len(updated) {
		updated = append(updated, cloneClaudeSegments(lineage.segments[len(updated):])...)
	}
	lineage.segments = updated
	lineage.lastAccess = time.Now()
	b.chainLineage[chain.scopeID] = lineage
	b.schedulePendingClaudeChainCompactionsLocked(chain.scopeID, &lineage)
}

func uniqueClaudeRangeEvidence(evidence []claudeRangeEvidence) []claudeRangeEvidence {
	seen := make(map[string]bool, len(evidence))
	unique := make([]claudeRangeEvidence, 0, len(evidence))
	for _, item := range evidence {
		if item.Digest == "" {
			continue
		}
		key := fmt.Sprintf("%d:%d:%s", item.Start, item.End, item.Digest)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, item)
	}
	return unique
}

func claudeChainEvidenceBounds(evidence []claudeRangeEvidence) (int64, int64, bool) {
	if len(evidence) == 0 {
		return 0, 0, false
	}
	start, end := evidence[0].Start, evidence[0].End
	if start < 0 || end <= start {
		return 0, 0, false
	}
	for _, item := range evidence[1:] {
		if item.Start < 0 || item.End <= item.Start {
			return 0, 0, false
		}
		if item.Start < start {
			start = item.Start
		}
		if item.End > end {
			end = item.End
		}
	}
	return start, end, true
}

func (b *Browser) compactClaudeChainEvidenceLocked(segments []claudeSegment) {
	for index := range segments {
		if segments[index].EvidenceCompactionPending || segments[index].EvidenceCompactionError != "" {
			continue
		}
		evidence := uniqueClaudeRangeEvidence(claudeChainEvidence(segments[index]))
		if len(evidence) <= maxClaudeChainEvidenceRanges {
			segments[index].ObservedRanges = uniqueClaudeRangeEvidence(segments[index].ObservedRanges)
			segments[index].EvidenceCompactionRanges = nil
			continue
		}
		prepareClaudeChainEvidenceCompaction(&segments[index], evidence)
	}
}

func prepareClaudeChainEvidenceCompaction(segment *claudeSegment, evidence []claudeRangeEvidence) {
	start, end, ok := claudeChainEvidenceBounds(evidence)
	if !ok {
		segment.EvidenceCompactionError = errClaudeChainLineageChanged.Error()
		return
	}
	segment.ObservedRanges = nil
	segment.EvidenceCompactionRanges = append([]claudeRangeEvidence(nil), evidence...)
	segment.EvidenceCompactionPending = true
	segment.EvidenceCompactionStart = start
	segment.EvidenceCompactionEnd = end
}

func claudeChainEvidenceGeneration(segment claudeSegment) string {
	evidence := claudeChainEvidence(segment)
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].Start != evidence[j].Start {
			return evidence[i].Start < evidence[j].Start
		}
		if evidence[i].End != evidence[j].End {
			return evidence[i].End < evidence[j].End
		}
		return evidence[i].Digest < evidence[j].Digest
	})
	return claudeChainValidationKey(segment, evidence)
}

func (b *Browser) schedulePendingClaudeChainCompactionsLocked(scopeID string, lineage *claudeChainLineage) {
	if lineage == nil || b.closing {
		return
	}
	if b.chainCompactions == nil {
		b.chainCompactions = make(map[string]*claudeChainEvidenceCompaction)
	}
	for index := range lineage.segments {
		segment := lineage.segments[index]
		if !segment.EvidenceCompactionPending {
			continue
		}
		key := fmt.Sprintf("%s:%d:%s:%d:%d", scopeID, index,
			claudeChainValidationKey(segment, nil), segment.EvidenceCompactionStart, segment.EvidenceCompactionEnd)
		if _, exists := b.chainCompactions[key]; exists || b.chainCompactionActive >= maxClaudeChainValidationTasks {
			continue
		}
		// A changed generation supersedes, rather than runs alongside, the
		// current task. Its completion will requeue the complete new union.
		running := false
		for _, task := range b.chainCompactions {
			if task.scopeID == scopeID && task.segment == index {
				running = true
				break
			}
		}
		if running {
			continue
		}
		evidence := claudeChainEvidence(segment)
		start, end, ok := claudeChainEvidenceBounds(evidence)
		if !ok {
			continue
		}
		task := &claudeChainEvidenceCompaction{
			key: key, scopeID: scopeID, segment: index, candidate: cloneClaudeSegments([]claudeSegment{segment})[0],
			start: start, end: end,
			evidence: evidence, generation: claudeChainEvidenceGeneration(segment),
		}
		b.chainCompactions[key] = task
		b.chainCompactionActive++
		b.validationWG.Add(1)
		go b.runClaudeChainEvidenceCompaction(task)
	}
}

func (b *Browser) runClaudeChainEvidenceCompaction(task *claudeChainEvidenceCompaction) {
	defer b.validationWG.Done()
	var result error
	source, err := captureFileSourceWithObserver(task.candidate.Location, b.observeBackgroundRead)
	if err != nil {
		result = errClaudeChainLineageChanged
	} else {
		defer source.close()
		if task.start < 0 || task.end <= task.start || source.end < task.end || task.candidate.FileIdentity != "" && fileIdentity(source.info) != task.candidate.FileIdentity ||
			(source.end == task.candidate.CapturedEnd && (source.revision != task.candidate.FileRevision ||
				task.candidate.SourceChangeToken != "" && fileChangeToken(source.info) != task.candidate.SourceChangeToken)) {
			result = errClaudeChainLineageChanged
		} else {
			items := uniqueClaudeRangeEvidence(task.evidence)
			validateItems := func() error {
				for _, item := range items {
					if item.Start < 0 || item.End <= item.Start || item.End > task.end {
						return errClaudeChainLineageChanged
					}
					b.observeRangeRead(item.End - item.Start)
					oldDigest, digestErr := fileRangeDigestObserved(b.ctx, source.file, item.Start, item.End, nil, b.observeBackgroundRead)
					if digestErr != nil {
						return digestErr
					}
					if oldDigest != item.Digest {
						return errClaudeChainLineageChanged
					}
				}
				return nil
			}

			// Authenticate the original obligations, then hash the enclosing range.
			// Repeat both phases before replacing the obligations: growth is allowed
			// outside the captured prefix, but a rewrite during either phase must not
			// be converted into a digest that retroactively authorizes old evidence.
			if result = validateItems(); result == nil {
				b.observeChainCompactionPhase()
				firstDigest, digestErr := fileRangeDigestObserved(b.ctx, source.file, task.start, task.end, nil, b.observeBackgroundRead)
				if digestErr != nil {
					result = digestErr
				} else if result = validateItems(); result == nil {
					secondDigest, secondErr := fileRangeDigestObserved(b.ctx, source.file, task.start, task.end, nil, b.observeBackgroundRead)
					if secondErr != nil {
						result = secondErr
					} else if firstDigest != secondDigest {
						result = errClaudeChainLineageChanged
					} else {
						task.digest = secondDigest
						latest, statErr := source.file.Stat()
						switch {
						case statErr != nil || latest.Size() < task.end:
							result = errClaudeChainLineageChanged
						case latest.Size() == source.info.Size() &&
							(latest.ModTime().UnixNano() != source.info.ModTime().UnixNano() || fileChangeToken(latest) != fileChangeToken(source.info)):
							result = errClaudeChainLineageChanged
						case latest.Size() == task.candidate.CapturedEnd &&
							(task.candidate.SourceModTime != 0 && latest.ModTime().UnixNano() != task.candidate.SourceModTime ||
								task.candidate.SourceChangeToken != "" && fileChangeToken(latest) != task.candidate.SourceChangeToken):
							result = errClaudeChainLineageChanged
						default:
							result = nil
						}
					}
				}
			}
		}
	}

	b.mu.Lock()
	if b.chainCompactionActive > 0 {
		b.chainCompactionActive--
	}
	// Pointer ownership fences scope reset/eviction and recreation, even when
	// a replacement task happens to have the same descriptor-derived key.
	owned := b.chainCompactions[task.key] == task
	if owned {
		delete(b.chainCompactions, task.key)
	}
	lineage, ok := b.chainLineage[task.scopeID]
	if owned && ok && task.segment >= 0 && task.segment < len(lineage.segments) {
		current := &lineage.segments[task.segment]
		if current.EvidenceCompactionPending {
			if claudeChainEvidenceGeneration(*current) != task.generation {
				// An older preparation or projection may publish while we scan.
				// Neither success nor failure for the superseded generation can
				// replace that union. Retain ALL obligations and authenticate a
				// new enclosure in the bounded worker slots. No foreground I/O.
				prepareClaudeChainEvidenceCompaction(current, claudeChainEvidence(*current))
			} else if result == nil {
				current.ObservedRanges = []claudeRangeEvidence{{Start: task.start, End: task.end, Digest: task.digest}}
				current.EvidenceCompactionPending = false
				current.EvidenceCompactionStart = 0
				current.EvidenceCompactionEnd = 0
				current.EvidenceCompactionError = ""
				current.EvidenceCompactionRanges = nil
			} else {
				current.EvidenceCompactionPending = false
				current.EvidenceCompactionError = result.Error()
				current.EvidenceCompactionRanges = nil
			}
			lineage.lastAccess = time.Now()
			b.chainLineage[task.scopeID] = lineage
		}
	}
	// A scope can have more pending segments than the two active compaction
	// slots. Completion must schedule the next one; otherwise the later
	// obligations would remain pending forever because no new lineage write is
	// required to trigger scheduling.
	if next, exists := b.chainLineage[task.scopeID]; exists {
		b.schedulePendingClaudeChainCompactionsLocked(task.scopeID, &next)
	}
	b.mu.Unlock()
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
	for key, compaction := range b.chainCompactions {
		if compaction.scopeID == scopeID {
			delete(b.chainCompactions, key)
		}
	}
	identity, err := randomIdentifier()
	if err != nil {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", scopeID, time.Now().UnixNano())))
		identity = hex.EncodeToString(digest[:])
	}
	b.chainLineageIdentity[scopeID] = claudeChainLineageIdentity{id: identity, lastAccess: time.Now()}
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
		chain:          claudeChain{Segments: cloneClaudeSegments(chain.chain.Segments), IncompleteReason: chain.chain.IncompleteReason},
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

func claudeChainSameDescriptor(left, right claudeSegment) bool {
	return left.SessionID == right.SessionID && left.Location.Path == right.Location.Path && left.FileIdentity == right.FileIdentity
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
		_, _ = io.WriteString(hash, segment.SourceChangeToken)
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
	// The first A→B context must remain wire-compatible with the ordinary
	// single-file A context. A public lineage identity is created only by an
	// authenticated replacement reset; otherwise a newly discovered child is
	// transparent to cursors and the frontend.
	publicRevision := chain.Segments[0].FileRevision
	b.mu.Lock()
	lineageIdentity := b.chainLineageIdentity[scopeID]
	lineageIdentity.lastAccess = time.Now()
	b.chainLineageIdentity[scopeID] = lineageIdentity
	b.mu.Unlock()
	if lineageIdentity.id != "" {
		digest := sha256.Sum256([]byte(publicRevision + "\x00" + lineageIdentity.id))
		publicRevision = hex.EncodeToString(digest[:])
	}
	context := &claudeChainContext{
		id: id, key: key, scopeID: scopeID, snapshotID: snapshotID,
		publicRevision: publicRevision, chain: chain,
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

func (b *Browser) captureClaudeChainSource(location Location, budget *claudeChainForegroundBudget) (fileSource, error) {
	return captureFileSourceWithReserve(location, reserveClaudeChainPhysical(budget), b.observeValidationRead)
}

func reserveClaudeChainPhysical(budget *claudeChainForegroundBudget) func(int64) error {
	if budget == nil {
		return nil
	}
	return func(bytes int64) error {
		if !budget.takePhysical(bytes) {
			return errClaudeChainForegroundBudget
		}
		return nil
	}
}

func (b *Browser) loadClaudeChainRecentWithBudget(ctx context.Context, scope BrowseScope, chain *claudeChainContext, segmentIndex int, budget *claudeChainRequestBudget) (recentProjection, error) {
	segment, ok := b.claudeChainSegmentSnapshot(chain, segmentIndex)
	if !ok {
		return recentProjection{}, errors.New("invalid Claude chain segment")
	}
	var foregroundBudget *claudeChainForegroundBudget
	if budget != nil {
		foregroundBudget = budget.validation
	}
	source, err := b.captureClaudeChainSource(segment.Location, foregroundBudget)
	if errors.Is(err, errClaudeChainForegroundBudget) {
		return recentProjection{}, errClaudeChainRecentBudget
	}
	if err != nil {
		return recentProjection{}, err
	}
	defer source.close()
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd ||
		segment.FileIdentity != "" && fileIdentity(source.info) != segment.FileIdentity {
		return recentProjection{}, errors.New("Claude chain source changed")
	}
	if source.end == segment.CapturedEnd &&
		(segment.SourceModTime != 0 && source.info.ModTime().UnixNano() != segment.SourceModTime ||
			segment.SourceChangeToken != "" && fileChangeToken(source.info) != segment.SourceChangeToken) {
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
	if budget != nil && budget.recent != nil && !budget.recent.take(window) {
		return recentProjection{}, errClaudeChainRecentBudget
	}
	b.observeSourceRead(window)
	b.observeRecentRead(window)
	start := segment.CapturedEnd - window
	if start < 0 {
		start = 0
	}
	if start > 0 && foregroundBudget != nil && !foregroundBudget.takePhysical(1) {
		return recentProjection{}, errClaudeChainRecentBudget
	}
	if segment.RecentDigest != "" && (segment.RecentStart != start || segment.RecentEnd != segment.CapturedEnd) {
		return recentProjection{}, errors.New("Claude chain selected range changed")
	}
	projection, digest, err := b.projectRecentRangeSingleRead(ctx, scope, source, start, segment.CapturedEnd, segment.RecentDigest, false)
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

func (b *Browser) claudeChainLatestValidationFailure(ctx context.Context, err error) BrowsePage {
	if errors.Is(err, errClaudeChainValidationStale) {
		message := "Conversation history advanced; try again shortly."
		return browseFailure(true, "validation_pending", message, browseErrorFor("validation_pending", message, true))
	}
	return b.claudeChainSourceFailure(ctx, err)
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

func (b *Browser) validateClaudeChainSegment(ctx context.Context, segment claudeSegment, foreground ...*claudeChainForegroundBudget) error {
	var foregroundBudget *claudeChainForegroundBudget
	if len(foreground) > 0 {
		foregroundBudget = foreground[0]
	}
	if foregroundBudget == nil {
		foregroundBudget = &claudeChainForegroundBudget{validationRemaining: claudeChainForegroundValidationBytes, physicalRemaining: claudeChainForegroundPhysicalBytes}
	}
	source, err := captureFileSourceWithReserve(segment.Location, reserveClaudeChainPhysical(foregroundBudget), b.observeValidationRead)
	if err != nil {
		if errors.Is(err, errClaudeChainForegroundBudget) {
			return errClaudeChainValidationPending
		}
		return err
	}
	defer source.close()
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd {
		return errors.New("Claude chain source changed")
	}
	if source.end == segment.CapturedEnd &&
		(segment.SourceModTime != 0 && source.info.ModTime().UnixNano() != segment.SourceModTime ||
			segment.SourceChangeToken != "" && fileChangeToken(source.info) != segment.SourceChangeToken) {
		return errors.New("Claude chain source changed")
	}
	if segment.FooterStart < 0 || segment.FooterEnd < segment.FooterStart || segment.FooterEnd > segment.CapturedEnd {
		return errors.New("Claude chain footer range is invalid")
	}
	if segment.FooterDigest != "" {
		digest, digestErr := b.digestClaudeChainValidationRange(ctx, source.file, segment.FooterStart, segment.FooterEnd, foregroundBudget)
		if digestErr != nil || digest != segment.FooterDigest {
			if digestErr == nil {
				digestErr = errors.New("Claude chain footer changed")
			}
			return digestErr
		}
	}
	if segment.RecentDigest != "" {
		if segment.RecentStart < 0 || segment.RecentEnd < segment.RecentStart || segment.RecentEnd > segment.CapturedEnd {
			return errors.New("Claude chain selected range is invalid")
		}
		// A recent projection is already re-digested by the bounded read path.
		// Prepared cursors validate a small selected range here, but never hash
		// an arbitrary recent tail in the foreground merely to check a cursor.
		if segment.RecentEnd-segment.RecentStart <= claudeChainForegroundValidationBytes {
			digest, digestErr := b.digestClaudeChainValidationRange(ctx, source.file, segment.RecentStart, segment.RecentEnd, foregroundBudget)
			if digestErr != nil || digest != segment.RecentDigest {
				if digestErr == nil {
					digestErr = errors.New("Claude chain selected range changed")
				}
				return digestErr
			}
		}
	}
	if segment.CapturedDigest != "" && segment.CapturedEnd <= claudeChainForegroundValidationBytes && segment.RecentDigest == "" {
		digest, digestErr := b.digestClaudeChainValidationRange(ctx, source.file, 0, segment.CapturedEnd, foregroundBudget)
		if digestErr != nil || digest != segment.CapturedDigest {
			if digestErr == nil {
				digestErr = errors.New("Claude chain captured range changed")
			}
			return digestErr
		}
	}
	return nil
}

func (b *Browser) claudeChainSegmentPrepared(chain *claudeChainContext, segment int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return segment >= 0 && segment < len(chain.prepared) && chain.prepared[segment]
}

func (b *Browser) claudeChainValidationPendingPage(scope BrowseScope, chain *claudeChainContext, cursor string) BrowsePage {
	view := b.claudeChainView(chain)
	return BrowsePage{
		Available: true, State: BrowsePreparing, Mode: BrowseSnapshot, Entries: []Entry{},
		SourceRevision: view.publicRevision, SnapshotID: view.snapshotID,
		NextCursor: cursor, HasMore: cursor != "", Diagnostics: view.diagnostics,
		Reason: "Validating conversation history on this computer…",
	}
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
	if page.State != BrowsePreparing {
		page.State = BrowseFailed
		if page.ReasonCode == "" {
			page.ReasonCode = "index_failed"
		}
		if page.Error == nil {
			page.Error = browseErrorFor(page.ReasonCode, page.Reason, true)
		}
	}
	cursor, err := b.claudeChainCursor(scope, chain, segment, boundary, byteBoundary)
	if err == nil {
		page.NextCursor = cursor
		page.HasMore = cursor != ""
	}
	return page
}

func (b *Browser) startClaudeChainJob(scope BrowseScope, chain *claudeChainContext, segmentIndex int, foreground ...*claudeChainForegroundBudget) (*browseJob, *BrowsePage) {
	var foregroundBudget *claudeChainForegroundBudget
	if len(foreground) > 0 {
		foregroundBudget = foreground[0]
	}
	if foregroundBudget == nil {
		foregroundBudget = &claudeChainForegroundBudget{validationRemaining: claudeChainForegroundValidationBytes, physicalRemaining: claudeChainForegroundPhysicalBytes}
	}
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
	if lineage, ok := b.chainLineage[chain.scopeID]; ok && segmentIndex < len(lineage.segments) {
		if lineage.segments[segmentIndex].EvidenceCompactionPending {
			// Existing jobs above may finish and publish their reserved fixed-end
			// digest, but do not admit new evidence-producing jobs while the
			// union is being compacted. Public cursor reads apply the same gate.
			b.mu.Unlock()
			page := b.claudeChainValidationPendingPage(scope, chain, "")
			return nil, &page
		}
		// A context stores only its current descriptor fields. Bind the job to
		// the scope lineage as well so evidence authenticated by earlier append
		// windows is not lost when this context prepares the segment later.
		segment = mergeClaudeSegmentEvidence(segment, lineage.segments[segmentIndex])
	}
	chainID := chain.id
	b.mu.Unlock()

	source, err := b.captureClaudeChainSource(segment.Location, foregroundBudget)
	if errors.Is(err, errClaudeChainForegroundBudget) {
		page := b.claudeChainValidationPendingPage(scope, chain, "")
		return nil, &page
	}
	if err != nil {
		page := browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false))
		return nil, &page
	}
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd ||
		source.end == segment.CapturedEnd &&
			(segment.SourceModTime != 0 && source.info.ModTime().UnixNano() != segment.SourceModTime ||
				segment.SourceChangeToken != "" && fileChangeToken(source.info) != segment.SourceChangeToken) {
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
	// Immutable lineage evidence is part of admission, not a follow-up
	// mutation. createJob publishes to the worker queue before returning, so
	// assigning these fields afterward would let runJob observe an empty ledger.
	binding := claudeChainJobBinding{
		footerStart: segment.FooterStart, footerEnd: segment.FooterEnd,
		footerDigest: segment.FooterDigest, observedRanges: claudeChainEvidence(segment),
	}
	job, page := b.createJob(scope, source, 0, segment.CapturedEnd, segment.CapturedEnd, fullDigest, false, chainID, segmentIndex, binding)
	if page != nil || job == nil {
		if job == nil {
			source.close()
		}
		return job, page
	}
	job.mu.RLock()
	ownedByJob := job.source.file == source.file
	job.mu.RUnlock()
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

func (b *Browser) continueClaudeChainJob(ctx context.Context, request BrowseRequest, chain *claudeChainContext, segmentIndex int, boundary int64, byteBoundary bool, job *browseJob, budget *claudeChainRequestBudget) BrowsePage {
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
			if b.retryJob(ctx, job, budgetForeground(budget)) {
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
	return b.claudeChainPreparedPageAt(ctx, request.Scope, request.Limit, chain, segmentIndex, boundary, request.Retry, budget)
}

func (b *Browser) claudeChainPreparedPageAt(ctx context.Context, scope BrowseScope, limit int, chain *claudeChainContext, segmentIndex int, boundary int64, retry bool, budget *claudeChainRequestBudget) BrowsePage {
	if budget == nil {
		budget = newClaudeChainRequestBudget(b.options.RecentBytes)
	}
	b.mu.Lock()
	if segmentIndex < 0 || segmentIndex >= len(chain.jobs) {
		b.mu.Unlock()
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false))
	}
	job := chain.jobs[segmentIndex]
	b.mu.Unlock()
	if job == nil {
		recent, err := b.loadClaudeChainRecentWithBudget(ctx, scope, chain, segmentIndex, budget)
		if errors.Is(err, errClaudeChainRecentBudget) {
			return b.claudeChainBudgetPage(scope, chain, segmentIndex)
		}
		if err != nil {
			return b.claudeChainSourceFailure(ctx, err)
		}
		return b.claudeChainPageAt(ctx, scope, limit, chain, false, segmentIndex, len(recent.Entries), recent.Entries, budget, false)
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
			return b.continueClaudeChainJob(ctx, BrowseRequest{Scope: scope, Limit: limit, Retry: retry}, chain, segmentIndex, boundary, true, job, budget)
		}
		return b.claudeChainPreparingPageForJob(scope, limit, chain, segmentIndex, boundary, true, job)
	}
	if err := b.validateSnapshotJob(ctx, job, source, budgetForeground(budget)); err != nil {
		if errors.Is(err, errClaudeChainValidationPending) {
			return b.claudeChainPreparingPage(scope, limit, chain, segmentIndex, boundary, true)
		}
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
		if budget == nil {
			budget = newClaudeChainRequestBudget(b.options.RecentBytes)
		}
		nextSegment, nextBoundary, hasNext, nextIsOffset, nextErr := b.claudeChainOlderPosition(ctx, scope, chain, segmentIndex, 0, budget, false)
		if nextErr != nil && !errors.Is(nextErr, errClaudeChainRecentBudget) {
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

func (b *Browser) claudeChainLatestPage(ctx context.Context, scope BrowseScope, limit int, chain *claudeChainContext, budget *claudeChainRequestBudget) BrowsePage {
	if chain == nil {
		return browseFailure(true, "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This history cursor has expired. Start browsing again from the latest messages.", true))
	}
	if budget == nil {
		budget = newClaudeChainRequestBudget(b.options.RecentBytes)
	}
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
			page := b.claudeChainPreparedPageAt(ctx, scope, limit, chain, index, view.chain.Segments[index].CapturedEnd, false, budget)
			// The index is an internal implementation detail for this cursorless
			// request. Latest always remains recent-mode on the wire; snapshot
			// mode is reserved for frozen older browsing.
			page.Mode = BrowseRecent
			page.SnapshotID = ""
			return page
		}
		recentTailExhausted := index < len(view.recentComplete) && !view.recentComplete[index]
		recent, err := b.loadClaudeChainRecentWithBudget(ctx, scope, chain, index, budget)
		if errors.Is(err, errClaudeChainRecentBudget) {
			return b.claudeChainBudgetPage(scope, chain, index)
		}
		if err != nil {
			return b.claudeChainSourceFailure(ctx, err)
		}
		if len(recent.Entries) > 0 {
			return b.claudeChainPageAt(ctx, scope, limit, chain, true, index, len(recent.Entries), recent.Entries, budget, recentTailExhausted)
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
	// This segment was not read in the current latest request. A cached
	// projection from an earlier request is not delivered by this page, so a
	// budget-exhaustion cursor must begin at the descriptor's captured end.
	// Starting at a cached RecentStart would silently discard that segment's
	// visible rows on the next traversal.
	boundary := segment.CapturedEnd
	cursor, err := b.claudeChainCursor(scope, chain, segmentIndex, boundary, true)
	if err != nil {
		return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
	}
	return BrowsePage{Available: true, State: BrowseReady, Mode: BrowseRecent, Entries: []Entry{},
		SourceRevision: view.publicRevision, NextCursor: cursor, HasMore: cursor != "", Diagnostics: view.diagnostics,
		Reason: "Older history can be prepared on this computer…"}
}

func (b *Browser) claudeChainPageAt(ctx context.Context, scope BrowseScope, limit int, chain *claudeChainContext, latest bool, segmentIndex, boundary int, entries []projectedEntry, budget *claudeChainRequestBudget, recentTailExhausted bool) BrowsePage {
	if budget == nil {
		budget = newClaudeChainRequestBudget(b.options.RecentBytes)
	}
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
		nextSegment, nextBoundary, hasNext, nextIsOffset, nextErr := b.claudeChainOlderPosition(ctx, scope, chain, segmentIndex, start, budget, recentTailExhausted)
		page := base
		page.Entries = projectedEntries(selected)
		if hasNext && (nextErr == nil || errors.Is(nextErr, errClaudeChainRecentBudget)) {
			if !nextIsOffset && nextSegment == segmentIndex {
				segmentView := b.claudeChainView(chain)
				if segmentIndex >= 0 && segmentIndex < len(segmentView.chain.Segments) {
					segment := segmentView.chain.Segments[segmentIndex]
					nextBoundary = claudeChainRecentBoundary(entries, nextBoundary, segment.CapturedEnd,
						recentTailExhausted, segment.RecentStart)
					nextIsOffset = true
				}
			}
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

func (b *Browser) claudeChainOlderPosition(ctx context.Context, scope BrowseScope, chain *claudeChainContext, segmentIndex, start int, budget *claudeChainRequestBudget, recentTailExhausted bool) (int, int64, bool, bool, error) {
	view := b.claudeChainView(chain)
	if segmentIndex < 0 || segmentIndex >= len(view.chain.Segments) {
		return 0, 0, false, false, errors.New("invalid Claude chain segment")
	}
	if start > 0 {
		return segmentIndex, int64(start), true, false, nil
	}
	if recentTailExhausted {
		// The caller exhausted a bounded tail. Preparation may have completed
		// concurrently, but that cannot turn this request into index exhaustion:
		// its next cursor must still expose this segment's omitted prefix.
		return segmentIndex, view.chain.Segments[segmentIndex].RecentStart, true, true, nil
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
			// The segment fits the recent window, so its captured end is the
			// byte boundary corresponding to the count of all projected rows.
			// Emit a byte cursor and avoid rereading this range just to translate
			// the count on the next request.
			return index, view.chain.Segments[index].CapturedEnd, true, true, nil
		}
	}
	return 0, 0, false, false, nil
}

func claudeChainRecentBoundary(entries []projectedEntry, boundary int64, capturedEnd int64, tailExhausted bool, recentStart int64) int64 {
	if boundary <= 0 || len(entries) == 0 {
		if tailExhausted && recentStart >= 0 {
			return recentStart
		}
		return 0
	}
	if boundary >= int64(len(entries)) {
		return capturedEnd
	}
	return entries[boundary].Offset
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
