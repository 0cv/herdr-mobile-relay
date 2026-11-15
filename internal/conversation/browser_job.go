package conversation

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
)

type expiredJob struct {
	index      *snapshotIndex
	sourceFile *os.File
}

type browseJob struct {
	id          string
	dedupKey    string
	scope       BrowseScope
	scopeID     string
	source      fileSource
	boundary    int64
	rangeStart  int64
	rangeEnd    int64
	rangeDigest string
	cursor      string
	snapshotID  string
	index       *snapshotIndex

	mu                     sync.RWMutex
	state                  BrowseState
	phase                  string
	scannedBytes           int64
	sourceBytes            int64
	diagnostics            BrowseDiagnostics
	plan                   *OMOTodoState
	total                  int
	err                    *BrowseError
	lastInterest           time.Time
	readers                int
	validatedSize          int64
	validatedModTime       int64
	validatedChangeToken   string
	sourceDigest           string
	chainContextID         string
	chainSegment           int
	chainFooterStart       int64
	chainFooterEnd         int64
	chainFooterDigest      string
	chainObservedRanges    []claudeRangeEvidence
	chainValidationKey     string
	chainValidationRunning bool
	chainValidationErr     error
	chainValidationDone    bool
	autoRetried            bool
	queued                 bool
	running                bool
	lastAccess             time.Time
	retired                bool
}

func (b *Browser) job(id string) *browseJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.jobs[id]
}

func (b *Browser) acquireJob(id string) *browseJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	job := b.jobs[id]
	if job == nil {
		return nil
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.retired {
		return nil
	}
	job.readers++
	job.lastInterest = time.Now()
	job.lastAccess = time.Now()
	return job
}

