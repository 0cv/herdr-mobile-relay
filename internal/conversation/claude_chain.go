package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

const (
	claudeContinuationFooterBytes  = int64(64 * 1024)
	claudeContinuationMaxSegments  = 64
	claudeContinuationFooterBudget = int64(4 * 1024 * 1024)
	claudeContinuationRecordBytes  = int64(1024 * 1024)
)

var partialClaudeContinuationType = regexp.MustCompile(`"type"\s*:\s*"continued-in"`)

const (
	continuationReasonMissingSource = "missing_source"
	continuationReasonInvalidLink   = "invalid_link"
	continuationReasonAmbiguous     = "ambiguous_link"
	continuationReasonCycle         = "cycle"
	continuationReasonLimit         = "resolution_limit"
	continuationReasonPartial       = "partial_link"
)

// claudeSegment is a fixed descriptor, not an open file. CapturedEnd and the
// footer evidence freeze a browsing context while allowing each read to reopen
// the source with the normal containment and no-follow checks.
type claudeRangeEvidence struct {
	Start  int64
	End    int64
	Digest string
}

type claudeSegment struct {
	SessionID    string
	Location     Location
	CapturedEnd  int64
	FileRevision string
	// SourceModTime detects equal-length in-place rewrites without changing the
	// normal snapshot revision semantics, which intentionally allow appends.
	SourceModTime int64
	// SourceChangeToken detects same-size rewrites even when a caller restores
	// the modification time. It is derived from the filesystem change time when
	// available and is never exposed on the wire.
	SourceChangeToken string
	// FileIdentity catches aliases which have different safe paths but refer to
	// the same inode. It is private descriptor data and never crosses the wire.
	FileIdentity string
	// RecentDigest is populated by a browser context once it has read the
	// selected frozen range. The resolver deliberately does not hash whole
	// transcripts while discovering a chain.
	RecentStart  int64
	RecentEnd    int64
	RecentDigest string
	// ObservedRanges retains selected ranges authenticated by a browsing
	// context. RecentStart/RecentEnd remain the current context's projection
	// range; this immutable ledger is what lineage validation uses when a file
	// grows and a later resolver descriptor has no range metadata of its own.
	ObservedRanges []claudeRangeEvidence
	// When overlapping append ranges would exceed the retained-evidence bound,
	// the ranges are replaced by a background-authenticated enclosing digest.
	// A pending compaction is an obligation, not permission to admit a new
	// lineage descriptor.
	EvidenceCompactionPending bool
	EvidenceCompactionStart   int64
	EvidenceCompactionEnd     int64
	EvidenceCompactionError   string
	// EvidenceCompactionRanges is retained only while the enclosing digest is
	// being authenticated. The worker checks these old digests before replacing
	// them, so compaction cannot silently bless a rewrite that happened before
	// the background pass began.
	EvidenceCompactionRanges []claudeRangeEvidence
	CapturedDigest           string
	FooterStart              int64
	FooterEnd                int64
	FooterDigest             string
}

type claudeChain struct {
	Segments         []claudeSegment
	IncompleteReason string
}

func (chain claudeChain) incomplete() bool {
	return chain.IncompleteReason != ""
}

func isClaudeProvider(agent string) bool {
	switch normalizedAgent(agent) {
	case "claude", "claudecode":
		return true
	default:
		return false
	}
}

// resolveClaudeChain follows only the explicit same-project continued-in
// footer links of the selected anchor. It intentionally does not call Locate
// for descendants: root/project selection belongs to the anchor, and a new
// global search could cross profiles or projects with duplicate session IDs.
func resolveClaudeChain(ctx context.Context, anchor Location, anchorSessionID string) (claudeChain, error) {
	return resolveClaudeChainWithObserver(ctx, anchor, anchorSessionID, nil)
}

func resolveClaudeChainWithObserver(ctx context.Context, anchor Location, anchorSessionID string, observe func(int64)) (claudeChain, error) {
	var readObserver func(int64) error
	if observe != nil {
		readObserver = func(bytes int64) error {
			observe(bytes)
			return nil
		}
	}
	return resolveClaudeChainWithReadObserver(ctx, anchor, anchorSessionID, readObserver)
}

func resolveClaudeChainWithReadObserver(ctx context.Context, anchor Location, anchorSessionID string, observe func(int64) error) (claudeChain, error) {
	return resolveClaudeChainWithReadHooks(ctx, anchor, anchorSessionID, nil, observe)
}

