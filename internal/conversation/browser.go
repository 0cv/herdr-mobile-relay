package conversation

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

type BrowseScope struct {
	Provider        string `json:"provider"`
	CWD             string `json:"cwd"`
	SessionID       string `json:"session_id"`
	PaneID          string `json:"pane_id"`
	ServerSessionID string `json:"server_session_id"`
	TerminalID      string `json:"terminal_id"`
	Generation      int64  `json:"generation"`
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

type BrowsePage struct {
	Available      bool              `json:"available"`
	ReasonCode     string            `json:"reason_code,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Entries        []Entry           `json:"entries"`
	NextCursor     string            `json:"next_cursor,omitempty"`
	HasMore        bool              `json:"has_more"`
	State          BrowseState       `json:"state"`
	Mode           BrowseMode        `json:"mode"`
	SourceRevision string            `json:"source_revision,omitempty"`
	SnapshotID     string            `json:"snapshot_id,omitempty"`
	Total          *int              `json:"total"`
	Progress       *BrowseProgress   `json:"progress,omitempty"`
	Diagnostics    BrowseDiagnostics `json:"diagnostics"`
	Error          *BrowseError      `json:"error,omitempty"`
	OMOPlan        *OMOTodoState     `json:"omo_plan,omitempty"`
}

type BrowserOptions struct {
	RecentBytes          int64
	MaxRecordBytes       int64
	DefaultPageSize      int
	MaxPageSize          int
	ResponseBytes        int
	ActiveWorkers        int
	QueuedPreparations   int
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
		RecentBytes:          maxConversationBytes,
		MaxRecordBytes:       16 * 1024 * 1024,
		DefaultPageSize:      defaultPageSize,
		MaxPageSize:          maxPageSize,
		ResponseBytes:        2 * 1024 * 1024,
		ActiveWorkers:        1,
		QueuedPreparations:   4,
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

func (o BrowserOptions) normalized() BrowserOptions {
	defaults := DefaultBrowserOptions()
	if o.RecentBytes < 1 {
		o.RecentBytes = defaults.RecentBytes
	}
	if o.MaxRecordBytes < 1 {
		o.MaxRecordBytes = defaults.MaxRecordBytes
	}
	if o.DefaultPageSize < 1 {
		o.DefaultPageSize = defaults.DefaultPageSize
	}
	if o.MaxPageSize < 1 {
		o.MaxPageSize = defaults.MaxPageSize
	}
	if o.MaxPageSize < o.DefaultPageSize {
		o.MaxPageSize = o.DefaultPageSize
	}
	if o.ResponseBytes < 1024 {
		o.ResponseBytes = defaults.ResponseBytes
	}
	if o.ActiveWorkers < 1 {
		o.ActiveWorkers = defaults.ActiveWorkers
	}
	if o.QueuedPreparations < 1 {
		o.QueuedPreparations = defaults.QueuedPreparations
	}
	if o.InterestLease < time.Millisecond {
		o.InterestLease = defaults.InterestLease
	}
	if o.SnapshotTTL < time.Millisecond {
		o.SnapshotTTL = defaults.SnapshotTTL
	}
	if o.SnapshotQuota < 1 {
		o.SnapshotQuota = defaults.SnapshotQuota
	}
	if o.AggregateQuota < 1 {
		o.AggregateQuota = defaults.AggregateQuota
	}
	if o.CursorTTL < time.Second {
		o.CursorTTL = defaults.CursorTTL
	}
	if o.RecentCacheEntries < 1 {
		o.RecentCacheEntries = defaults.RecentCacheEntries
	}
	if o.RecentCacheItemBytes < 1 {
		o.RecentCacheItemBytes = defaults.RecentCacheItemBytes
	}
	if o.RecentCacheBytes < 1 {
		o.RecentCacheBytes = defaults.RecentCacheBytes
	}
	if o.RecentCacheTTL < time.Millisecond {
		o.RecentCacheTTL = defaults.RecentCacheTTL
	}
	return o
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

	mu                sync.RWMutex
	state             BrowseState
	phase             string
	scannedBytes      int64
	sourceBytes       int64
	diagnostics       BrowseDiagnostics
	plan              *OMOTodoState
	total             int
	err               *BrowseError
	lastInterest      time.Time
	readers           int
	validatedSize     int64
	validatedModTime  int64
	sourceDigest      string
	chainContextID    string
	chainSegment      int
	chainFooterStart  int64
	chainFooterEnd    int64
	chainFooterDigest string
	autoRetried       bool
	queued            bool
	running           bool
	lastAccess        time.Time
	retired           bool
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

	mu                sync.Mutex
	jobs              map[string]*browseJob
	dedup             map[string]string
	queue             []*browseJob
	chains            map[string]*claudeChainContext
	chainByKey        map[string]string
	chainLineage      map[string]claudeChainLineage
	chainReservations int
	active            int
	closing           bool

	schedulerWG sync.WaitGroup
	workerWG    sync.WaitGroup
	readWG      sync.WaitGroup
	closeOnce   sync.Once

	sourceReadObserver func(int64)
	recentCache        *recentProjectionCache
}

func NewBrowser(reader *Reader, cacheRoot string, options BrowserOptions) (*Browser, error) {
	if reader == nil {
		return nil, errors.New("conversation browser requires a reader")
	}
	key, err := newBrowseSigningKey()
	if err != nil {
		return nil, fmt.Errorf("create conversation browser key: %w", err)
	}
	options = options.normalized()
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
		chainLineage: make(map[string]claudeChainLineage),
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
		case job := <-b.done:
			b.finishJob(job)
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

func (b *Browser) finishJob(job *browseJob) {
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

func (b *Browser) observeSourceRead(bytes int64) {
	if b.sourceReadObserver != nil {
		b.sourceReadObserver(bytes)
	}
}

func (b *Browser) evictExpired(now time.Time) {
	var expired []expiredJob
	b.mu.Lock()
	for id, job := range b.jobs {
		job.mu.Lock()
		eligible := (job.state == BrowseReady || job.state == BrowseFailed) && !job.running && !job.queued && job.readers == 0 && now.Sub(job.lastAccess) >= b.options.SnapshotTTL
		if eligible {
			job.retired = true
			delete(b.jobs, id)
			if b.dedup[job.dedupKey] == id {
				delete(b.dedup, job.dedupKey)
			}
			index := job.index
			job.index = nil
			sourceFile := job.source.file
			job.source.file = nil
			expired = append(expired, expiredJob{index: index, sourceFile: sourceFile})
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
	b.mu.Unlock()
	for _, item := range expired {
		if item.sourceFile != nil {
			_ = item.sourceFile.Close()
		}
		if item.index != nil {
			_ = item.index.remove()
		}
	}
}

func (b *Browser) ReadPage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return browseFailure(true, "index_failed", "Conversation history browsing is shutting down.", browseErrorFor("index_failed", "Conversation history browsing is shutting down.", true)), nil
	}
	b.readWG.Add(1)
	b.mu.Unlock()
	defer b.readWG.Done()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", &BrowseError{Code: "request_cancelled", Message: "History request was cancelled.", Retryable: true}), nil
	}
	request.Scope = normalizeBrowseScope(request.Scope)
	request.Limit = b.limit(request.Limit)
	if request.Scope.SessionID == "" {
		return browseUnavailable("invalid_session", "This agent has not reported a conversation session yet."), nil
	}
	provider := normalizedAgent(request.Scope.Provider)
	if !Supported(provider) {
		return browseUnavailable("invalid_provider", "Conversation history is not available for this agent."), nil
	}
	if provider == "opencode" {
		return b.readOpenCodePage(ctx, request)
	}
	if provider == "omo" || provider == "ohmyopencode" {
		return b.readFilePage(ctx, request, true)
	}
	if isHermesAgent(provider) {
		return b.readHermesPage(ctx, request)
	}
	return b.readFilePage(ctx, request, false)
}

func normalizeBrowseScope(scope BrowseScope) BrowseScope {
	scope.Provider = normalizedAgent(scope.Provider)
	scope.CWD = strings.TrimSpace(scope.CWD)
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

func browseFailure(available bool, code, reason string, failure *BrowseError) BrowsePage {
	return BrowsePage{
		Available: available, ReasonCode: code, Reason: reason, Entries: []Entry{}, State: BrowseFailed,
		Mode: BrowseRecent, Diagnostics: BrowseDiagnostics{}, Error: failure,
	}
}

func browseErrorFor(code, message string, retryable bool) *BrowseError {
	return &BrowseError{Code: code, Message: message, Retryable: retryable}
}

func (b *Browser) readFilePage(ctx context.Context, request BrowseRequest, omo bool) (BrowsePage, error) {
	if request.Cursor == "" {
		if !omo && isClaudeProvider(request.Scope.Provider) {
			return b.readClaudeChainPage(ctx, request)
		}
		return b.readRecentFilePage(ctx, request, omo)
	}
	cursor, err := decodeBrowseCursor(b.key, request.Cursor, request.Scope)
	if err != nil {
		code, reason := browseCursorError(err)
		return browseFailure(true, code, reason, browseErrorFor(code, reason, code == "cursor_expired")), nil
	}
	switch cursor.Mode {
	case "recent":
		return b.readRecentCursorPage(ctx, request, cursor, omo)
	case "prepare":
		return b.readPreparePage(ctx, request, cursor, omo)
	case "snapshot":
		return b.readSnapshotCursorPage(ctx, request, cursor)
	case "chain":
		if !isClaudeProvider(request.Scope.Provider) {
			return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
		}
		return b.readClaudeChainCursorPage(ctx, request, cursor)
	default:
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
	}
}

func (b *Browser) sourceFor(scope BrowseScope, omo bool) (fileSource, string) {
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
		location = b.reader.Locate(scope.Provider, scope.CWD, scope.SessionID)
		if location.Path == "" {
			return fileSource{}, "invalid_session"
		}
	}
	source, err := captureFileSource(location)
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
	if location.Path == "" || location.Root == "" {
		return fileSource{}, errors.New("conversation source is not contained")
	}
	resolved := containedRegularFile(location.Path, location.Root)
	if resolved == "" || filepath.Clean(resolved) != filepath.Clean(location.Path) {
		return fileSource{}, errors.New("conversation source is not contained")
	}
	file, err := openConversationSource(location.Path)
	if err != nil {
		return fileSource{}, err
	}
	closeSource := true
	defer func() {
		if closeSource {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("conversation source is not a regular file")
		}
		return fileSource{}, err
	}
	resolvedAgain, err := filepath.EvalSymlinks(location.Path)
	if err != nil || filepath.Clean(resolvedAgain) != filepath.Clean(location.Path) || containedRegularFile(location.Path, location.Root) != location.Path {
		return fileSource{}, errors.New("conversation source changed while opening")
	}
	pathInfo, err := os.Stat(location.Path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return fileSource{}, errors.New("conversation source changed while opening")
	}
	revision, err := fileRevision(file, info)
	if err != nil {
		return fileSource{}, err
	}
	closeSource = false
	return fileSource{location: location, path: location.Path, file: file, info: info, end: info.Size(), revision: revision}, nil
}

func fileRevision(file *os.File, info os.FileInfo) (string, error) {
	const anchorBytes = int64(64 * 1024)
	digest := sha256.New()
	sectionSize := info.Size()
	if sectionSize > anchorBytes {
		sectionSize = anchorBytes
	}
	if sectionSize > 0 {
		firstRecord, err := bufio.NewReader(io.NewSectionReader(file, 0, sectionSize)).ReadBytes('\n')
		if len(firstRecord) > 0 {
			_, _ = digest.Write(firstRecord)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
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

func writeFileRange(ctx context.Context, file *os.File, start, end int64, output io.Writer) error {
	return writeFileRangeChecked(ctx, file, start, end, output, nil)
}

func writeFileRangeChecked(ctx context.Context, file *os.File, start, end int64, output io.Writer, checkpoint func() error) error {
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
	return fileRangeDigestChecked(ctx, file, start, end, nil)
}

func fileRangeDigestChecked(ctx context.Context, file *os.File, start, end int64, checkpoint func() error) (string, error) {
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
	if err := writeFileRangeChecked(ctx, file, start, end, digest, checkpoint); err != nil {
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
	digest, err := fileRangeDigest(ctx, source.file, start, source.end)
	if err != nil {
		if ctx.Err() != nil {
			return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
		}
		return browseFailure(true, "source_unavailable", "The conversation source could not be read.", browseErrorFor("source_unavailable", "The conversation source could not be read.", true)), nil
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
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true))
	}
	if errors.Is(err, errJSONLSourceTruncated) || errors.Is(err, errRecentProjectionChanged) {
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false))
	}
	return browseFailure(true, "source_unavailable", "The conversation source could not be read.", browseErrorFor("source_unavailable", "The conversation source could not be read.", true))
}

func (b *Browser) readRecentCursorPage(ctx context.Context, request BrowseRequest, cursor browseCursor, omo bool) (BrowsePage, error) {
	start, err := parseBrowseOffset(cursor.RangeStart)
	if err != nil {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
	}
	end, err := parseBrowseOffset(cursor.RangeEnd)
	if err != nil || end < start || cursor.RangeDigest == "" {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
	}
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil || boundary > end {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
	}
	source, code := b.sourceFor(request.Scope, omo)
	if code != "" {
		return browseUnavailable(code, fileSourceReason(code, omo)), nil
	}
	defer source.close()
	if source.revision != cursor.Revision || source.end < end {
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	digest, err := fileRangeDigest(ctx, source.file, start, end)
	if err != nil {
		if ctx.Err() != nil {
			return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
		}
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	if digest != cursor.RangeDigest {
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
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

func (b *Browser) memoryPage(
	scope BrowseScope,
	source fileSource,
	entries []projectedEntry,
	diagnostics BrowseDiagnostics,
	plan *OMOTodoState,
	rangeStart, rangeEnd int64,
	rangeDigest string,
	boundary int64,
	initial bool,
	limit int,
) BrowsePage {
	eligible := make([]projectedEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Offset < boundary {
			eligible = append(eligible, entry)
		}
	}
	start := len(eligible) - limit
	if start < 0 {
		start = 0
	}
	selected := append([]projectedEntry(nil), eligible[start:]...)
	potentialOlder := start > 0 || rangeStart > 0
	base := BrowsePage{
		Available: true, State: BrowseReady, Mode: BrowseRecent,
		SourceRevision: source.revision, Entries: []Entry{}, Diagnostics: diagnostics, OMOPlan: boundTodoPlan(plan, b.options.ResponseBytes/4),
	}
	if rangeStart == 0 {
		total := len(entries)
		base.Total = &total
	} else {
		base.Reason = "Showing recent messages. Older history can be loaded from this computer."
	}
	if !potentialOlder && len(selected) == 0 && initial && rangeStart > 0 {
		potentialOlder = true
	}

	var next func(int64) (string, error)
	if potentialOlder {
		next = func(nextBoundary int64) (string, error) {
			if start > 0 {
				return b.recentCursor(scope, source, rangeStart, rangeEnd, rangeDigest, nextBoundary)
			}
			prepareBoundary := rangeStart
			if initial && len(entries) == 0 {
				prepareBoundary = rangeEnd
			}
			return b.prepareCursor(scope, source, rangeStart, rangeEnd, rangeDigest, prepareBoundary)
		}
	}
	fitted, omitted := b.fitProjected(base, selected, next)
	toolsOmitted, payloadsOmitted := browseOmissions(selected, fitted)
	base.Diagnostics.OmittedTools += toolsOmitted
	base.Diagnostics.OmittedPayloads += payloadsOmitted
	base.Entries = projectedEntries(fitted)
	if omitted > 0 {
		base.HasMore = true
		cursor, err := b.recentCursor(scope, source, rangeStart, rangeEnd, rangeDigest, fitted[0].Offset)
		if err != nil {
			return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
		}
		base.NextCursor = cursor
	} else if potentialOlder && len(fitted) > 0 {
		base.HasMore = true
		cursor, err := next(fitted[0].Offset)
		if err != nil {
			return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
		}
		base.NextCursor = cursor
	} else if potentialOlder {
		base.HasMore = true
		cursor, err := next(boundary)
		if err != nil {
			return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true))
		}
		base.NextCursor = cursor
	}
	return b.enforcePageBudget(base)
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

func (b *Browser) enforcePageBudget(page BrowsePage) BrowsePage {
	for b.pageSize(page) > b.options.ResponseBytes && len(page.Entries) > 0 {
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
	if b.pageSize(page) > b.options.ResponseBytes {
		if page.OMOPlan != nil {
			page.Diagnostics.OmittedPayloads++
		}
		page.OMOPlan = nil
	}
	return page
}

func (b *Browser) fitProjected(base BrowsePage, entries []projectedEntry, next func(int64) (string, error)) ([]projectedEntry, int) {
	if len(entries) == 0 {
		return nil, 0
	}
	for omitted := 0; omitted < len(entries); omitted++ {
		candidate := entries[omitted:]
		page := base
		page.Entries = projectedEntries(candidate)
		if next != nil {
			cursor, err := next(candidate[0].Offset)
			if err != nil {
				continue
			}
			page.NextCursor = cursor
			page.HasMore = true
		}
		if b.pageSize(page) <= b.options.ResponseBytes {
			return candidate, omitted
		}
	}
	last := entries[len(entries)-1]
	last.Entry = boundBrowseEntry(last.Entry, b.options.ResponseBytes)
	page := base
	page.Entries = []Entry{last.Entry}
	if next != nil {
		cursor, err := next(last.Offset)
		if err == nil {
			page.NextCursor = cursor
			page.HasMore = true
		}
	}
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
	if len(entry.Text) > 4096 {
		entry.Text = entry.Text[:4096]
	}
	for len(entry.Text) > 0 {
		if data, _ := json.Marshal(entry); len(data) <= budget {
			return entry
		}
		entry.Text = entry.Text[:len(entry.Text)/2]
	}
	return entry
}

func boundBrowseTool(tool ToolActivity) ToolActivity {
	tool.Truncated = true
	tool.ID, _ = clampText(tool.ID, 128)
	tool.Name, _ = clampText(tool.Name, 64)
	tool.Input, _ = clampText(tool.Input, 1024)
	tool.Output, _ = clampText(tool.Output, 1024)
	if tool.Name == "" {
		tool.Name = "Tool"
	}
	return tool
}

func (b *Browser) pageSize(page BrowsePage) int {
	data, err := json.Marshal(page)
	if err != nil {
		return math.MaxInt
	}
	return len(data)
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
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
	}
	end, err := parseBrowseOffset(cursor.RangeEnd)
	if err != nil || end < start || cursor.RangeDigest == "" {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
	}
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil || boundary > end {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
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
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	anchorDigest, err := fileRangeDigest(ctx, source.file, start, end)
	if err != nil {
		if ctx.Err() != nil {
			return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
		}
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	if anchorDigest != cursor.RangeDigest {
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	if cursor.JobID != "" {
		job := b.job(cursor.JobID)
		if job == nil {
			return browseFailure(true, "cursor_expired", "This preparation cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This preparation cursor has expired. Start browsing again from the latest messages.", true)), nil
		}
		if !b.jobMatches(job, request.Scope, source, start, end, boundary, cursor.RangeDigest) {
			return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
		}
		return b.continueJob(ctx, request, job)
	}
	job, page := b.createJob(request.Scope, source, start, end, boundary, cursor.RangeDigest, omo, "", -1)
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

func (b *Browser) createJob(scope BrowseScope, source fileSource, start, end, boundary int64, digest string, omo bool, chainContextID string, chainSegment int) (*browseJob, *BrowsePage) {
	if b.cacheErr != nil {
		page := browseFailure(true, "index_storage_unavailable", "Older history cannot be prepared on this computer right now.", browseErrorFor("index_storage_unavailable", "Older history cannot be prepared on this computer right now.", true))
		return nil, &page
	}
	if b.aggregateCacheBytes() >= b.options.AggregateQuota {
		page := browseFailure(true, "index_capacity_exceeded", "The conversation history cache reached its storage limit.", browseErrorFor("index_capacity_exceeded", "The conversation history cache reached its storage limit.", true))
		return nil, &page
	}
	scopeID := browseScopeID(scope)
	dedupKey := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%d", scopeID, source.revision, start, end, boundary, digest, chainContextID, chainSegment)
	b.mu.Lock()
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
		page := browseFailure(true, "index_failed", "Older history preparation is shutting down.", browseErrorFor("index_failed", "Older history preparation is shutting down.", true))
		return nil, &page
	}
	if len(b.queue) >= b.options.QueuedPreparations {
		b.mu.Unlock()
		page := browseFailure(true, "index_busy", "Another history preparation is already queued. Try again shortly.", browseErrorFor("index_busy", "Another history preparation is already queued. Try again shortly.", true))
		return nil, &page
	}
	id, err := randomIdentifier()
	if err != nil {
		b.mu.Unlock()
		page := browseFailure(true, "index_failed", "History preparation could not be started.", browseErrorFor("index_failed", "History preparation could not be started.", true))
		return nil, &page
	}
	snapshotID, err := randomIdentifier()
	if err != nil {
		b.mu.Unlock()
		page := browseFailure(true, "index_failed", "History preparation could not be started.", browseErrorFor("index_failed", "History preparation could not be started.", true))
		return nil, &page
	}
	metadata := snapshotMetadata{
		Schema: 1, State: "building", SnapshotID: snapshotID, SourceRevision: source.revision,
		EndOffset: source.end, Boundary: boundary, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	index, err := openSnapshotIndex(b.cacheRoot, snapshotID, metadata)
	if err != nil {
		b.mu.Unlock()
		page := browseFailure(true, "index_storage_unavailable", "Older history cannot be prepared on this computer right now.", browseErrorFor("index_storage_unavailable", "Older history cannot be prepared on this computer right now.", true))
		return nil, &page
	}
	job := &browseJob{
		id: id, dedupKey: dedupKey, scope: scope, scopeID: scopeID, source: source,
		boundary: boundary, rangeStart: start, rangeEnd: end, rangeDigest: digest,
		chainContextID: chainContextID, chainSegment: chainSegment,
		snapshotID: snapshotID, index: index, state: BrowsePreparing, phase: "queued",
		sourceBytes: source.end, lastInterest: time.Now(), lastAccess: time.Now(),
	}
	job.cursor, err = encodeBrowseCursor(b.key, browseCursor{
		Mode: "prepare", Scope: scopeID, Revision: source.revision, JobID: id,
		Boundary: browseOffset(boundary), RangeStart: browseOffset(start), RangeEnd: browseOffset(end),
		RangeDigest: digest, ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
	})
	if err != nil {
		_ = index.remove()
		b.mu.Unlock()
		page := browseFailure(true, "index_failed", "History preparation could not be started.", browseErrorFor("index_failed", "History preparation could not be started.", true))
		return nil, &page
	}
	job.queued = true
	b.jobs[id] = job
	b.dedup[dedupKey] = id
	b.queue = append(b.queue, job)
	b.mu.Unlock()
	b.signalWake()
	return job, nil
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
		return browseFailure(true, "cursor_expired", "This snapshot cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This snapshot cursor has expired. Start browsing again from the latest messages.", true)), nil
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
		if err != nil && err.Retryable && (request.Retry || !autoRetried) {
			if !request.Retry {
				job.mu.Lock()
				job.autoRetried = true
				job.mu.Unlock()
			}
			if b.retryJob(ctx, job) {
				return b.preparingPage(job, cursor), nil
			}
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

func (b *Browser) retryJob(ctx context.Context, job *browseJob) bool {
	if ctx == nil {
		ctx = context.Background()
	}
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
	omo := normalizedAgent(scope.Provider) == "omo" || normalizedAgent(scope.Provider) == "ohmyopencode"
	var source fileSource
	var code string
	var err error
	if chainContextID != "" {
		source, err = captureFileSource(chainLocation)
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
		digest, digestErr := fileRangeDigest(ctx, source.file, chainFooterStart, chainFooterEnd)
		if digestErr != nil || digest != chainFooterDigest {
			return false
		}
	} else if digest, err := fileRangeDigest(ctx, source.file, rangeStart, rangeEnd); err != nil || digest != rangeDigest {
		return false
	}
	b.mu.Lock()
	if b.closing || len(b.queue) >= b.options.QueuedPreparations {
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
	job.index = nil
	snapshotID, err := randomIdentifier()
	if err != nil {
		job.index = oldIndex
		job.mu.Unlock()
		b.mu.Unlock()
		return false
	}
	metadata := snapshotMetadata{
		Schema: 1, State: "building", SnapshotID: snapshotID, SourceRevision: source.revision,
		EndOffset: source.end, Boundary: job.boundary, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	index, err := openSnapshotIndex(b.cacheRoot, snapshotID, metadata)
	if err != nil {
		job.index = oldIndex
		job.mu.Unlock()
		b.mu.Unlock()
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
	b.signalWake()
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

func (b *Browser) readSnapshotCursorPage(ctx context.Context, request BrowseRequest, cursor browseCursor) (BrowsePage, error) {
	boundary, err := parseBrowseOffset(cursor.Boundary)
	if err != nil || cursor.JobID == "" || cursor.SnapshotID == "" {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid.", browseErrorFor("invalid_cursor", "This history cursor is invalid.", false)), nil
	}
	job := b.acquireJob(cursor.JobID)
	if job == nil {
		return browseFailure(true, "cursor_expired", "This snapshot cursor has expired. Start browsing again from the latest messages.", browseErrorFor("cursor_expired", "This snapshot cursor has expired. Start browsing again from the latest messages.", true)), nil
	}
	defer b.releaseJob(job)
	job.mu.RLock()
	matches := job.snapshotID == cursor.SnapshotID && job.state == BrowseReady && job.source.revision == cursor.Revision && boundary <= job.boundary
	job.mu.RUnlock()
	if !matches {
		return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested snapshot.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested snapshot.", false)), nil
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
		return browseFailure(true, "index_failed", "The prepared conversation history is no longer available.", browseErrorFor("index_failed", "The prepared conversation history is no longer available.", true)), nil
	}
	if err := b.validateSnapshotJob(ctx, job, source); err != nil {
		if ctx.Err() != nil {
			return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
		}
		return browseFailure(true, "source_changed", "The conversation source changed while history was being browsed.", browseErrorFor("source_changed", "The conversation source changed while history was being browsed.", false)), nil
	}
	entries, hasMore, err := index.entriesBefore(boundary, limit)
	if err != nil {
		return browseFailure(true, "index_failed", "The prepared conversation history could not be read.", browseErrorFor("index_failed", "The prepared conversation history could not be read.", true)), nil
	}
	base := BrowsePage{
		Available: true, State: BrowseReady, Mode: BrowseSnapshot, SourceRevision: revision,
		SnapshotID: snapshotID, Entries: []Entry{}, HasMore: hasMore, Diagnostics: diagnostics,
		OMOPlan: boundTodoPlan(plan, b.options.ResponseBytes/4),
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
			return browseFailure(true, "index_failed", "History continuation could not be created.", browseErrorFor("index_failed", "History continuation could not be created.", true)), nil
		}
	} else {
		base.NextCursor = ""
	}
	return b.enforcePageBudget(base), nil
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
	if !os.SameFile(source.info, info) || info.Size() < source.end {
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

func (b *Browser) validateSnapshotJob(ctx context.Context, job *browseJob, source fileSource) error {
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
	expectedDigest := job.sourceDigest
	job.mu.RUnlock()
	modTime := info.ModTime().UnixNano()
	if expectedDigest == "" {
		return errors.New("conversation snapshot has no source digest")
	}
	currentRevision, err := fileRevision(source.file, info)
	if err != nil || currentRevision != source.revision {
		return errors.New("conversation source changed")
	}
	if info.Size() == validatedSize && modTime == validatedModTime {
		return nil
	}
	if info.Size() > validatedSize && info.Size() >= source.end {
		job.mu.Lock()
		job.validatedSize = info.Size()
		job.validatedModTime = modTime
		job.mu.Unlock()
		return nil
	}
	b.observeSourceRead(source.end)
	digest, err := fileRangeDigest(ctx, source.file, 0, source.end)
	if err != nil {
		return err
	}
	if digest != expectedDigest {
		return errors.New("conversation source changed")
	}
	job.mu.Lock()
	job.validatedSize = info.Size()
	job.validatedModTime = modTime
	job.mu.Unlock()
	return nil
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

func (b *Browser) runJob(job *browseJob) {
	defer b.workerWG.Done()
	defer func() {
		job.mu.Lock()
		job.running = false
		job.mu.Unlock()
		b.done <- job
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			b.failJob(job, "index_failed", "History preparation failed on this computer.", true)
		}
	}()
	job.mu.Lock()
	job.phase = "scanning"
	job.scannedBytes = 0
	job.sourceBytes = job.source.end
	file := job.source.file
	job.mu.Unlock()
	if file == nil {
		b.failJob(job, "source_unavailable", "The conversation source became unavailable.", true)
		return
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(job.source.info, info) || info.Size() < job.source.end {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
	}
	if resolved, resolveErr := filepath.EvalSymlinks(job.source.path); resolveErr != nil || filepath.Clean(resolved) != filepath.Clean(job.source.path) || containedRegularFile(job.source.path, job.source.location.Root) != job.source.path {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
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
		results, applyErr := job.index.applyRecords(job.scope.Provider, job.source.revision, batch)
		if applyErr != nil {
			b.failJob(job, "index_failed", "History preparation could not store its index.", true)
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
		if job.index.size() > b.options.SnapshotQuota || b.aggregateCacheBytes() > b.options.AggregateQuota {
			b.failJob(job, "index_capacity_exceeded", "The conversation history index reached its storage limit.", true)
			return
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
	b.observeSourceRead(job.source.end)
	secondDigest, digestErr := fileRangeDigestChecked(b.ctx, file, 0, job.source.end, func() error {
		if !b.jobHasInterest(job) {
			return errJSONLCheckpoint
		}
		return nil
	})
	if errors.Is(digestErr, errJSONLCheckpoint) {
		b.failJob(job, "index_failed", "History preparation paused because no client is waiting for it.", true)
		return
	}
	if digestErr != nil || firstDigest != secondDigest {
		b.failJob(job, "source_changed", "The conversation source changed while it was being prepared.", false)
		return
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
	if normalizedAgent(job.scope.Provider) == "omo" || normalizedAgent(job.scope.Provider) == "ohmyopencode" {
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
	if job.index.size() > b.options.SnapshotQuota || b.aggregateCacheBytes() > b.options.AggregateQuota {
		b.failJob(job, "index_capacity_exceeded", "The conversation history index reached its storage limit.", true)
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
	if err := job.index.setMetadata(metadata); err != nil || job.index.sync() != nil {
		b.failJob(job, "index_failed", "The prepared conversation index could not be finalized.", true)
		return
	}
	verifiedInfo, _ := file.Stat()
	job.mu.Lock()
	chainContextID := job.chainContextID
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
	failure := browseErrorFor(code, message, retryable)
	job.mu.Lock()
	job.state = BrowseFailed
	job.phase = "failed"
	job.err = failure
	index := job.index
	job.index = nil
	sourceFile := job.source.file
	job.source.file = nil
	job.mu.Unlock()
	if sourceFile != nil {
		_ = sourceFile.Close()
	}
	if index != nil {
		_ = index.remove()
	}
}

func (b *Browser) readOpenCodePage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	return b.readNativePage(ctx, request, false)
}

func (b *Browser) readHermesPage(ctx context.Context, request BrowseRequest) (BrowsePage, error) {
	return b.readNativePage(ctx, request, true)
}

func (b *Browser) readNativePage(ctx context.Context, request BrowseRequest, hermes bool) (BrowsePage, error) {
	var cursor browseCursor
	var err error
	if request.Cursor != "" {
		cursor, err = decodeBrowseCursor(b.key, request.Cursor, request.Scope)
		if err != nil {
			code, reason := browseCursorError(err)
			return browseFailure(true, code, reason, browseErrorFor(code, reason, code == "cursor_expired")), nil
		}
		if cursor.Mode != "native" || cursor.Revision == "" || cursor.NativeBefore == "" {
			return browseFailure(true, "invalid_cursor", "This history cursor is invalid for the requested conversation.", browseErrorFor("invalid_cursor", "This history cursor is invalid for the requested conversation.", false)), nil
		}
	}
	before := ""
	if cursor.NativeBefore != "" {
		before = cursor.NativeBefore
	}
	if hermes {
		return b.readHermesNative(ctx, request, cursor, before)
	}
	return b.readOpenCodeNative(ctx, request, cursor, before)
}

func (b *Browser) readOpenCodeNative(ctx context.Context, request BrowseRequest, cursor browseCursor, before string) (BrowsePage, error) {
	entries, hasMore, corrupt, metadata, code := b.reader.openCode.readContext(ctx, request.Scope.SessionID, before, request.Limit)
	if ctx.Err() != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
	}
	if code != "" {
		return nativeFailure(code, "OpenCode conversation history is unavailable."), nil
	}
	if request.Scope.CWD != "" && !sameOpenCodeDirectory(request.Scope.CWD, metadata.Directory) {
		return browseUnavailable("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	revision := nativeOpenCodeRevision(metadata)
	if cursor.Revision != "" && cursor.Revision != revision {
		return browseFailure(true, "source_changed", "The OpenCode database changed while history was being browsed.", browseErrorFor("source_changed", "The OpenCode database changed while history was being browsed.", false)), nil
	}
	return b.nativePage(request.Scope, entries, hasMore, corrupt, metadata.Total, metadata.MessageID, revision, cursor), nil
}

func (b *Browser) readHermesNative(ctx context.Context, request BrowseRequest, cursor browseCursor, before string) (BrowsePage, error) {
	if ctx.Err() != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
	}
	location, locateCode := b.reader.hermes.locateContext(ctx, request.Scope.CWD, request.Scope.SessionID)
	if ctx.Err() != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
	}
	if locateCode != "" || location.Path == "" {
		if locateCode == "" {
			locateCode = "invalid_session"
		}
		return nativeFailure(locateCode, "Hermes conversation history is unavailable."), nil
	}
	entries, hasMore, corrupt, metadata, code := b.reader.hermes.readFromContext(ctx, location.Path, request.Scope.SessionID, before, request.Limit)
	if ctx.Err() != nil {
		return browseFailure(true, "request_cancelled", "History request was cancelled.", browseErrorFor("request_cancelled", "History request was cancelled.", true)), nil
	}
	if code != "" {
		return nativeFailure(code, "Hermes conversation history is unavailable."), nil
	}
	if request.Scope.CWD != "" && metadata.CWD != "" && !sameOpenCodeDirectory(request.Scope.CWD, metadata.CWD) {
		return browseUnavailable("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	revision := nativeHermesRevision(location.Path, metadata)
	if cursor.Revision != "" && cursor.Revision != revision {
		return browseFailure(true, "source_changed", "The Hermes database changed while history was being browsed.", browseErrorFor("source_changed", "The Hermes database changed while history was being browsed.", false)), nil
	}
	return b.nativePage(request.Scope, entries, hasMore, corrupt, metadata.Total, fmt.Sprint(metadata.MessageID), revision, cursor), nil
}

func nativeFailure(code, reason string) BrowsePage {
	if code == "invalid_session" || code == "source_unavailable" {
		return browseUnavailable(code, reason)
	}
	if code == "source_corrupt" {
		return browseFailure(true, code, reason, browseErrorFor(code, reason, false))
	}
	return browseFailure(true, code, reason, browseErrorFor(code, reason, code == "output_limit" || code == "query_failed"))
}

func nativeOpenCodeRevision(metadata openCodeRow) string {
	return nativeSourceIdentityRevision(metadata.Database)
}

func nativeHermesRevision(path string, _ hermesRow) string {
	return nativeSourceIdentityRevision(path)
}

func nativeSourceIdentityRevision(path string) string {
	identity := ""
	if info, err := os.Stat(path); err == nil {
		identity = fileIdentity(info)
	}
	data, _ := json.Marshal([]string{filepath.Clean(path), identity})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (b *Browser) nativePage(scope BrowseScope, entries []Entry, hasMore bool, corrupt bool, total int, rawBefore, revision string, cursor browseCursor) BrowsePage {
	diagnostics := normalizeEntriesForResponse(entries)
	if corrupt {
		diagnostics.CorruptRecords = 1
	}
	base := BrowsePage{
		Available: true, State: BrowseReady, Mode: BrowseNative, Entries: []Entry{},
		HasMore: hasMore, SourceRevision: revision, Diagnostics: diagnostics,
	}
	baseTotal := total
	base.Total = &baseTotal
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
			Mode: "native", Scope: browseScopeID(scope), Revision: revision, NativeBefore: before,
			ExpiresAt: time.Now().Add(b.options.CursorTTL).Unix(),
		})
	}
	selected := projected
	omitted := 0
	for from := 0; from < len(projected); from++ {
		candidate := projected[from:]
		page := base
		page.Entries = projectedEntries(candidate)
		page.HasMore = potentialOlder || from > 0
		if page.HasMore {
			page.NextCursor, _ = cursorFor(candidate[0].Entry)
		}
		if b.pageSize(page) <= b.options.ResponseBytes {
			selected, omitted = candidate, from
			break
		}
		if from == len(projected)-1 {
			selected, omitted = []projectedEntry{{Offset: candidate[0].Offset, Entry: boundBrowseEntry(candidate[0].Entry, b.options.ResponseBytes)}}, from
		}
	}
	toolsOmitted, payloadsOmitted := browseOmissions(projected, selected)
	base.Diagnostics.OmittedTools += toolsOmitted
	base.Diagnostics.OmittedPayloads += payloadsOmitted
	base.Entries = projectedEntries(selected)
	if omitted > 0 || potentialOlder {
		base.HasMore = true
		anchor := Entry{}
		if len(selected) > 0 {
			anchor = selected[0].Entry
		}
		var err error
		base.NextCursor, err = cursorFor(anchor)
		if err != nil {
			return browseFailure(true, "source_corrupt", "The native conversation cursor was not available.", browseErrorFor("source_corrupt", "The native conversation cursor was not available.", false))
		}
	}
	return b.enforcePageBudget(base)
}

func (b *Browser) aggregateCacheBytes() int64 {
	if b.cacheRoot == "" {
		return 0
	}
	entries, err := os.ReadDir(b.cacheRoot)
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "snapshot-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := os.Lstat(filepath.Join(b.cacheRoot, entry.Name()))
		if err == nil && info.Mode().IsRegular() {
			if info.Size() > math.MaxInt64-total {
				return math.MaxInt64
			}
			total += info.Size()
		}
	}
	return total
}