func (b *Browser) retainJob(job *browseJob) bool {
	if job == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.jobs[job.id] != job {
		return false
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.retired {
		return false
	}
	job.readers++
	job.lastInterest = time.Now()
	job.lastAccess = time.Now()
	return true
}

func (b *Browser) releaseJob(job *browseJob) {
	if job == nil {
		return
	}
	job.mu.Lock()
	if job.readers > 0 {
		job.readers--
	}
	job.mu.Unlock()
}

func (b *Browser) jobMatches(job *browseJob, scope BrowseScope, source fileSource, start, end, boundary int64, digest string) bool {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return !job.retired && job.scopeID == browseScopeID(scope) && job.source.revision == source.revision &&
		job.rangeStart == start && job.rangeEnd == end && job.boundary == boundary && job.rangeDigest == digest
}

type claudeChainJobBinding struct {
	footerStart    int64
	footerEnd      int64
	footerDigest   string
	observedRanges []claudeRangeEvidence
}

func (b *Browser) createJob(scope BrowseScope, source fileSource, start, end, boundary int64, digest string, chainContextID string, chainSegment int, binding claudeChainJobBinding) (*browseJob, *BrowsePage) {
	if b.cacheErr != nil {
		page := failPage("index_storage_unavailable", "Older history cannot be prepared on this computer right now.", true)
		return nil, &page
	}
	b.observeCapacityOperation("admission", nil, false)
	b.capacityMu.Lock()
	capacityLocked := true
	defer func() {
		if capacityLocked {
			b.unlockCapacity()
		}
	}()
	scopeID := browseScopeID(scope)
	dedupKey := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%d", scopeID, source.revision, start, end, boundary, digest, chainContextID, chainSegment)
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		page := failPage("index_failed", "Older history preparation is shutting down.", true)
		return nil, &page
	}
	if id := b.dedup[dedupKey]; id != "" {
		job := b.jobs[id]
		b.mu.Unlock()
		if job != nil {
			b.renewJob(job)
			return job, nil
		}
		b.mu.Lock()
		delete(b.dedup, dedupKey)
	}
	if b.closing {
		b.mu.Unlock()
		page := failPage("index_failed", "Older history preparation is shutting down.", true)
		return nil, &page
	}
	if len(b.queue) >= browseQueuedPreparations {
		b.mu.Unlock()
		page := failPage("index_busy", "Another history preparation is already queued. Try again shortly.", true)
		return nil, &page
	}
	id, err := randomIdentifier()
	if err != nil {
		b.mu.Unlock()
		page := failPage("index_failed", "History preparation could not be started.", true)
		return nil, &page
	}
	snapshotID, err := randomIdentifier()
	if err != nil {
		b.mu.Unlock()
		page := failPage("index_failed", "History preparation could not be started.", true)
		return nil, &page
	}
	b.mu.Unlock()
	request := &browseJob{scopeID: scopeID, source: source}
	if code := b.ensureCapacity(request, nil, "admission", true); code != "" {
		return nil, capacityFailurePage(code)
	}
	index, code, err := b.openSnapshotWithCapacity(request, "admission", snapshotID, source, boundary)
	if err != nil {
		page := failPage("index_storage_unavailable", "Older history cannot be prepared on this computer right now.", true)
		return nil, &page
	}
	if code != "" {
		return nil, capacityFailurePage(code)
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		_ = index.remove()
		page := failPage("index_failed", "Older history preparation is shutting down.", true)
		return nil, &page
	}
	binding.observedRanges = append([]claudeRangeEvidence(nil), binding.observedRanges...)
	job := &browseJob{
		id: id, dedupKey: dedupKey, scope: scope, scopeID: scopeID, source: source,
		boundary: boundary, rangeStart: start, rangeEnd: end, rangeDigest: digest,
		chainContextID: chainContextID, chainSegment: chainSegment,
		chainFooterStart: binding.footerStart, chainFooterEnd: binding.footerEnd,
		chainFooterDigest: binding.footerDigest, chainObservedRanges: binding.observedRanges,
		snapshotID: snapshotID, index: index, state: BrowsePreparing, phase: "queued",
		sourceBytes: source.end, lastInterest: time.Now(), lastAccess: time.Now(),
	}
	job.cursor, err = encodeBrowseCursor(b.key, browseCursor{
		Mode: "prepare", Scope: scopeID, Revision: source.revision, JobID: id,
		Boundary: browseOffset(boundary), RangeStart: browseOffset(start), RangeEnd: browseOffset(end),
		RangeDigest: digest, ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
	})
	if err != nil {
		b.mu.Unlock()
		_ = index.remove()
		page := failPage("index_failed", "History preparation could not be started.", true)
		return nil, &page
	}
	job.queued = true
	b.jobs[id] = job
	b.dedup[dedupKey] = id
	b.queue = append(b.queue, job)
	b.mu.Unlock()
	b.unlockCapacity()
	capacityLocked = false
	b.signalWake()
	if chainContextID != "" {
		b.observe(browserObservation{kind: "chain_queue_admission", job: job})
	}
	return job, nil
}