// resolveClaudeChainWithReadHooks separates reservation from observation. A
// foreground caller must reserve each bounded read before issuing it; an
// observer alone would discover an over-budget ReadAt only after the kernel had
// already returned the bytes.
func resolveClaudeChainWithReadHooks(ctx context.Context, anchor Location, anchorSessionID string, reserve, observe func(int64) error) (claudeChain, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return resolveClaudeChainContext(ctx, anchor, anchorSessionID, reserve, observe, ordinaryClaudeChainAccess())
}

func inspectClaudeSegmentFooter(ctx context.Context, source fileSource, sessionID string, reserve, observe func(int64) error) (claudeSegment, string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source.file == nil || !safeSessionID(sessionID) {
		return claudeSegment{}, "", "", readerDiscoveryResult(ctx, errors.New("Claude chain source is unavailable"))
	}
	footerRead := func(readCtx context.Context, start, end int64) ([]byte, error) {
		return readFileRangeObservedWithReserve(readCtx, source.file, start, end, reserve, observe)
	}
	peek := func(readCtx context.Context, offset int64) (byte, bool, error) {
		return ordinaryClaudePreviousByteContext(readCtx, source.file, offset, reserve, observe)
	}
	return inspectClaudeSegmentFooterContext(ctx, claudeSegmentSource{
		end: source.end, revision: source.revision, info: source.info,
		location: source.location, footerRead: footerRead, peek: peek,
	}, sessionID)
}

func readFileRangeObserved(ctx context.Context, file browserFile, start, end int64, observe func(int64) error) ([]byte, error) {
	return readFileRangeObservedWithReserve(ctx, file, start, end, nil, observe)
}

