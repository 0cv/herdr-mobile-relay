package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
)

type BrowseScope struct {
	Provider        string `json:"provider"`
	CWD             string `json:"cwd"`
	ForegroundCWD   string `json:"foreground_cwd,omitempty"`
	SessionID       string `json:"session_id"`
	PaneID          string `json:"pane_id"`
	ServerSessionID string `json:"server_session_id"`
	TerminalID      string `json:"terminal_id"`
	Generation      int64  `json:"generation"`
	ProcessIdentity string `json:"process_identity,omitempty"`
	SelectionReason string `json:"selection_reason,omitempty"`
	InferredSession bool   `json:"inferred_session,omitempty"`
	SelectionSource string `json:"selection_source,omitempty"`
}

type BrowseRequest struct {
	Scope  BrowseScope
	Cursor string
	Limit  int
	Retry  bool
}

type BrowseState string

const (
	BrowseReady     BrowseState = "ready"
	BrowsePreparing BrowseState = "preparing"
	BrowseFailed    BrowseState = "failed"
)

type BrowseMode string

const (
	BrowseRecent   BrowseMode = "recent"
	BrowseSnapshot BrowseMode = "snapshot"
	BrowseNative   BrowseMode = "native"
)

type BrowseProgress struct {
	Phase        string `json:"phase"`
	ScannedBytes int64  `json:"scanned_bytes"`
	SourceBytes  int64  `json:"source_bytes"`
}

type BrowseDiagnostics struct {
	OversizedRecords       int    `json:"oversized_records"`
	CorruptRecords         int    `json:"corrupt_records"`
	OmittedTools           int    `json:"omitted_tools,omitempty"`
	OmittedPayloads        int    `json:"omitted_payloads,omitempty"`
	PlanCorrupt            bool   `json:"plan_corrupt,omitempty"`
	SourceTruncated        bool   `json:"source_truncated,omitempty"`
	ContinuationIncomplete bool   `json:"continuation_incomplete,omitempty"`
	ContinuationReason     string `json:"continuation_reason,omitempty"`
}

type BrowseError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type SourceCapabilities struct {
	Source       string `json:"source"`
	ActiveBranch bool   `json:"active_branch"`
}

type BrowsePage struct {
	SubscriptionID string              `json:"subscription_id,omitempty"`
	RecordedEdits  map[string]bool     `json:"-"`
	Provisional    []Entry             `json:"provisional,omitempty"`
	Sequence       uint64              `json:"sequence,omitempty"`
	Capabilities   *SourceCapabilities `json:"capabilities,omitempty"`
	LiveState      string              `json:"live_state,omitempty"`
	Available      bool                `json:"available"`
	ReasonCode     string              `json:"reason_code,omitempty"`
	Reason         string              `json:"reason,omitempty"`
	Entries        []Entry             `json:"entries"`
	NextCursor     string              `json:"next_cursor,omitempty"`
	HasMore        bool                `json:"has_more"`
	State          BrowseState         `json:"state"`
	Mode           BrowseMode          `json:"mode"`
	SourceRevision string              `json:"source_revision,omitempty"`
	SnapshotID     string              `json:"snapshot_id,omitempty"`
	Total          *int                `json:"total"`
	Progress       *BrowseProgress     `json:"progress,omitempty"`
	Diagnostics    BrowseDiagnostics   `json:"diagnostics"`
	Error          *BrowseError        `json:"error,omitempty"`
	OMOPlan        *OMOTodoState       `json:"omo_plan,omitempty"`

	recordedPending []string
}

const (
	browseResponseBytes      = 2 * 1024 * 1024
	browseQueuedPreparations = 4
)

type BrowserOptions struct {
	CapacityObserver     func(CapacityEvent)
	RecentBytes          int64
	MaxRecordBytes       int64
	DefaultPageSize      int
	MaxPageSize          int
	ActiveWorkers        int
	InterestLease        time.Duration
	SnapshotTTL          time.Duration
	SnapshotQuota        int64
	AggregateQuota       int64
	CursorTTL            time.Duration
	RecentCacheEntries   int
	RecentCacheItemBytes int64
	RecentCacheBytes     int64
	RecentCacheTTL       time.Duration
}

func DefaultBrowserOptions() BrowserOptions {
	return BrowserOptions{
		RecentBytes:          256 * 1024,
		MaxRecordBytes:       16 * 1024 * 1024,
		DefaultPageSize:      defaultPageSize,
		MaxPageSize:          maxPageSize,
		ActiveWorkers:        1,
		InterestLease:        30 * time.Second,
		SnapshotTTL:          15 * time.Minute,
		SnapshotQuota:        512 * 1024 * 1024,
		AggregateQuota:       1024 * 1024 * 1024,
		CursorTTL:            15 * time.Minute,
		RecentCacheEntries:   4,
		RecentCacheItemBytes: 16 * 1024 * 1024,
		RecentCacheBytes:     64 * 1024 * 1024,
		RecentCacheTTL:       60 * time.Second,
	}
}

type fileSource struct {
	location Location
	path     string
	file     *os.File
	info     os.FileInfo
	end      int64
	revision string
}

func (source *fileSource) close() {
	if source == nil || source.file == nil {
		return
	}
	_ = source.file.Close()
	source.file = nil
}

type browserObservation struct {
	kind       string
	bytes      int64
	stage      string
	job        *browseJob
	locked     bool
	chain      claudeChain
	afterBuild bool
	evidence   []claudeRangeEvidence
	work       kimiMaterializationWork
}