func (b *Browser) openSnapshotWithCapacity(owner *browseJob, stage, snapshotID string, source fileSource, boundary int64) (*snapshotIndex, string, error) {
	metadata := snapshotMetadata{
		Schema: 1, State: "building", SnapshotID: snapshotID, SourceRevision: source.revision,
		EndOffset: source.end, Boundary: boundary, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	index, err := openSnapshotIndex(b.cacheRoot, snapshotID, metadata)
	if err != nil {
		return nil, "", err
	}
	if code := b.ensureCapacity(owner, index, stage, false); code != "" {
		_ = index.remove()
		return nil, code, nil
	}
	return index, "", nil
}

func randomIdentifier() (string, error) {
	var bytes [16]byte
	if _, err := cryptorand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (b *Browser) renewJob(job *browseJob) {
	job.mu.Lock()
	job.lastInterest = time.Now()
	job.lastAccess = time.Now()
	job.mu.Unlock()
}

func (b *Browser) continueJob(ctx context.Context, request BrowseRequest, job *browseJob) (BrowsePage, error) {
	if !b.retainJob(job) {
		return failPage("cursor_expired", "This snapshot cursor has expired. Start browsing again from the latest messages.", true), nil
	}
	defer b.releaseJob(job)
	job.mu.RLock()
	state := job.state
	err := job.err
	cursor := job.cursor
	autoRetried := job.autoRetried
	job.mu.RUnlock()
	if state == BrowsePreparing {
		return b.preparingPage(job, cursor), nil
	}
	if state == BrowseFailed {
		if b.retryFailedJob(ctx, job, err, autoRetried, request.Retry, nil) {
			return b.preparingPage(job, cursor), nil
		}
		if err == nil {
			err = browseErrorFor("index_failed", "History preparation failed.", true)
		}
		nextCursor := ""
		if cursor != "" && err.Retryable {
			nextCursor = cursor
		}
		return BrowsePage{
			Available: true, State: BrowseFailed, Mode: BrowseRecent, Entries: []Entry{}, HasMore: nextCursor != "",
			ReasonCode: err.Code, Reason: err.Message, NextCursor: nextCursor, Error: err,
		}, nil
	}
	return b.readSnapshotPage(ctx, request.Scope, request.Limit, job)
}

func (b *Browser) retryFailedJob(ctx context.Context, job *browseJob, failure *BrowseError, autoRetried, explicitRetry bool, foregroundBudget *claudeChainForegroundBudget) bool {
	if failure == nil || !failure.Retryable || (!explicitRetry && autoRetried) {
		return false
	}
	if !explicitRetry {
		job.mu.Lock()
		job.autoRetried = true
		job.mu.Unlock()
	}
	return b.retryJob(ctx, job, foregroundBudget)
}

func (b *Browser) retryJob(ctx context.Context, job *browseJob, foregroundBudget *claudeChainForegroundBudget) bool {
	job.mu.RLock()
	scope := job.scope
	sourceRevision := job.source.revision
	rangeStart := job.rangeStart
	rangeEnd := job.rangeEnd
	rangeDigest := job.rangeDigest
	chainContextID := job.chainContextID
	chainLocation := job.source.location
	chainFooterStart := job.chainFooterStart
	chainFooterEnd := job.chainFooterEnd
	chainFooterDigest := job.chainFooterDigest
	job.mu.RUnlock()
	omo := agentkind.Of(scope.Provider) == agentkind.OMO
	var source fileSource
	var code string
	var err error
	if chainContextID != "" {
		source, err = b.captureClaudeChainSource(chainLocation, foregroundBudget)
	} else {
		source, code = b.sourceFor(scope, omo)
	}
	if err != nil || code != "" {
		return false
	}
	if chainContextID != "" {
		if chainFooterStart < 0 || chainFooterEnd < chainFooterStart {
			source.close()
			return false
		}
		source.end = rangeEnd
	}
	sourceOwned := true
	defer func() {
		if sourceOwned {
			source.close()
		}
	}()
	if source.revision != sourceRevision || source.end < rangeEnd {
		return false
	}
	if chainContextID != "" {
		digest, digestErr := b.digestClaudeChainValidationRange(ctx, source.file, chainFooterStart, chainFooterEnd, foregroundBudget)
		if digestErr != nil || digest != chainFooterDigest {
			return false
		}
	} else if digest, err := fileRangeDigest(ctx, source.file, rangeStart, rangeEnd); err != nil || digest != rangeDigest {
		return false
	}
	b.observeCapacityOperation("retry", job, false)
	b.capacityMu.Lock()
	capacityLocked := true
	defer func() {
		if capacityLocked {
			b.unlockCapacity()
		}
	}()
	b.mu.Lock()
	if b.closing || len(b.queue) >= browseQueuedPreparations {
		b.mu.Unlock()
		return false
	}
	job.mu.Lock()
	if b.jobs[job.id] != job || job.retired || job.state != BrowseFailed || job.running || job.queued || job.source.revision != sourceRevision {
		job.mu.Unlock()
		b.mu.Unlock()
		return false
	}
	oldIndex := job.index
	oldSourceFile := job.source.file
	boundary := job.boundary
	job.mu.Unlock()
	b.mu.Unlock()
	if code := b.ensureCapacity(job, nil, "retry", true); code != "" {
		return false
	}
	snapshotID, err := randomIdentifier()
	if err != nil {
		return false
	}
	index, code, err := b.openSnapshotWithCapacity(job, "retry", snapshotID, source, boundary)
	if err != nil || code != "" {
		return false
	}
	b.mu.Lock()
	job.mu.Lock()
	if b.closing || b.jobs[job.id] != job || job.retired || job.running || job.queued {
		job.mu.Unlock()
		b.mu.Unlock()
		_ = index.remove()
		return false
	}
	job.source = source
	sourceOwned = false
	job.snapshotID = snapshotID
	job.index = index
	job.state = BrowsePreparing
	job.phase = "queued"
	job.scannedBytes = 0
	job.sourceBytes = source.end
	job.autoRetried = true
	job.sourceDigest = ""
	job.validatedSize = 0
	job.validatedModTime = 0
	job.validatedChangeToken = ""
	job.chainValidationKey = ""
	job.chainValidationRunning = false
	job.chainValidationErr = nil
	job.chainValidationDone = false
	job.err = nil
	job.queued = true
	job.lastInterest = time.Now()
	job.lastAccess = time.Now()
	b.queue = append(b.queue, job)
	job.mu.Unlock()
	b.mu.Unlock()
	if oldSourceFile != nil {
		_ = oldSourceFile.Close()
	}
	if oldIndex != nil {
		_ = oldIndex.remove()
	}
	b.unlockCapacity()
	capacityLocked = false
	b.signalWake()
	if job.chainContextID != "" {
		b.observe(browserObservation{kind: "chain_queue_admission", job: job})
	}
	return true
}

func (b *Browser) preparingPage(job *browseJob, cursor string) BrowsePage {
	job.mu.RLock()
	progress := &BrowseProgress{Phase: job.phase, ScannedBytes: job.scannedBytes, SourceBytes: job.sourceBytes}
	job.mu.RUnlock()
	return BrowsePage{
		Available: true, State: BrowsePreparing, Mode: BrowseRecent, Entries: []Entry{}, HasMore: cursor != "",
		Reason: "Preparing older history on this computer…", NextCursor: cursor, Progress: progress,
	}
}

func validateSnapshotSource(source fileSource) error {
	if source.file == nil {
		return errors.New("conversation source is unavailable")
	}
	info, err := source.file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("conversation source is unavailable")
		}
		return err
	}
	if !os.SameFile(source.info, info) || source.stamp().compare(info) == sourceChanged {
		return errors.New("conversation source changed")
	}
	pathInfo, err := os.Stat(source.path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) || containedRegularFile(source.path, source.location.Root) != source.path {
		return errors.New("conversation source changed")
	}
	resolved, err := filepath.EvalSymlinks(source.path)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(source.path) {
		return errors.New("conversation source changed")
	}
	return nil
}