func readFileRangeObservedWithReserve(ctx context.Context, file browserFile, start, end int64, reserve, observe func(int64) error) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if file == nil || start < 0 || end < start {
		return nil, errors.New("invalid file range")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	length := end - start
	if length == 0 {
		return []byte{}, nil
	}
	if reserve != nil {
		if err := reserve(length); err != nil {
			return nil, err
		}
	}
	data := make([]byte, length)
	read, err := file.ReadAt(data, start)
	if read > 0 && observe != nil {
		if observeErr := observe(int64(read)); observeErr != nil {
			return nil, observeErr
		}
	}
	if err != nil {
		if !errors.Is(err, io.EOF) || int64(read) != length {
			return nil, err
		}
	}
	if int64(read) != length {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

func claudeFooterBoundaryUninspectableBytes(footer []byte, maxRecordBytes int64) bool {
	newline := bytes.IndexByte(footer, '\n')
	if newline < 0 {
		return true
	}
	lineBytes := int64(newline) + 1
	if newline+1 >= len(footer) {
		return true
	}
	return lineBytes > maxRecordBytes
}

// readCapturedSegment reads at most limit bytes ending at a descriptor's
// captured boundary. It is used by the legacy reader and deliberately verifies
// the descriptor before reading, so an old chain never grows into a new leaf.
func claudeFooterBoundaryUninspectable(file browserFile, start, end, maxRecordBytes int64) (bool, error) {
	if file == nil || start <= 0 || end <= start {
		return false, nil
	}
	var previous [1]byte
	if _, err := file.ReadAt(previous[:], start-1); err != nil {
		return false, err
	}
	if previous[0] == '\n' {
		return false, nil
	}
	// The footer window begins inside a JSONL record. It is safe to skip a
	// bounded prefix only when that record terminates inside the window; an
	// unterminated or oversized boundary record makes the footer uninspectable.
	buffer := make([]byte, 32*1024)
	var scanned int64
	for position := start; position < end; {
		want := int64(len(buffer))
		if remaining := end - position; want > remaining {
			want = remaining
		}
		read, err := file.ReadAt(buffer[:want], position)
		if read > 0 {
			if newline := bytes.IndexByte(buffer[:read], '\n'); newline >= 0 {
				lineBytes := scanned + int64(newline) + 1
				if position+int64(newline)+1 >= end {
					return true, nil
				}
				return lineBytes > maxRecordBytes, nil
			}
			scanned += int64(read)
			position += int64(read)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if read == 0 {
			break
		}
	}
	return true, nil
}

func readCapturedSegment(ctx context.Context, segment claudeSegment, limit int64) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return readCapturedSegmentContext(ctx, segment, limit, ordinaryClaudeSegmentAccess())
}

func namespaceClaudeEntries(entries []Entry, segment claudeSegment, anchor bool) []Entry {
	entries, _ = namespaceClaudeEntriesContext(context.Background(), entries, segment, anchor)
	return entries
}

func continuationDiagnostic(reason string) (bool, string) {
	switch reason {
	case continuationReasonMissingSource, continuationReasonInvalidLink, continuationReasonAmbiguous, continuationReasonCycle, continuationReasonLimit, continuationReasonPartial:
		return true, reason
	default:
		return false, ""
	}
}

func (r *Reader) readClaudeChain(_ string, sessionID string, anchor Location, before string, limit int) (Page, error) {
	return readClaudeChainContext(context.Background(), sessionID, anchor, before, limit, ordinaryClaudeChainAccess())
}

// claudeSegmentSource is available only inside its synchronous with-source
// callback. Metadata is captured; read is bound to that source's live scope.
// Neither this descriptor nor its read callback transfers a resource lease.
type claudeSegmentSource struct {
	end        int64
	revision   string
	info       os.FileInfo
	read       func(context.Context, int64, int64) ([]byte, error)
	location   Location
	footerRead func(context.Context, int64, int64) ([]byte, error)
	peek       func(context.Context, int64) (byte, bool, error)
}

// claudeSegmentAccess must be explicitly supplied. The callback context is the
// live source context, not authority. The binding owns capture and close around
// the entire callback, including abnormal exits. Hooks account physical bytes.
type claudeSegmentAccess func(context.Context, Location, func(int64) error, func(int64) error, func(context.Context, claudeSegmentSource) error) error

// ordinaryClaudeSegmentAccess constructs callbacks without effects. Its native
// read and ignored close errors are legacy presentation, never installed policy.
func ordinaryClaudeSegmentAccess() claudeSegmentAccess {
	return func(ctx context.Context, location Location, reserve, observe func(int64) error, fn func(context.Context, claudeSegmentSource) error) (err error) {
		if fn == nil {
			return conversationlife.ErrRefused
		}
		if err = readerParseContextErr(ctx); err != nil {
			return err
		}
		source, err := captureFileSourceWithHooks(location, reserve, observe)
		if err != nil {
			return readerDiscoveryResult(ctx, err)
		}
		defer func() {
			_ = source.close()
			err = readerDiscoveryResult(ctx, err)
		}()
		read := func(readCtx context.Context, start, end int64) (data []byte, err error) {
			defer func() {
				err = readerDiscoveryResult(readCtx, err)
				if err != nil {
					data = nil
				}
			}()
			if err = readerParseContextErr(readCtx); err != nil {
				return nil, err
			}
			if start < 0 || end < start || end > source.end || end-start > int64(int(^uint(0)>>1)) {
				return nil, errors.New("invalid file range")
			}
			if reserve != nil {
				if err = reserve(end - start); err != nil {
					return nil, err
				}
			}
			data = make([]byte, int(end-start))
			n, readErr := source.file.ReadAt(data, start)
			if n > 0 && observe != nil {
				if err = observe(int64(n)); err != nil {
					return nil, err
				}
			}
			// Preserve the ordinary segment's EOF/short-read presentation.
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			if int64(n) != end-start {
				return nil, io.ErrUnexpectedEOF
			}
			return data, nil
		}
		footerRead := func(readCtx context.Context, start, end int64) ([]byte, error) {
			return readFileRangeObservedWithReserve(readCtx, source.file, start, end, reserve, observe)
		}
		peek := func(readCtx context.Context, offset int64) (byte, bool, error) {
			return ordinaryClaudePreviousByteContext(readCtx, source.file, offset, reserve, observe)
		}
		return fn(ctx, claudeSegmentSource{end: source.end, revision: source.revision, info: source.info, read: read, location: source.location, footerRead: footerRead, peek: peek})
	}
}

// readCapturedSegmentContext reopens the selected path, validates the frozen
// descriptor, and reads only its captured boundary. Scratch is discarded on
// every error. Access must close before this function can publish text.
func readCapturedSegmentContext(ctx context.Context, segment claudeSegment, limit int64, access claudeSegmentAccess) (text string, clipped bool, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			text, clipped = "", false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return "", false, err
	}
	if access == nil {
		return "", false, conversationlife.ErrRefused
	}
	err = access(ctx, segment.Location, nil, nil, func(sourceCtx context.Context, source claudeSegmentSource) (e error) {
		defer func() { e = readerDiscoveryResult(sourceCtx, e) }()
		if e = readerParseContextErr(sourceCtx); e != nil {
			return e
		}
		if source.info == nil || source.read == nil {
			return conversationlife.ErrRefused
		}
		if source.revision != segment.FileRevision || source.end < segment.CapturedEnd ||
			source.end == segment.CapturedEnd && segment.SourceChangeToken != "" && fileChangeToken(source.info) != segment.SourceChangeToken {
			return errors.New("Claude chain source changed")
		}
		if e = readerParseContextErr(sourceCtx); e != nil {
			return e
		}
		end := segment.CapturedEnd
		if end == 0 {
			return nil
		}
		if limit < 1 {
			return errors.New("invalid Claude segment budget")
		}
		start := int64(0)
		clipped = end > limit
		if clipped {
			start = end - limit
		}
		data, e := source.read(sourceCtx, start, end)
		if e != nil {
			return e
		}
		if e = readerParseContextErr(sourceCtx); e != nil {
			return e
		}
		if clipped {
			if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
				data = data[newline+1:]
			} else {
				data = nil
			}
		}
		if e = readerParseContextErr(sourceCtx); e != nil {
			return e
		}
		text = string(data)
		return nil
	})
	return text, clipped, err
}