type Browser struct {
	reader    *Reader
	cacheRoot string
	options   BrowserOptions
	key       [32]byte
	cacheErr  error

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan *browseJob
	closed chan struct{}

	capacityMu         sync.Mutex
	capacityEvent      *CapacityEvent
	capacityLast       time.Time
	capacitySuppressed int
	capacityNow        func() time.Time
	capacityRemove     func(*snapshotIndex) error

	mu                    sync.Mutex
	jobs                  map[string]*browseJob
	dedup                 map[string]string
	queue                 []*browseJob
	chains                map[string]*claudeChainContext
	chainByKey            map[string]string
	chainLineage          map[string]claudeChainLineage
	chainLineageIdentity  map[string]claudeChainLineageIdentity
	chainValidations      map[string]*claudeChainValidation
	chainCompactions      map[string]*claudeChainEvidenceCompaction
	chainValidationActive int
	chainCompactionActive int
	chainReservations     int
	active                int
	closing               bool

	schedulerWG  sync.WaitGroup
	workerWG     sync.WaitGroup
	readWG       sync.WaitGroup
	validationWG sync.WaitGroup
	closeOnce    sync.Once

	observer       atomic.Pointer[func(browserObservation)]
	recentCache    *recentProjectionCache
	kimiProjection *kimiProjectionSnapshot
}

func NewBrowser(reader *Reader, cacheRoot string, options BrowserOptions) (*Browser, error) {
	if reader == nil {
		return nil, errors.New("conversation browser requires a reader")
	}
	key, err := newBrowseSigningKey()
	if err != nil {
		return nil, fmt.Errorf("create conversation browser key: %w", err)
	}
	options.MaxPageSize = max(options.MaxPageSize, options.DefaultPageSize)
	cacheErr := ensureBrowserCacheRoot(cacheRoot)
	if cacheErr == nil {
		cleanupBrowserCache(cacheRoot)
	}
	ctx, cancel := context.WithCancel(context.Background())
	browser := &Browser{
		reader: reader, cacheRoot: cacheRoot, options: options, key: key, cacheErr: cacheErr,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan *browseJob, options.ActiveWorkers+1),
		closed: make(chan struct{}), jobs: make(map[string]*browseJob), dedup: make(map[string]string),
		recentCache: newRecentProjectionCache(options),
		chains:      make(map[string]*claudeChainContext), chainByKey: make(map[string]string),
		chainLineage: make(map[string]claudeChainLineage), chainLineageIdentity: make(map[string]claudeChainLineageIdentity),
		chainValidations: make(map[string]*claudeChainValidation), chainCompactions: make(map[string]*claudeChainEvidenceCompaction),
	}
	browser.schedulerWG.Add(1)
	go browser.scheduler()
	return browser, nil
}

func cleanupBrowserCache(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "snapshot-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		_ = os.Remove(path)
	}
}

func ensureBrowserCacheRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("conversation history cache is disabled")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("conversation history cache cannot be a symlink")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("conversation history cache is not a directory")
	}
	return nil
}

func (b *Browser) Reader() *Reader {
	if b == nil {
		return nil
	}
	return b.reader
}

func (b *Browser) Close() error {
	var closeErr error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		b.cancel()
		b.schedulerWG.Wait()
		b.workerWG.Wait()
		b.readWG.Wait()
		b.kimiProjection = nil
		b.validationWG.Wait()
		b.capacityMu.Lock()
		defer b.unlockCapacity()
		b.mu.Lock()
		jobs := make([]*browseJob, 0, len(b.jobs))
		for _, job := range b.jobs {
			jobs = append(jobs, job)
		}
		b.jobs = make(map[string]*browseJob)
		b.dedup = make(map[string]string)
		b.chains = make(map[string]*claudeChainContext)
		b.chainByKey = make(map[string]string)
		b.chainLineage = make(map[string]claudeChainLineage)
		b.chainLineageIdentity = make(map[string]claudeChainLineageIdentity)
		b.chainValidations = make(map[string]*claudeChainValidation)
		b.chainCompactions = make(map[string]*claudeChainEvidenceCompaction)
		b.chainValidationActive = 0
		b.chainCompactionActive = 0
		b.chainReservations = 0
		b.mu.Unlock()
		if b.recentCache != nil {
			b.recentCache.clear()
		}
		for _, job := range jobs {
			job.mu.Lock()
			job.retired = true
			index := job.index
			job.index = nil
			sourceFile := job.source.file
			job.source.file = nil
			job.mu.Unlock()
			if sourceFile != nil {
				_ = sourceFile.Close()
			}
			if index != nil {
				if err := index.remove(); err != nil && !errors.Is(err, os.ErrNotExist) && closeErr == nil {
					closeErr = err
				}
			}
		}
		close(b.closed)
	})
	return closeErr
}

func (b *Browser) scheduler() {
	defer b.schedulerWG.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		b.launchQueued()
		select {
		case <-b.ctx.Done():
			return
		case <-b.done:
			b.finishJob()
		case <-b.wake:
		case now := <-ticker.C:
			b.evictExpired(now)
		}
	}
}

func (b *Browser) launchQueued() {
	for {
		b.mu.Lock()
		if b.closing || b.active >= b.options.ActiveWorkers || len(b.queue) == 0 {
			b.mu.Unlock()
			return
		}
		job := b.queue[0]
		b.queue = b.queue[1:]
		b.active++
		job.mu.Lock()
		job.queued = false
		job.running = true
		job.mu.Unlock()
		b.mu.Unlock()
		b.workerWG.Add(1)
		go b.runJob(job)
	}
}