func (b *Browser) ensureClaudeChainJobValidation(job *browseJob, source fileSource, expectedDigest string) error {
	if job == nil || source.file == nil || expectedDigest == "" {
		return errors.New("conversation snapshot has no source digest")
	}
	currentInfo, err := source.file.Stat()
	if err != nil {
		return err
	}
	job.mu.RLock()
	chainSegment := job.chainSegment
	chainFooterStart := job.chainFooterStart
	chainFooterEnd := job.chainFooterEnd
	chainFooterDigest := job.chainFooterDigest
	chainEvidence := append([]claudeRangeEvidence(nil), job.chainObservedRanges...)
	job.mu.RUnlock()
	candidate := claudeSegment{
		SessionID: source.location.Path, Location: source.location,
		CapturedEnd: source.end, FileRevision: source.revision,
		SourceModTime: source.info.ModTime().UnixNano(), SourceChangeToken: fileChangeToken(source.info),
		FileIdentity: fileIdentity(source.info),
		FooterStart:  chainFooterStart, FooterEnd: chainFooterEnd, FooterDigest: chainFooterDigest,
	}
	key := fmt.Sprintf("%s:%d:%d:%s:%d:%s", claudeChainValidationKey(candidate, chainEvidence),
		currentInfo.Size(), currentInfo.ModTime().UnixNano(), fileChangeToken(currentInfo), chainSegment, expectedDigest)
	job.mu.Lock()
	if job.chainValidationKey == key {
		if job.chainValidationRunning {
			job.mu.Unlock()
			return errClaudeChainValidationPending
		}
		if job.chainValidationDone {
			err := job.chainValidationErr
			job.mu.Unlock()
			if errors.Is(err, errClaudeChainValidationStale) {
				return errClaudeChainValidationPending
			}
			return err
		}
	}
	job.chainValidationKey = key
	job.chainValidationRunning = true
	job.chainValidationDone = false
	job.chainValidationErr = nil
	job.mu.Unlock()

	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		job.mu.Lock()
		job.chainValidationKey = ""
		job.chainValidationRunning = false
		job.chainValidationDone = false
		job.chainValidationErr = errors.New("conversation browser is closed")
		job.mu.Unlock()
		return errors.New("conversation browser is closed")
	}
	if b.chainValidationActive >= maxClaudeChainValidationTasks {
		b.mu.Unlock()
		job.mu.Lock()
		job.chainValidationKey = ""
		job.chainValidationRunning = false
		job.chainValidationDone = false
		job.mu.Unlock()
		return errClaudeChainValidationPending
	}
	b.chainValidationActive++
	b.validationWG.Add(1)
	b.mu.Unlock()
	location := source.location
	capturedEnd := source.end
	expected := stampOfInfo(currentInfo)
	captured := expected
	captured.size = capturedEnd
	go func() {
		defer b.validationWG.Done()
		validationErr := error(nil)
		validated, err := captureFileSourceWithObserver(location, b.observeBackgroundRead)
		if err != nil {
			validationErr = err
		} else {
			defer validated.close()
			if captured.compare(validated.info) == sourceChanged {
				validationErr = errors.New("conversation source changed")
			} else {
				b.observeRangeRead(capturedEnd)
				var digest string
				digest, validationErr = fileRangeDigestObserved(b.ctx, validated.file, 0, capturedEnd, nil, b.observeBackgroundRead)
				if validationErr == nil && digest != expectedDigest {
					validationErr = errors.New("conversation source changed")
				}
				if validationErr == nil {
					latest, statErr := validated.file.Stat()
					switch {
					case statErr != nil || latest.Size() < capturedEnd:
						validationErr = errors.New("conversation source changed")
					default:
						switch expected.compare(latest) {
						case sourceChanged:
							validationErr = errors.New("conversation source changed")
						case sourceGrown:
							// The source advanced after this candidate was captured. Do
							// not admit the result; the next request has a new key.
							validationErr = errClaudeChainValidationStale
						}
					}
				}
			}
		}
		job.mu.Lock()
		if job.chainValidationKey == key {
			job.chainValidationRunning = false
			job.chainValidationDone = true
			job.chainValidationErr = validationErr
		}
		job.mu.Unlock()
		b.mu.Lock()
		if b.chainValidationActive > 0 {
			b.chainValidationActive--
		}
		b.mu.Unlock()
	}()
	return errClaudeChainValidationPending
}