// Explicit capabilities: neither a context nor an incomplete bundle grants access.
type claudeChainAccess struct {
	source    claudeSegmentAccess
	contained func(context.Context, string, string) (string, error)
}

func (access claudeChainAccess) complete() bool {
	return access.source != nil && access.contained != nil
}

func ordinaryClaudeChainAccess() claudeChainAccess {
	return claudeChainAccess{
		source: ordinaryClaudeSegmentAccess(),
		contained: func(ctx context.Context, path, root string) (string, error) {
			return containedRegularFileContext(ctx, ordinaryReaderDiscoveryDeps(), path, root)
		},
	}
}

// Native legacy peek policy is intentionally distinct from segment range reads.
func ordinaryClaudePreviousByteContext(ctx context.Context, file browserFile, offset int64, reserve, observe func(int64) error) (value byte, present bool, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			value, present = 0, false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return 0, false, err
	}
	if file == nil || offset < 0 {
		return 0, false, conversationlife.ErrRefused
	}
	if reserve != nil {
		if err = reserve(1); err != nil {
			return 0, false, err
		}
	}
	if err = readerParseContextErr(ctx); err != nil {
		return 0, false, err
	}
	var previous [1]byte
	n, readErr := file.ReadAt(previous[:], offset)
	var observeErr error
	if n > 0 && observe != nil {
		observeErr = observe(int64(n))
	}
	if readErr == io.EOF {
		readErr = nil
	}
	if err = errors.Join(readErr, observeErr); err != nil {
		return 0, false, err
	}
	return previous[0], n > 0, nil
}