func (b *Browser) finishJob() {
	b.mu.Lock()
	if b.active > 0 {
		b.active--
	}
	b.mu.Unlock()
	b.signalWake()
}

func (b *Browser) signalWake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Browser) observe(event browserObservation) {
	if observer := b.observer.Load(); observer != nil {
		(*observer)(event)
	}
}

func (b *Browser) observePhysicalRead(bytes int64) {
	b.observeForegroundRead(bytes)
	b.observe(browserObservation{kind: "physical_read", bytes: bytes})
}

func (b *Browser) observeDiscoveryRead(bytes int64) {
	b.observeForegroundRead(bytes)
	b.observe(browserObservation{kind: "discovery_read", bytes: bytes})
}

func (b *Browser) observeRangeRead(bytes int64) {
	if bytes > 0 {
		b.observe(browserObservation{kind: "source_read", bytes: bytes})
	}
}

func (b *Browser) observeValidationPhysicalRead(bytes int64) {
	b.observeForegroundRead(bytes)
	if bytes > 0 {
		b.observe(browserObservation{kind: "validation_read", bytes: bytes})
	}
}

func (b *Browser) observeForegroundRead(bytes int64) {
	if bytes > 0 {
		b.observe(browserObservation{kind: "foreground_read", bytes: bytes})
	}
}

func (b *Browser) observeBackgroundRead(bytes int64) error {
	if bytes > 0 {
		b.observe(browserObservation{kind: "background_read", bytes: bytes})
		b.observe(browserObservation{kind: "validation_read", bytes: bytes})
	}
	return nil
}

func (b *Browser) observeValidationRead(bytes int64) error {
	b.observeValidationPhysicalRead(bytes)
	return nil
}

func (b *Browser) observeChainWorkerCapture(evidence []claudeRangeEvidence) {
	if b.observer.Load() != nil {
		b.observe(browserObservation{kind: "chain_worker_capture", evidence: append([]claudeRangeEvidence(nil), evidence...)})
	}
}

func (b *Browser) evictExpired(now time.Time) {
	b.observeCapacityOperation("expiry", nil, false)
	b.capacityMu.Lock()
	defer b.unlockCapacity()
	var expired []expiredJob
	b.mu.Lock()
	for _, job := range b.jobs {
		job.mu.Lock()
		if b.capacityEligibleLocked(job) && now.Sub(job.lastAccess) >= b.options.SnapshotTTL {
			expired = append(expired, b.detachJobLocked(job))
		}
		job.mu.Unlock()
	}
	for id, chain := range b.chains {
		if chain.refs == 0 && now.Sub(chain.lastAccess) >= b.options.CursorTTL {
			delete(b.chains, id)
			if b.chainByKey[chain.key] == id {
				delete(b.chainByKey, chain.key)
			}
		}
	}
	for scopeID, lineage := range b.chainLineage {
		if now.Sub(lineage.lastAccess) >= b.options.CursorTTL {
			delete(b.chainLineage, scopeID)
			delete(b.chainLineageIdentity, scopeID)
			for key, task := range b.chainCompactions {
				if task.scopeID == scopeID {
					delete(b.chainCompactions, key)
				}
			}
		}
	}
	for scopeID, identity := range b.chainLineageIdentity {
		if _, retained := b.chainLineage[scopeID]; !retained && now.Sub(identity.lastAccess) >= b.options.CursorTTL {
			delete(b.chainLineageIdentity, scopeID)
		}
	}
	b.mu.Unlock()
	for _, item := range expired {
		_ = b.removeCapacityItem(item)
	}
}

func (b *Browser) ReadPage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return failPage("index_failed", "Conversation history browsing is shutting down.", true), nil
	}
	b.readWG.Add(1)
	b.mu.Unlock()
	defer b.readWG.Done()
	if err := ctx.Err(); err != nil {
		return browseRequestCancelled(), nil
	}
	request.Scope = normalizeBrowseScope(request.Scope)
	request.Limit = b.limit(request.Limit)
	if IsKimi(request.Scope.Provider) {
		return b.readKimiPage(ctx, request)
	}
	if request.Scope.SessionID == "" {
		return browseUnavailable("invalid_session", "This agent has not reported a conversation session yet."), nil
	}
	switch agentkind.Of(request.Scope.Provider) {
	case "":
		return browseUnavailable("invalid_provider", "Conversation history is not available for this agent."), nil
	case agentkind.OpenCode:
		return b.readOpenCodePage(ctx, request)
	case agentkind.OMO:
		return b.readFilePage(ctx, request, true)
	case agentkind.Hermes:
		return b.readHermesPage(ctx, request)
	case agentkind.Agy:
		return b.readNativePage(ctx, request, "agy")
	case agentkind.Grok:
		return b.readNativePage(ctx, request, "grok")
	}
	return b.readFilePage(ctx, request, false)
}

func normalizeBrowseScope(scope BrowseScope) BrowseScope {
	scope.Provider = agentkind.Of(scope.Provider)
	project := normalizeBrowseProjectContext(scope.Provider, scope.CWD, scope.ForegroundCWD)
	scope.CWD = project.CWD
	scope.ForegroundCWD = project.ForegroundCWD
	scope.SessionID = strings.TrimSpace(scope.SessionID)
	scope.PaneID = strings.TrimSpace(scope.PaneID)
	scope.ServerSessionID = strings.TrimSpace(scope.ServerSessionID)
	scope.TerminalID = strings.TrimSpace(scope.TerminalID)
	if scope.ServerSessionID == "" {
		scope.ServerSessionID = "primary"
	}
	if scope.Generation < 0 {
		scope.Generation = 0
	}
	return scope
}