func (b *Browser) validateSnapshotJob(ctx context.Context, job *browseJob, source fileSource, foregroundBudget *claudeChainForegroundBudget) error {
	if err := validateSnapshotSource(source); err != nil {
		return err
	}
	if source.file == nil {
		return errors.New("conversation source is unavailable")
	}
	info, err := source.file.Stat()
	if err != nil {
		return err
	}
	job.mu.RLock()
	validatedSize := job.validatedSize
	validatedModTime := job.validatedModTime
	validatedChangeToken := job.validatedChangeToken
	expectedDigest := job.sourceDigest
	chainContextID := job.chainContextID
	chainFooterStart := job.chainFooterStart
	chainFooterEnd := job.chainFooterEnd
	chainFooterDigest := job.chainFooterDigest
	job.mu.RUnlock()
	modTime := info.ModTime().UnixNano()
	changeToken := fileChangeToken(info)
	if expectedDigest == "" {
		return errors.New("conversation snapshot has no source digest")
	}
	currentRevision, err := fileRevisionWithReservation(source.file, info, reserveClaudeChainPhysical(foregroundBudget), b.observeValidationRead)
	if errors.Is(err, errClaudeChainForegroundBudget) {
		return errClaudeChainValidationPending
	}
	if err != nil || currentRevision != source.revision {
		return errors.New("conversation source changed")
	}
	if info.Size() == validatedSize && modTime == validatedModTime &&
		(validatedChangeToken == "" || changeToken == validatedChangeToken) {
		return nil
	}
	if chainContextID != "" && info.Size() >= source.end {
		// Footer validation gives a cheap early rejection. If the source metadata
		// changed, the worker-owned captured digest must also authenticate the
		// entire frozen range. Large ranges are checked by a cancellable browser
		// validation task rather than by this request goroutine.
		if chainFooterStart < 0 || chainFooterEnd < chainFooterStart || chainFooterEnd > source.end {
			return errors.New("conversation source changed")
		}
		if chainFooterDigest != "" {
			digest, digestErr := b.digestClaudeChainValidationRange(ctx, source.file, chainFooterStart, chainFooterEnd, foregroundBudget)
			if errors.Is(digestErr, errClaudeChainForegroundBudget) {
				return errClaudeChainValidationPending
			}
			if digestErr != nil || digest != chainFooterDigest {
				return errors.New("conversation source changed")
			}
		}
		if validationErr := b.ensureClaudeChainJobValidation(job, source, expectedDigest); validationErr != nil {
			return validationErr
		}
		job.mu.Lock()
		job.validatedSize = info.Size()
		job.validatedModTime = modTime
		job.validatedChangeToken = changeToken
		job.mu.Unlock()
		return nil
	}
	if info.Size() > validatedSize && info.Size() >= source.end {
		job.mu.Lock()
		job.validatedSize = info.Size()
		job.validatedModTime = modTime
		job.mu.Unlock()
		return nil
	}
	if foregroundBudget != nil && !foregroundBudget.takeValidation(source.end) {
		return errClaudeChainValidationPending
	}
	b.observeRangeRead(source.end)
	digest, err := fileRangeDigestObserved(ctx, source.file, 0, source.end, nil, func(bytes int64) error {
		b.observeValidationPhysicalRead(bytes)
		return nil
	})
	if err != nil {
		return err
	}
	if digest != expectedDigest {
		return errors.New("conversation source changed")
	}
	job.mu.Lock()
	job.validatedSize = info.Size()
	job.validatedModTime = modTime
	job.validatedChangeToken = changeToken
	job.mu.Unlock()
	return nil
}