func resolveClaudeChainContext(ctx context.Context, anchor Location, anchorSessionID string, reserve, observe func(int64) error, access claudeChainAccess) (chain claudeChain, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			chain = claudeChain{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return chain, err
	}
	if !access.complete() {
		return chain, conversationlife.ErrRefused
	}
	anchorSessionID = strings.TrimSpace(anchorSessionID)
	if err = readerParseContextErr(ctx); err != nil {
		return chain, err
	}
	valid, err := claudeSafeSessionIDContext(ctx, anchorSessionID)
	if err != nil {
		return chain, err
	}
	if anchor.Path == "" || anchor.Root == "" || !valid {
		return chain, errors.New("invalid Claude chain anchor")
	}
	projectDir := filepath.Dir(anchor.Path)
	if err = readerParseContextErr(ctx); err != nil {
		return chain, err
	}
	chain = claudeChain{Segments: make([]claudeSegment, 0, 4)}
	seenSessions := make(map[string]bool)
	seenFiles := make(map[string]bool)
	seenIdentities := make(map[string]bool)
	location, sessionID := anchor, anchorSessionID
	var footerBudget int64
	for hop := 0; hop < claudeContinuationMaxSegments; hop++ {
		if err = readerParseContextErr(ctx); err != nil {
			return chain, err
		}
		var segment claudeSegment
		var childID, inspectReason, identity string
		err = access.source(ctx, location, reserve, observe, func(sourceCtx context.Context, source claudeSegmentSource) (e error) {
			defer func() { e = readerDiscoveryResult(sourceCtx, e) }()
			segment, childID, inspectReason, e = inspectClaudeSegmentFooterContext(sourceCtx, source, sessionID)
			if e != nil {
				return e
			}
			if e = readerParseContextErr(sourceCtx); e != nil {
				return e
			}
			identity = fileIdentity(source.info)
			if e = readerParseContextErr(sourceCtx); e != nil {
				return e
			}
			if identity == "" {
				identity = filepath.Clean(segment.Location.Path)
			}
			return nil
		})
		// The source has physically closed. Only the enclosing context is live.
		err = readerDiscoveryResult(ctx, err)
		if terminalProviderError(err) {
			return chain, err
		}
		if err != nil {
			if errors.Is(err, errClaudeChainDiscoveryBudget) {
				chain.IncompleteReason = continuationReasonLimit
				return chain, nil
			}
			if len(chain.Segments) == 0 {
				return chain, err
			}
			chain.IncompleteReason = continuationReasonMissingSource
			return chain, nil
		}
		footerBudget += segment.FooterEnd - segment.FooterStart
		if footerBudget > claudeContinuationFooterBudget {
			chain.IncompleteReason = continuationReasonLimit
			return chain, nil
		}
		fileKey := filepath.Clean(segment.Location.Path)
		if err = readerParseContextErr(ctx); err != nil {
			return chain, err
		}
		if seenSessions[sessionID] || seenFiles[fileKey] || seenIdentities[identity] {
			chain.IncompleteReason = continuationReasonCycle
			return chain, nil
		}
		seenSessions[sessionID], seenFiles[fileKey], seenIdentities[identity] = true, true, true
		segment.FileIdentity = identity
		chain.Segments = append(chain.Segments, segment)
		if err = readerParseContextErr(ctx); err != nil {
			return chain, err
		}
		if inspectReason != "" {
			chain.IncompleteReason = inspectReason
			return chain, nil
		}
		if childID == "" {
			return chain, nil
		}
		if len(chain.Segments) >= claudeContinuationMaxSegments {
			chain.IncompleteReason = continuationReasonLimit
			return chain, nil
		}
		valid, err = claudeSafeSessionIDContext(ctx, childID)
		if err != nil {
			return chain, err
		}
		if !valid {
			chain.IncompleteReason = continuationReasonInvalidLink
			return chain, nil
		}
		childPath := filepath.Join(projectDir, childID+".jsonl")
		if err = readerParseContextErr(ctx); err != nil {
			return chain, err
		}
		// containedRegularFile follows a contained symlink but rejects an escape,
		// FIFO, device, and directory. The exact resolved path is retained so a
		// later capture cannot silently switch the source.
		resolved, containErr := access.contained(ctx, childPath, anchor.Root)
		containErr = readerDiscoveryResult(ctx, containErr)
		if terminalProviderError(containErr) {
			return chain, containErr
		}
		if containErr != nil || resolved == "" || filepath.Clean(filepath.Dir(resolved)) != filepath.Clean(projectDir) {
			chain.IncompleteReason = continuationReasonMissingSource
			return chain, nil
		}
		if err = readerParseContextErr(ctx); err != nil {
			return chain, err
		}
		location = Location{Path: resolved, Root: anchor.Root}
		sessionID = childID
	}
	chain.IncompleteReason = continuationReasonLimit
	return chain, nil
}