func (b *Browser) limit(value int) int {
	if value < 1 {
		return b.options.DefaultPageSize
	}
	if value > b.options.MaxPageSize {
		return b.options.MaxPageSize
	}
	return value
}

func browseUnavailable(code, reason string) BrowsePage {
	return BrowsePage{
		Available: false, ReasonCode: code, Reason: reason, Entries: []Entry{}, State: BrowseReady,
		Mode: BrowseRecent, Diagnostics: BrowseDiagnostics{}, Total: nil,
	}
}

func browseFailure(code, reason string, failure *BrowseError) BrowsePage {
	return BrowsePage{
		Available: true, ReasonCode: code, Reason: reason, Entries: []Entry{}, State: BrowseFailed,
		Mode: BrowseRecent, Diagnostics: BrowseDiagnostics{}, Error: failure,
	}
}

func failPage(code, message string, retryable bool) BrowsePage {
	return browseFailure(code, message, browseErrorFor(code, message, retryable))
}

func browseRequestCancelled() BrowsePage {
	return failPage("request_cancelled", "History request was cancelled.", true)
}

func browseSourceChanged() BrowsePage {
	return failPage("source_changed", "The conversation source changed while history was being browsed.", false)
}

func browseContinuationFailed() BrowsePage {
	return failPage("index_failed", "History continuation could not be created.", true)
}

func browseInvalidCursor() BrowsePage {
	return failPage("invalid_cursor", "This history cursor is invalid.", false)
}