func (b *Browser) runJob(job *browseJob) {
	defer b.workerWG.Done()
	defer func() {
		b.observe(browserObservation{kind: "worker_before_finish", job: job})
		job.mu.Lock()
		job.running = false
		if job.err != nil {
			job.state = BrowseFailed
			job.phase = "failed"
		}
		job.mu.Unlock()
		b.done <- job
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			b.failJob(job, "index_failed", "History preparation failed on this computer.", true)
		}
	}()
	if job.chainContextID != "" {
		b.observe(browserObservation{kind: "chain_worker_before_capture"})
	}
	job.mu.Lock()
	job.phase = "scanning"
	job.scannedBytes = 0
	job.sourceBytes = job.source.end
	file := job.source.file
	chainContextID := job.chainContextID
	chainEvidence := append([]claudeRangeEvidence(nil), job.chainObservedRanges...)
	chainIdentity := fileIdentity(job.source.info)
	capturedStamp := job.source.stamp()
	job.mu.Unlock()
	if chainContextID != "" {
		b.observeChainWorkerCapture(chainEvidence)
	}
	if file == nil {
		b.failJob(job, "source_unavailable", "The conversation source became unavailable.", true)
		return
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(job.source.info, info) || capturedStamp.compare(info) == sourceChanged {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
	}
	if resolved, resolveErr := filepath.EvalSymlinks(job.source.path); resolveErr != nil || filepath.Clean(resolved) != filepath.Clean(job.source.path) || containedRegularFile(job.source.path, job.source.location.Root) != job.source.path {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
	}
	if chainContextID != "" {
		if err := b.validateClaudeChainJobEvidence(file, job.source.end, chainIdentity, chainEvidence); err != nil {
			if errors.Is(err, context.Canceled) {
				b.failJob(job, "request_cancelled", "History preparation was cancelled.", true)
			} else {
				b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
			}
			return
		}
		// Keep the evidence fence immediately adjacent to the indexing pass. A
		// valid range may be rewritten after the admission read and before the
		// scan establishes a new full digest; without this second check the
		// rewritten bytes could be published as the old cursor's snapshot.
		b.observe(browserObservation{kind: "chain_preparation"})
	}
	scanDigest := sha256.New()
	records, err := NewJSONLRecordReader(b.ctx, file, 0, job.source.end, b.options.MaxRecordBytes, func(progress JSONLRecordProgress) {
		job.mu.Lock()
		job.scannedBytes = progress.ScannedBytes
		job.sourceBytes = progress.SourceBytes
		job.mu.Unlock()
	})
	if err != nil {
		b.failJob(job, "index_failed", "History preparation could not read the source.", true)
		return
	}
	records.SetDigestWriter(scanDigest)
	records.SetReadObserver(func(bytes int64) { _ = b.observeBackgroundRead(bytes) })
	records.SetCheckpoint(func() error {
		if !b.jobHasInterest(job) {
			return errJSONLCheckpoint
		}
		return nil
	})
	diagnostics := BrowseDiagnostics{}
	var plan *OMOTodoState
	todoSeen, todoValid := false, false
	recordCount := 0
	const maxBatchRecords = 64
	const maxBatchBytes = 4 * 1024 * 1024
	for {
		batch := make([]JSONLRecord, 0, maxBatchRecords)
		batchBytes := 0
		eof := false
		for len(batch) < maxBatchRecords && batchBytes < maxBatchBytes {
			if !b.jobHasInterest(job) {
				b.failJob(job, "index_failed", "History preparation paused because no client is waiting for it.", true)
				return
			}
			record, nextErr := records.Next()
			if errors.Is(nextErr, io.EOF) {
				eof = true
				break
			}
			if nextErr != nil {
				code := "index_failed"
				retryable := true
				message := "History preparation could not read the source."
				if errors.Is(nextErr, context.Canceled) {
					code, message = "request_cancelled", "History preparation was cancelled."
				} else if errors.Is(nextErr, errJSONLSourceTruncated) {
					code, retryable, message = "source_changed", false, "The conversation source changed while it was being prepared."
				} else if errors.Is(nextErr, errJSONLCheckpoint) {
					message = "History preparation paused because no client is waiting for it."
				}
				b.failJob(job, code, message, retryable)
				return
			}
			batch = append(batch, record)
			batchBytes += len(record.Raw)
		}
		if len(batch) == 0 {
			break
		}
		var results []projectionResult
		if code := b.writeWithCapacity(job, "batch", "History preparation could not store its index.", func() error {
			var err error
			results, err = job.index.applyRecords(job.scope.Provider, job.source.revision, batch)
			return err
		}); code != "" {
			return
		}
		for _, result := range results {
			diagnostics.OversizedRecords += result.Diagnostics.OversizedRecords
			diagnostics.CorruptRecords += result.Diagnostics.CorruptRecords
			diagnostics.OmittedTools += result.Diagnostics.OmittedTools
			diagnostics.OmittedPayloads += result.Diagnostics.OmittedPayloads
			diagnostics.PlanCorrupt = diagnostics.PlanCorrupt || result.Diagnostics.PlanCorrupt
			if result.Plan != nil {
				plan = result.Plan
				todoValid = true
			}
			todoSeen = todoSeen || result.TodoSeen
			recordCount++
		}
		if eof {
			break
		}
	}
	job.mu.Lock()
	job.phase = "verifying"
	job.mu.Unlock()
	firstDigest := hex.EncodeToString(scanDigest.Sum(nil))
	job.mu.RLock()
	chainDigest := job.chainContextID != ""
	expectedRangeDigest := job.rangeDigest
	job.mu.RUnlock()
	if chainDigest && expectedRangeDigest != "" && firstDigest != expectedRangeDigest {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
	}
	b.observe(browserObservation{kind: "source_read", bytes: job.source.end})
	secondDigest, digestErr := fileRangeDigestObserved(b.ctx, file, 0, job.source.end, func() error {
		if !b.jobHasInterest(job) {
			return errJSONLCheckpoint
		}
		return nil
	}, b.observeBackgroundRead)
	if errors.Is(digestErr, errJSONLCheckpoint) {
		b.failJob(job, "index_failed", "History preparation paused because no client is waiting for it.", true)
		return
	}
	if digestErr != nil || firstDigest != secondDigest {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
	}
	if chainDigest {
		if err := b.validateClaudeChainJobEvidence(file, job.source.end, chainIdentity, chainEvidence); err != nil {
			if errors.Is(err, context.Canceled) {
				b.failJob(job, "request_cancelled", "History preparation was cancelled.", true)
			} else {
				b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
			}
			return
		}
	}
	if chainDigest && expectedRangeDigest == "" {
		job.mu.Lock()
		job.rangeDigest = secondDigest
		job.mu.Unlock()
	}
	if err := validateSnapshotSource(job.source); err != nil {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
	}
	if agentkind.Of(job.scope.Provider) == agentkind.OMO {
		if todoSeen && !todoValid {
			diagnostics.CorruptRecords++
			diagnostics.PlanCorrupt = true
		}
		if plan != nil {
			copyPlan := *plan
			copyPlan.SessionID = job.scope.SessionID
			copyPlan.Available = true
			plan = &copyPlan
		}
	}
	if !b.jobHasInterest(job) {
		b.failJob(job, "index_failed", "History preparation paused because no client is waiting for it.", true)
		return
	}
	metadata, metadataErr := job.index.metadata()
	if metadataErr != nil {
		b.failJob(job, "index_failed", "The prepared conversation index is corrupt.", true)
		return
	}
	count, countErr := job.index.countWithCheckpoint(func() error {
		if !b.jobHasInterest(job) {
			return errJSONLCheckpoint
		}
		return nil
	})
	if errors.Is(countErr, errJSONLCheckpoint) {
		b.failJob(job, "index_failed", "History preparation paused because no client is waiting for it.", true)
		return
	}
	if countErr != nil {
		b.failJob(job, "index_failed", "The prepared conversation index is corrupt.", true)
		return
	}
	metadata.State = "ready"
	metadata.SourceDigest = secondDigest
	metadata.VisibleEntries = count
	metadata.OversizedRecords = diagnostics.OversizedRecords
	metadata.CorruptRecords = diagnostics.CorruptRecords
	metadata.OmittedTools = diagnostics.OmittedTools
	metadata.OmittedPayloads = diagnostics.OmittedPayloads
	metadata.PlanCorrupt = diagnostics.PlanCorrupt
	metadata.Plan = plan
	metadata.UpdatedAt = time.Now().UTC()
	if !b.jobHasInterest(job) {
		b.failJob(job, "index_failed", "History preparation paused because no client is waiting for it.", true)
		return
	}
	if code := b.writeWithCapacity(job, "finalize", "The prepared conversation index could not be finalized.", func() error {
		if err := job.index.setMetadata(metadata); err != nil {
			return err
		}
		return job.index.sync()
	}); code != "" {
		return
	}
	verifiedInfo, _ := file.Stat()
	job.mu.Lock()
	chainContextID = job.chainContextID
	chainSegment := job.chainSegment
	job.state = BrowseReady
	job.phase = "ready"
	job.diagnostics = diagnostics
	job.plan = cloneTodo(plan)
	job.total = count
	job.sourceDigest = secondDigest
	if verifiedInfo != nil {
		job.validatedSize = verifiedInfo.Size()
		job.validatedModTime = verifiedInfo.ModTime().UnixNano()
		job.validatedChangeToken = fileChangeToken(verifiedInfo)
	}
	job.lastAccess = time.Now()
	job.err = nil
	job.mu.Unlock()
	if chainContextID != "" {
		b.mu.Lock()
		if chain := b.chains[chainContextID]; chain != nil && chainSegment >= 0 && chainSegment < len(chain.chain.Segments) {
			chain.chain.Segments[chainSegment].CapturedDigest = secondDigest
			b.updateClaudeChainLineageEvidenceLocked(chain)
		}
		b.mu.Unlock()
	}
}

func (b *Browser) jobHasInterest(job *browseJob) bool {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return time.Since(job.lastInterest) <= b.options.InterestLease
}

func (b *Browser) failJob(job *browseJob, code, message string, retryable bool) {
	b.capacityMu.Lock()
	defer b.unlockCapacity()
	b.failJobCapacityLocked(job, code, message, retryable)
}

func (b *Browser) failJobCapacityLocked(job *browseJob, code, message string, retryable bool) {
	failure := browseErrorFor(code, message, retryable)
	job.mu.Lock()
	job.err = failure
	index := job.index
	job.index = nil
	sourceFile := job.source.file
	job.source.file = nil
	job.mu.Unlock()
	_ = b.removeCapacityItem(expiredJob{index: index, sourceFile: sourceFile})
}