func inspectClaudeSegmentFooterContext(ctx context.Context, source claudeSegmentSource, sessionID string) (segment claudeSegment, childID, reason string, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			segment, childID, reason = claudeSegment{}, "", ""
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return segment, "", "", err
	}
	if source.info == nil || source.footerRead == nil || source.peek == nil || source.location.Path == "" || source.location.Root == "" || source.revision == "" || source.end < 0 {
		return segment, "", "", conversationlife.ErrRefused
	}
	valid, err := claudeSafeSessionIDContext(ctx, sessionID)
	if err != nil {
		return segment, "", "", err
	}
	if !valid {
		return segment, "", "", errors.New("Claude chain source is unavailable")
	}
	end := source.end
	start := end - claudeContinuationFooterBytes
	if start < 0 {
		start = 0
	}
	footer, err := source.footerRead(ctx, start, end)
	if err = readerDiscoveryResult(ctx, err); err != nil {
		return segment, "", "", err
	}
	digest := sha256.Sum256(footer)
	if err = readerParseContextErr(ctx); err != nil {
		return segment, "", "", err
	}
	footerDigest := hex.EncodeToString(digest[:])
	if err = readerParseContextErr(ctx); err != nil {
		return segment, "", "", err
	}
	modTime := source.info.ModTime().UnixNano()
	if err = readerParseContextErr(ctx); err != nil {
		return segment, "", "", err
	}
	changeToken := fileChangeToken(source.info)
	if err = readerParseContextErr(ctx); err != nil {
		return segment, "", "", err
	}
	segment = claudeSegment{
		SessionID: sessionID, Location: source.location, CapturedEnd: end,
		FileRevision: source.revision, SourceModTime: modTime, SourceChangeToken: changeToken,
		FooterStart: start, FooterEnd: end, FooterDigest: footerDigest,
	}
	startsInside := false
	if start > 0 {
		previous, present, peekErr := source.peek(ctx, start-1)
		if err = readerDiscoveryResult(ctx, peekErr); err != nil {
			return segment, "", "", err
		}
		startsInside = present && previous != '\n'
	}
	if startsInside {
		uninspectable := claudeFooterBoundaryUninspectableBytes(footer, claudeContinuationFooterBytes)
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		if uninspectable {
			return segment, "", continuationReasonLimit, nil
		}
	}
	records, oversized, err := collectJSONLRecordsBytes(ctx, footer, start, startsInside, claudeContinuationRecordBytes)
	if err = readerDiscoveryResult(ctx, err); err != nil {
		return segment, "", "", err
	}
	if oversized > 0 {
		return segment, "", continuationReasonLimit, nil
	}
	links := make(map[string]bool)
	invalidLink, partialLink := false, false
	for _, record := range records {
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		if record.Oversized || int64(len(record.Raw)) > claudeContinuationFooterBytes {
			return segment, "", continuationReasonLimit, nil
		}
		var raw map[string]any
		var decodeErr error
		if record.Complete {
			decodeErr = json.Unmarshal(record.Raw, &raw)
		}
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		if !record.Complete || decodeErr != nil {
			if record.Trailing {
				partial := partialClaudeContinuationType.Match(record.Raw)
				if err = readerParseContextErr(ctx); err != nil {
					return segment, "", "", err
				}
				partialLink = partialLink || partial
			}
			continue
		}
		kind := stringValue(raw["type"])
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		if kind != "continued-in" || raw["isSidechain"] == true {
			continue
		}
		if value, present := raw["sessionId"]; present {
			provided, ok := value.(string)
			if err = readerParseContextErr(ctx); err != nil {
				return segment, "", "", err
			}
			if !ok || provided == "" || provided != sessionID {
				invalidLink = true
				continue
			}
		}
		child := stringValue(raw["continuedInSessionId"])
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		child = strings.TrimSpace(child)
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		valid, err = claudeSafeSessionIDContext(ctx, child)
		if err != nil {
			return segment, "", "", err
		}
		if !valid {
			invalidLink = true
			continue
		}
		links[child] = true
	}
	if err = readerParseContextErr(ctx); err != nil {
		return segment, "", "", err
	}
	if len(links) > 1 {
		return segment, "", continuationReasonAmbiguous, nil
	}
	// A malformed or mismatched completed marker is not safe evidence for a
	// child, even when another marker in the bounded footer looks usable. A
	// trailing partial marker is different: it may become valid when Claude
	// finishes writing the record, so leave the segment at the current prefix
	// and let the next latest request inspect it again.
	if invalidLink {
		return segment, "", continuationReasonInvalidLink, nil
	}
	if len(links) == 0 {
		if partialLink {
			return segment, "", continuationReasonPartial, nil
		}
		return segment, "", "", nil
	}
	for child := range links {
		if err = readerParseContextErr(ctx); err != nil {
			return segment, "", "", err
		}
		return segment, child, "", nil
	}
	return segment, "", "", nil
}

// Same safe-ID grammar, with supplied-context checkpoints in the scalar loop.
func claudeSafeSessionIDContext(ctx context.Context, value string) (valid bool, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			valid = false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return false, err
	}
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false, nil
	}
	for _, character := range value {
		if err = readerParseContextErr(ctx); err != nil {
			return false, err
		}
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false, nil
		}
	}
	return true, nil
}