func browseInvalidConversationCursor() BrowsePage {
	return failPage("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)
}

func browseInvalidSnapshotCursor() BrowsePage {
	return failPage("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)
}

func browseErrorFor(code, message string, retryable bool) *BrowseError {
	return &BrowseError{Code: code, Message: message, Retryable: retryable}
}

func (b *Browser) readFilePage(ctx context.Context, request BrowseRequest, omo bool) (BrowsePage, error) {
	if request.Cursor == "" {
		if !omo && agentkind.Of(request.Scope.Provider) == agentkind.Claude {
			return b.readClaudeChainPage(ctx, request)
		}
		return b.readRecentFilePage(ctx, request, omo)
	}
	cursor, err := decodeBrowseCursor(b.key, request.Cursor, request.Scope)
	if err != nil {
		code, reason := browseCursorError(err)
		return failPage(code, reason, code == "cursor_expired"), nil
	}
	switch cursor.Mode {
	case "recent":
		return b.readRecentCursorPage(ctx, request, cursor, omo)
	case "prepare":
		return b.readPreparePage(ctx, request, cursor, omo)
	case "snapshot":
		return b.readSnapshotCursorPage(ctx, request, cursor)
	case "chain":
		if agentkind.Of(request.Scope.Provider) != agentkind.Claude {
			return browseInvalidConversationCursor(), nil
		}
		return b.readClaudeChainCursorPage(ctx, request, cursor)
	default:
		return browseInvalidConversationCursor(), nil
	}
}

func (b *Browser) sourceFor(scope BrowseScope, omo bool) (fileSource, string) {
	scope = normalizeBrowseScope(scope)
	var location Location
	var expectedOMO *omoIdentity
	if omo {
		located, identity, code := b.reader.locateOMO(scope.CWD, scope.SessionID)
		if code != "" {
			return fileSource{}, code
		}
		location = located
		expectedOMO = &identity
	} else {
		location = b.reader.LocateWithProject(scope.Provider, ProjectContext{
			CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD,
		}, scope.SessionID)
		if location.Path == "" {
			return fileSource{}, "invalid_session"
		}
	}
	source, err := captureFileSourceWithObserver(location, func(bytes int64) error {
		b.observePhysicalRead(bytes)
		return nil
	})
	if err != nil {
		return fileSource{}, "source_unavailable"
	}
	if expectedOMO != nil && !verifyOMOIdentityFile(source.file, *expectedOMO, scope.CWD) {
		source.close()
		return fileSource{}, "invalid_session"
	}
	return source, ""
}

func captureFileSource(location Location) (fileSource, error) {
	return captureFileSourceWithObserver(location, nil)
}

// captureFileSourceWithObserver is the same containment-checked source open,
// with an optional accounting hook for the bounded identity-anchor read. The
// hook is called with bytes actually returned by the source read.
func captureFileSourceWithObserver(location Location, observe func(int64) error) (fileSource, error) {
	return captureFileSourceWithHooks(location, nil, observe)
}

func captureFileSourceWithHooks(location Location, reserve func(int64) error, observe func(int64) error) (fileSource, error) {
	file, info, err := openContainedConversationFile(location)
	if err != nil {
		return fileSource{}, err
	}
	closeSource := true
	defer func() {
		if closeSource {
			_ = file.Close()
		}
	}()
	revision, err := fileRevisionWithReservation(file, info, reserve, observe)
	if err != nil {
		return fileSource{}, err
	}
	closeSource = false
	return fileSource{location: location, path: location.Path, file: file, info: info, end: info.Size(), revision: revision}, nil
}

// openContainedConversationFile performs the same no-follow, regular-file,
// containment, and identity checks as a source capture without reading any
// transcript bytes. Completed background validation uses it to recheck the
// descriptor metadata cheaply before trusting a cached result; charging a
// second 64 KiB first-record read there would make an aggregate validation
// request unable to authenticate several segments in one pass.
func openContainedConversationFile(location Location) (*os.File, os.FileInfo, error) {
	if location.Path == "" || location.Root == "" {
		return nil, nil, errors.New("conversation source is not contained")
	}
	resolved := containedRegularFile(location.Path, location.Root)
	if resolved == "" || filepath.Clean(resolved) != filepath.Clean(location.Path) {
		return nil, nil, errors.New("conversation source is not contained")
	}
	file, err := openConversationSource(location.Path)
	if err != nil {
		return nil, nil, err
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("conversation source is not a regular file")
		}
		return nil, nil, err
	}
	resolvedAgain, err := filepath.EvalSymlinks(location.Path)
	if err != nil || filepath.Clean(resolvedAgain) != filepath.Clean(location.Path) || containedRegularFile(location.Path, location.Root) != location.Path {
		return nil, nil, errors.New("conversation source changed while opening")
	}
	pathInfo, err := os.Stat(location.Path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return nil, nil, errors.New("conversation source changed while opening")
	}
	closeFile = false
	return file, info, nil
}

func fileRevisionWithReservation(file *os.File, info os.FileInfo, reserve func(int64) error, observe func(int64) error) (string, error) {
	const anchorBytes = int64(64 * 1024)
	digest := sha256.New()
	sectionSize := info.Size()
	if sectionSize > anchorBytes {
		sectionSize = anchorBytes
	}
	if sectionSize > 0 {
		// ReadAt makes the accounting reflect the physical bounded anchor read,
		// rather than the amount of the first line retained by bufio.ReadBytes.
		// Reserve the requested read before issuing it so a foreground budget
		// cannot be exceeded by a filesystem chunk returned before observation.
		if reserve != nil {
			if err := reserve(sectionSize); err != nil {
				return "", err
			}
		}
		anchor := make([]byte, sectionSize)
		read, readErr := file.ReadAt(anchor, 0)
		if read > 0 {
			if observe != nil {
				if err := observe(int64(read)); err != nil {
					return "", err
				}
			}
			firstRecord := anchor[:read]
			if newline := bytes.IndexByte(firstRecord, '\n'); newline >= 0 {
				firstRecord = firstRecord[:newline+1]
			}
			_, _ = digest.Write(firstRecord)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
	}
	result := sha256.New()
	_, _ = io.WriteString(result, fileIdentity(info))
	_, _ = io.WriteString(result, "\x00")
	_, _ = io.WriteString(result, hex.EncodeToString(digest.Sum(nil)))
	return hex.EncodeToString(result.Sum(nil)), nil
}

func fileIdentity(info os.FileInfo) string {
	system := info.Sys()
	value := reflect.ValueOf(system)
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	parts := make([]string, 0, 5)
	if value.IsValid() && value.Kind() == reflect.Struct {
		for _, name := range []string{"Dev", "Ino", "VolumeSerialNumber", "FileIndexHigh", "FileIndexLow", "Index"} {
			field := value.FieldByName(name)
			if field.IsValid() && field.CanInterface() {
				parts = append(parts, name+"="+fmt.Sprint(field.Interface()))
			}
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%T", system)
	}
	return strings.Join(parts, ",")
}

// fileChangeToken is a mutation token in addition to ModTime. On Unix the
// change time changes for an in-place rewrite even when a caller restores the
// modification time; unlike atime, it is not updated by a normal read. The
// fallback keeps the check useful on platforms whose FileInfo does not expose
// a change-time field.
func fileChangeToken(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		for _, name := range []string{"Ctim", "Ctimespec", "ChangeTime", "CTime"} {
			field := value.FieldByName(name)
			if field.IsValid() && field.CanInterface() {
				return name + "=" + fmt.Sprint(field.Interface())
			}
		}
	}
	return fmt.Sprintf("mtime=%d", info.ModTime().UnixNano())
}

type sourceChange int

const (
	sourceUnchanged sourceChange = iota
	sourceGrown
	sourceChanged
)

// sourceStamp is what a capture recorded about a file. A zero modTime, an
// empty changeToken or an empty identity means the capture did not record it.
type sourceStamp struct {
	size        int64
	modTime     int64
	changeToken string
	identity    string
}

func stampOfInfo(info os.FileInfo) sourceStamp {
	return sourceStamp{
		size: info.Size(), modTime: info.ModTime().UnixNano(),
		changeToken: fileChangeToken(info), identity: fileIdentity(info),
	}
}

// compare reports whether info is still the captured file. Growth past the
// captured size is allowed, so metadata is only compared at the captured size.
func (stamp sourceStamp) compare(info os.FileInfo) sourceChange {
	switch {
	case stamp.identity != "" && fileIdentity(info) != stamp.identity:
		return sourceChanged
	case info.Size() < stamp.size:
		return sourceChanged
	case info.Size() > stamp.size:
		return sourceGrown
	case stamp.modTime != 0 && info.ModTime().UnixNano() != stamp.modTime:
		return sourceChanged
	case stamp.changeToken != "" && fileChangeToken(info) != stamp.changeToken:
		return sourceChanged
	}
	return sourceUnchanged
}

func (source fileSource) stamp() sourceStamp {
	stamp := stampOfInfo(source.info)
	stamp.size = source.end
	return stamp
}

func writeFileRangeObserved(ctx context.Context, file *os.File, start, end int64, output io.Writer, checkpoint func() error, observe func(int64) error) error {
	if start < 0 || end < start {
		return errors.New("invalid file range")
	}
	section := io.NewSectionReader(file, start, end-start)
	buffer := make([]byte, 256*1024)
	for remaining := end - start; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if checkpoint != nil {
			if err := checkpoint(); err != nil {
				return err
			}
		}
		want := int64(len(buffer))
		if want > remaining {
			want = remaining
		}
		read, err := section.Read(buffer[:want])
		if read > 0 {
			if observe != nil {
				if observeErr := observe(int64(read)); observeErr != nil {
					return observeErr
				}
			}
			if _, writeErr := output.Write(buffer[:read]); writeErr != nil {
				return writeErr
			}
			remaining -= int64(read)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && remaining == 0 {
				break
			}
			return err
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func fileRangeDigest(ctx context.Context, file *os.File, start, end int64) (string, error) {
	return fileRangeDigestObserved(ctx, file, start, end, nil, nil)
}

func fileRangeDigestObserved(ctx context.Context, file *os.File, start, end int64, checkpoint func() error, observe func(int64) error) (string, error) {
	if file == nil {
		return "", errors.New("conversation source is closed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || end > info.Size() {
		if err == nil {
			err = errors.New("source range is unavailable")
		}
		return "", err
	}
	digest := sha256.New()
	if err := writeFileRangeObserved(ctx, file, start, end, digest, checkpoint, observe); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (b *Browser) readRecentFilePage(ctx context.Context, request BrowseRequest, omo bool) (BrowsePage, error) {
	source, code := b.sourceFor(request.Scope, omo)
	if code != "" {
		return browseUnavailable(code, fileSourceReason(code, omo)), nil
	}
	defer source.close()
	start := source.end - b.options.RecentBytes
	if start < 0 {
		start = 0
	}
	digest, err := fileRangeDigestObserved(ctx, source.file, start, source.end, nil, func(bytes int64) error {
		b.observePhysicalRead(bytes)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return browseRequestCancelled(), nil
		}
		return failPage("source_unavailable", "The conversation source could not be read.", true), nil
	}
	projection, err := b.projectRecentRange(ctx, request.Scope, source, start, source.end, digest, omo)
	if err != nil {
		return b.recordReadFailure(err), nil
	}
	projection.Diagnostics.SourceTruncated = start > 0
	return b.memoryPage(
		request.Scope, source, projection.Entries, projection.Diagnostics, projection.Plan,
		start, source.end, digest, source.end, true, request.Limit,
	), nil
}

func fileSourceReason(code string, omo bool) string {
	if code == "invalid_session" {
		if omo {
			return "OMO conversation history is unavailable."
		}
		return "No conversation log is available for this session."
	}
	if code == "path_uncontained" {
		return "The conversation source is outside the configured profile root."
	}
	if omo {
		return "OMO conversation history is unavailable."
	}
	return "Conversation history is unavailable."
}

func (b *Browser) recordReadFailure(err error) BrowsePage {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return browseRequestCancelled()
	}
	if errors.Is(err, errJSONLSourceTruncated) || errors.Is(err, errRecentProjectionChanged) {
		return browseSourceChanged()
	}
	return failPage("source_unavailable", "The conversation source could not be read.", true)
}

func (b *Browser) readRecentCursorPage(ctx context.Context, request BrowseRequest, cursor browseCursor, omo bool) (BrowsePage, error) {
	start, err := parseBrowseOffset(cursor.RangeStart)
	if err != nil {
		return browseInvalidCursor(), nil
	}
	end, err := parseBrowseOffset(cursor.RangeEnd)
	if err != nil || end < start || cursor.RangeDigest == "" {
		return browseInvalidCursor(), nil
	}
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil || boundary > end {
		return browseInvalidCursor(), nil
	}
	source, code := b.sourceFor(request.Scope, omo)
	if code != "" {
		return browseUnavailable(code, fileSourceReason(code, omo)), nil
	}
	defer source.close()
	if source.revision != cursor.Revision || source.end < end {
		return browseSourceChanged(), nil
	}
	digest, err := fileRangeDigestObserved(ctx, source.file, start, end, nil, func(bytes int64) error {
		b.observePhysicalRead(bytes)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return browseRequestCancelled(), nil
		}
		return browseSourceChanged(), nil
	}
	if digest != cursor.RangeDigest {
		return browseSourceChanged(), nil
	}
	projection, err := b.projectRecentRange(ctx, request.Scope, source, start, end, digest, omo)
	if err != nil {
		return b.recordReadFailure(err), nil
	}
	projection.Diagnostics.SourceTruncated = start > 0
	return b.memoryPage(
		request.Scope, source, projection.Entries, projection.Diagnostics, projection.Plan,
		start, end, digest, boundary, false, request.Limit,
	), nil
}

func (b *Browser) recentCursor(scope BrowseScope, source fileSource, start, end int64, digest string, boundary int64) (string, error) {
	return encodeBrowseCursor(b.key, browseCursor{
		Mode: "recent", Scope: browseScopeID(scope), Revision: source.revision,
		Boundary: browseOffset(boundary), RangeStart: browseOffset(start), RangeEnd: browseOffset(end),
		RangeDigest: digest, ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
	})
}

func (b *Browser) prepareCursor(scope BrowseScope, source fileSource, start, end int64, digest string, boundary int64) (string, error) {
	return encodeBrowseCursor(b.key, browseCursor{
		Mode: "prepare", Scope: browseScopeID(scope), Revision: source.revision,
		Boundary: browseOffset(boundary), RangeStart: browseOffset(start), RangeEnd: browseOffset(end),
		RangeDigest: digest, ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
	})
}

func (b *Browser) readPreparePage(ctx context.Context, request BrowseRequest, cursor browseCursor, omo bool) (BrowsePage, error) {
	start, err := parseBrowseOffset(cursor.RangeStart)
	if err != nil {
		return browseInvalidCursor(), nil
	}
	end, err := parseBrowseOffset(cursor.RangeEnd)
	if err != nil || end < start || cursor.RangeDigest == "" {
		return browseInvalidCursor(), nil
	}
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil || boundary > end {
		return browseInvalidCursor(), nil
	}
	source, code := b.sourceFor(request.Scope, omo)
	if code != "" {
		return browseUnavailable(code, fileSourceReason(code, omo)), nil
	}
	sourceOwned := true
	defer func() {
		if sourceOwned {
			source.close()
		}
	}()
	if source.revision != cursor.Revision || source.end < end {
		return browseSourceChanged(), nil
	}
	anchorDigest, err := fileRangeDigest(ctx, source.file, start, end)
	if err != nil {
		if ctx.Err() != nil {
			return browseRequestCancelled(), nil
		}
		return browseSourceChanged(), nil
	}
	if anchorDigest != cursor.RangeDigest {
		return browseSourceChanged(), nil
	}
	if cursor.JobID != "" {
		job := b.job(cursor.JobID)
		if job == nil {
			return failPage("cursor_expired", "This preparation cursor has expired. Start browsing again from the latest messages.", true), nil
		}
		if !b.jobMatches(job, request.Scope, source, start, end, boundary, cursor.RangeDigest) {
			return browseInvalidConversationCursor(), nil
		}
		return b.continueJob(ctx, request, job)
	}
	job, page := b.createJob(request.Scope, source, start, end, boundary, cursor.RangeDigest, "", -1, claudeChainJobBinding{})
	if page != nil {
		return *page, nil
	}
	if job != nil {
		job.mu.RLock()
		sourceOwned = job.source.file != source.file
		job.mu.RUnlock()
	}
	return b.continueJob(ctx, request, job)
}

func (b *Browser) readSnapshotCursorPage(ctx context.Context, request BrowseRequest, cursor browseCursor) (BrowsePage, error) {
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil || cursor.JobID == "" || cursor.SnapshotID == "" {
		return browseInvalidCursor(), nil
	}
	job := b.acquireJob(cursor.JobID)
	if job == nil {
		return failPage("cursor_expired", "This snapshot cursor has expired. Start browsing again from the latest messages.", true), nil
	}
	defer b.releaseJob(job)
	job.mu.RLock()
	matches := job.snapshotID == cursor.SnapshotID && job.state == BrowseReady && job.source.revision == cursor.Revision && boundary <= job.boundary
	job.mu.RUnlock()
	if !matches {
		return browseInvalidSnapshotCursor(), nil
	}
	return b.readSnapshotPageAt(ctx, request.Scope, request.Limit, job, boundary)
}

func (b *Browser) readSnapshotPage(ctx context.Context, scope BrowseScope, limit int, job *browseJob) (BrowsePage, error) {
	job.mu.RLock()
	boundary := job.boundary
	job.mu.RUnlock()
	return b.readSnapshotPageAt(ctx, scope, limit, job, boundary)
}

func (b *Browser) readSnapshotPageAt(ctx context.Context, scope BrowseScope, limit int, job *browseJob, boundary int64) (BrowsePage, error) {
	b.observe(browserObservation{kind: "snapshot_read", job: job})
	b.renewJob(job)
	job.mu.Lock()
	index := job.index
	state := job.state
	source := job.source
	diagnostics := job.diagnostics
	plan := cloneTodo(job.plan)
	total := job.total
	snapshotID := job.snapshotID
	revision := job.source.revision
	jobCursor := job.cursor
	job.mu.Unlock()
	if state != BrowseReady {
		return b.continueJob(ctx, BrowseRequest{Scope: scope, Limit: limit}, job)
	}
	if index == nil {
		return failPage("index_failed", "The prepared conversation history is no longer available.", true), nil
	}
	if err := b.validateSnapshotJob(ctx, job, source, nil); err != nil {
		if ctx.Err() != nil {
			return browseRequestCancelled(), nil
		}
		return browseSourceChanged(), nil
	}
	entries, hasMore, err := index.entriesBefore(boundary, limit)
	if err != nil {
		return failPage("index_failed", "The prepared conversation history could not be read.", true), nil
	}
	base := BrowsePage{
		Available: true, State: BrowseReady, Mode: BrowseSnapshot, SourceRevision: revision,
		SnapshotID: snapshotID, Entries: []Entry{}, HasMore: hasMore, Diagnostics: diagnostics,
		OMOPlan: boundTodoPlan(plan, browseResponseBytes/4),
	}
	baseTotal := total
	base.Total = &baseTotal
	selected, omitted := b.fitProjected(base, entries, func(nextBoundary int64) (string, error) {
		return b.snapshotCursor(scope, job, nextBoundary)
	})
	toolsOmitted, payloadsOmitted := browseOmissions(entries, selected)
	base.Diagnostics.OmittedTools += toolsOmitted
	base.Diagnostics.OmittedPayloads += payloadsOmitted
	base.Entries = projectedEntries(selected)
	if omitted > 0 || hasMore {
		base.HasMore = true
		if len(selected) > 0 {
			base.NextCursor, err = b.snapshotCursor(scope, job, selected[0].Offset)
		} else {
			base.NextCursor = jobCursor
		}
		if err != nil {
			return browseContinuationFailed(), nil
		}
	} else {
		base.NextCursor = ""
	}
	return b.enforcePageBudget(base), nil
}

func (b *Browser) snapshotCursor(scope BrowseScope, job *browseJob, boundary int64) (string, error) {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return encodeBrowseCursor(b.key, browseCursor{
		Mode: "snapshot", Scope: browseScopeID(scope), Revision: job.source.revision,
		SnapshotID: job.snapshotID, JobID: job.id, Boundary: browseOffset(boundary),
		ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
	})
}

func (b *Browser) readOpenCodePage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	return b.readNativePage(ctx, request, "opencode")
}

func (b *Browser) readHermesPage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	return b.readNativePage(ctx, request, "hermes")
}

func (b *Browser) readNativePage(ctx context.Context, request BrowseRequest, provider string) (BrowsePage, error) {
	var cursor browseCursor
	var err error
	if request.Cursor != "" {
		cursor, err = decodeBrowseCursor(b.key, request.Cursor, request.Scope)
		if err != nil {
			code, reason := browseCursorError(err)
			return failPage(code, reason, code == "cursor_expired"), nil
		}
		if cursor.Mode != "native" || cursor.Revision == "" || cursor.NativeBefore == "" {
			return browseInvalidConversationCursor(), nil
		}
	}
	before := ""
	if cursor.NativeBefore != "" {
		before = cursor.NativeBefore
	}
	switch provider {
	case "hermes":
		return b.readHermesNative(ctx, request, cursor, before)
	case "agy":
		return b.readAgyNative(ctx, request, cursor, before)
	case "grok":
		return b.readGrokNative(ctx, request, cursor, before)
	}
	return b.readOpenCodeNative(ctx, request, cursor, before)
}

func (b *Browser) readAgyNative(ctx context.Context, request BrowseRequest, cursor browseCursor, before string) (BrowsePage, error) {
	const unavailable = "Antigravity conversation history is unavailable."
	if ctx.Err() != nil {
		return browseRequestCancelled(), nil
	}
	location, locateCode := b.reader.agy.locateContext(request.Scope.SessionID)
	if locateCode != "" {
		return nativeFailure(locateCode, unavailable), nil
	}
	entries, hasMore, corrupt, metadata, code := b.reader.agy.readFromContext(ctx, location.Path, before, request.Limit)
	if ctx.Err() != nil {
		return browseRequestCancelled(), nil
	}
	if code != "" {
		return nativeFailure(code, unavailable), nil
	}
	revision := nativeDatabaseRevision(location.Path, metadata.SchemaVersion, "agy")
	if cursor.Revision != "" && cursor.Revision != revision {
		return failPage("source_changed", "The Antigravity database changed while history was being browsed.", false), nil
	}
	return b.nativePage(request.Scope, entries, hasMore, corrupt, metadata.Total, fmt.Sprint(metadata.OldestIdx), revision, cursor), nil
}

func (b *Browser) readOpenCodeNative(ctx context.Context, request BrowseRequest, cursor browseCursor, before string) (BrowsePage, error) {
	entries, hasMore, corrupt, metadata, code := b.reader.openCode.readContext(ctx, request.Scope.SessionID, before, request.Limit)
	if ctx.Err() != nil {
		return browseRequestCancelled(), nil
	}
	if code != "" {
		return nativeFailure(code, "OpenCode conversation history is unavailable."), nil
	}
	// The session ID is authoritative for OpenCode; see readOpenCode.
	revision := nativeOpenCodeRevision(metadata)
	if cursor.Revision != "" && cursor.Revision != revision {
		return failPage("source_changed", "The OpenCode database changed while history was being browsed.", false), nil
	}
	page := b.nativePage(request.Scope, entries, hasMore, corrupt, metadata.Total, metadata.MessageID, revision, cursor)
	page.recordedPending = metadata.pendingIDs
	return page, nil
}

func (b *Browser) readHermesNative(ctx context.Context, request BrowseRequest, cursor browseCursor, before string) (BrowsePage, error) {
	if ctx.Err() != nil {
		return browseRequestCancelled(), nil
	}
	location, locateCode := b.reader.hermes.locateContext(ctx, request.Scope.CWD, request.Scope.SessionID)
	if ctx.Err() != nil {
		return browseRequestCancelled(), nil
	}
	if locateCode != "" || location.Path == "" {
		if locateCode == "" {
			locateCode = "invalid_session"
		}
		return nativeFailure(locateCode, "Hermes conversation history is unavailable."), nil
	}
	entries, hasMore, corrupt, metadata, code := b.reader.hermes.readFromContext(ctx, location.Path, request.Scope.SessionID, before, request.Limit)
	if ctx.Err() != nil {
		return browseRequestCancelled(), nil
	}
	if code != "" {
		return nativeFailure(code, "Hermes conversation history is unavailable."), nil
	}
	if request.Scope.CWD != "" && metadata.CWD != "" && !sameHermesDirectory(request.Scope.CWD, metadata.CWD) {
		return browseUnavailable("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	revision := nativeHermesRevision(location.Path, metadata)
	if cursor.Revision != "" && cursor.Revision != revision {
		return failPage("source_changed", "The Hermes database changed while history was being browsed.", false), nil
	}
	return b.nativePage(request.Scope, entries, hasMore, corrupt, metadata.Total, fmt.Sprint(metadata.MessageID), revision, cursor), nil
}

func nativeOpenCodeRevision(metadata openCodeRow) string {
	return nativeDatabaseRevision(metadata.Database, metadata.SchemaVersion, fmt.Sprint(metadata.V2))
}

func nativeHermesRevision(path string, metadata hermesRow) string {
	return nativeDatabaseRevision(path, metadata.SchemaVersion, "hermes")
}