func namespaceClaudeEntriesContext(ctx context.Context, entries []Entry, segment claudeSegment, anchor bool) (result []Entry, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			result = nil
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	if anchor || len(entries) == 0 {
		return entries, nil
	}
	hash := sha256.Sum256([]byte(segment.SessionID + "\x00" + segment.FileRevision))
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	prefix := hex.EncodeToString(hash[:])[:12]
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	for index := range entries {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		entries[index].ID = prefix + "-" + entries[index].ID
	}
	return entries, nil
}

func readClaudeChainContext(ctx context.Context, sessionID string, anchor Location, before string, limit int, access claudeChainAccess) (page Page, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			page = Page{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Page{}, err
	}
	if !access.complete() {
		return Page{}, conversationlife.ErrRefused
	}
	chain, err := resolveClaudeChainContext(ctx, anchor, sessionID, nil, nil, access)
	if err != nil {
		return Page{}, fmt.Errorf("resolve Claude conversation chain: %w", err)
	}
	if err = readerParseContextErr(ctx); err != nil {
		return Page{}, err
	}
	if len(chain.Segments) == 0 {
		return unavailableCode("invalid_session", "No conversation log is available for this session."), nil
	}
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	groups := make([][]Entry, len(chain.Segments))
	remaining := int64(maxConversationBytes)
	clipped := false
	for index := len(chain.Segments) - 1; index >= 0; index-- {
		if err = readerParseContextErr(ctx); err != nil {
			return Page{}, err
		}
		if remaining <= 0 {
			clipped = true
			break
		}
		segment := chain.Segments[index]
		budget := remaining
		if segment.CapturedEnd < budget {
			budget = segment.CapturedEnd
		}
		text, segmentClipped, readErr := readCapturedSegmentContext(ctx, segment, budget, access.source)
		readErr = readerDiscoveryResult(ctx, readErr)
		if terminalProviderError(readErr) {
			return Page{}, fmt.Errorf("read Claude conversation segment: %w", readErr)
		}
		if readErr != nil {
			if index == len(chain.Segments)-1 {
				return Page{}, fmt.Errorf("read Claude conversation segment: %w", readErr)
			}
			chain.IncompleteReason = continuationReasonMissingSource
			clipped = true
			break
		}
		entries, parseErr := parseTranscriptContext(ctx, "claude", text)
		if parseErr != nil {
			return Page{}, parseErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return Page{}, err
		}
		groups[index], err = namespaceClaudeEntriesContext(ctx, entries, segment, index == 0)
		if err != nil {
			return Page{}, err
		}
		if err = readerParseContextErr(ctx); err != nil {
			return Page{}, err
		}
		consumed := segment.CapturedEnd
		if consumed > remaining {
			consumed = remaining
		}
		remaining -= consumed
		clipped = clipped || segmentClipped
		if segmentClipped {
			if index > 0 {
				clipped = true
			}
			break
		}
	}
	entries := make([]Entry, 0)
	for _, group := range groups {
		if err = readerParseContextErr(ctx); err != nil {
			return Page{}, err
		}
		for _, entry := range group {
			if err = readerParseContextErr(ctx); err != nil {
				return Page{}, err
			}
			entries = append(entries, entry)
		}
	}
	end := len(entries)
	if before != "" {
		found := false
		for index := range entries {
			if err = readerParseContextErr(ctx); err != nil {
				return Page{}, err
			}
			if entries[index].ID == before {
				end = index
				found = true
				break
			}
		}
		if !found {
			return Page{Available: true, ReasonCode: "source_changed", Reason: "The conversation chain changed; reload history.", Entries: []Entry{}}, nil
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	var pageEntries []Entry
	for index := start; index < end; index++ {
		if err = readerParseContextErr(ctx); err != nil {
			return Page{}, err
		}
		pageEntries = append(pageEntries, entries[index])
	}
	if err = readerParseContextErr(ctx); err != nil {
		return Page{}, err
	}
	page = Page{
		Available: true, Entries: pageEntries, HasMore: start > 0, Total: len(entries), FileTruncated: clipped,
	}
	if chain.IncompleteReason != "" {
		page.ContinuationIncomplete, page.ContinuationReason = continuationDiagnostic(chain.IncompleteReason)
	}
	return page, nil
}
