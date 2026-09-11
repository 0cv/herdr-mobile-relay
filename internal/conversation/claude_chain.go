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
type claudeSegment struct {
	SessionID    string
	Location     Location
	CapturedEnd  int64
	FileRevision string
	// SourceModTime detects equal-length in-place rewrites without changing the
	// normal snapshot revision semantics, which intentionally allow appends.
	SourceModTime int64
	// FileIdentity catches aliases which have different safe paths but refer to
	// the same inode. It is private descriptor data and never crosses the wire.
	FileIdentity string
	// RecentDigest is populated by a browser context once it has read the
	// selected frozen range. The resolver deliberately does not hash whole
	// transcripts while discovering a chain.
	RecentStart    int64
	RecentEnd      int64
	RecentDigest   string
	CapturedDigest string
	FooterStart    int64
	FooterEnd      int64
	FooterDigest   string
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
	if ctx == nil {
		ctx = context.Background()
	}
	anchorSessionID = strings.TrimSpace(anchorSessionID)
	if anchor.Path == "" || anchor.Root == "" || !safeSessionID(anchorSessionID) {
		return claudeChain{}, errors.New("invalid Claude chain anchor")
	}
	projectDir := filepath.Dir(anchor.Path)
	chain := claudeChain{Segments: make([]claudeSegment, 0, 4)}
	seenSessions := make(map[string]bool)
	seenFiles := make(map[string]bool)
	seenIdentities := make(map[string]bool)
	location := anchor
	sessionID := anchorSessionID
	var footerBudget int64

	for hop := 0; hop < claudeContinuationMaxSegments; hop++ {
		if err := ctx.Err(); err != nil {
			return chain, err
		}
		source, err := captureFileSource(location)
		if err != nil {
			if len(chain.Segments) == 0 {
				return chain, err
			}
			chain.IncompleteReason = continuationReasonMissingSource
			return chain, nil
		}
		segment, childID, inspectReason, inspectErr := inspectClaudeSegmentFooter(ctx, source, sessionID)
		identity := fileIdentity(source.info)
		if identity == "" {
			identity = filepath.Clean(segment.Location.Path)
		}
		source.close()
		if inspectErr != nil {
			if len(chain.Segments) == 0 {
				return chain, inspectErr
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
		if seenSessions[sessionID] || seenFiles[fileKey] || seenIdentities[identity] {
			chain.IncompleteReason = continuationReasonCycle
			return chain, nil
		}
		seenSessions[sessionID] = true
		seenFiles[fileKey] = true
		seenIdentities[identity] = true
		segment.FileIdentity = identity
		chain.Segments = append(chain.Segments, segment)
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
		if !safeSessionID(childID) {
			chain.IncompleteReason = continuationReasonInvalidLink
			return chain, nil
		}
		childPath := filepath.Join(projectDir, childID+".jsonl")
		// containedRegularFile follows a contained symlink but rejects an escape,
		// FIFO, device, and directory. The exact resolved path is retained so a
		// later capture cannot silently switch the source.
		resolved := containedRegularFile(childPath, anchor.Root)
		if resolved == "" || filepath.Clean(filepath.Dir(resolved)) != filepath.Clean(projectDir) {
			chain.IncompleteReason = continuationReasonMissingSource
			return chain, nil
		}
		location = Location{Path: resolved, Root: anchor.Root}
		sessionID = childID
	}
	chain.IncompleteReason = continuationReasonLimit
	return chain, nil
}

func inspectClaudeSegmentFooter(ctx context.Context, source fileSource, sessionID string) (claudeSegment, string, string, error) {
	if source.file == nil || !safeSessionID(sessionID) {
		return claudeSegment{}, "", continuationReasonMissingSource, errors.New("Claude chain source is unavailable")
	}
	end := source.end
	start := end - claudeContinuationFooterBytes
	if start < 0 {
		start = 0
	}
	digest, err := fileRangeDigest(ctx, source.file, start, end)
	if err != nil {
		return claudeSegment{}, "", continuationReasonMissingSource, err
	}
	segment := claudeSegment{
		SessionID: sessionID, Location: source.location, CapturedEnd: end,
		FileRevision: source.revision, SourceModTime: source.info.ModTime().UnixNano(),
		FooterStart: start, FooterEnd: end, FooterDigest: digest,
	}
	if uninspectable, boundaryErr := claudeFooterBoundaryUninspectable(source.file, start, end, claudeContinuationFooterBytes); boundaryErr != nil {
		return segment, "", continuationReasonMissingSource, boundaryErr
	} else if uninspectable {
		return segment, "", continuationReasonLimit, nil
	}
	records, oversized, err := collectJSONLRecords(ctx, source.file, start, end, claudeContinuationRecordBytes, nil)
	if err != nil {
		return segment, "", continuationReasonMissingSource, err
	}
	if oversized > 0 {
		return segment, "", continuationReasonLimit, nil
	}
	links := make(map[string]bool)
	invalidLink := false
	partialLink := false
	for _, record := range records {
		if record.Oversized || int64(len(record.Raw)) > claudeContinuationFooterBytes {
			return segment, "", continuationReasonLimit, nil
		}
		var raw map[string]any
		if !record.Complete || json.Unmarshal(record.Raw, &raw) != nil {
			if record.Trailing && partialClaudeContinuationType.Match(record.Raw) {
				partialLink = true
			}
			continue
		}
		if stringValue(raw["type"]) != "continued-in" || raw["isSidechain"] == true {
			continue
		}
		if value, present := raw["sessionId"]; present {
			provided, ok := value.(string)
			if !ok || provided == "" || provided != sessionID {
				invalidLink = true
				continue
			}
		}
		child := strings.TrimSpace(stringValue(raw["continuedInSessionId"]))
		if child == "" || !safeSessionID(child) {
			invalidLink = true
			continue
		}
		links[child] = true
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
		return segment, child, "", nil
	}
	return segment, "", "", nil
}

// readCapturedSegment reads at most limit bytes ending at a descriptor's
// captured boundary. It is used by the legacy reader and deliberately verifies
// the descriptor before reading, so an old chain never grows into a new leaf.
func claudeFooterBoundaryUninspectable(file *os.File, start, end, maxRecordBytes int64) (bool, error) {
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
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	source, err := captureFileSource(segment.Location)
	if err != nil {
		return "", false, err
	}
	defer source.close()
	if source.revision != segment.FileRevision || source.end < segment.CapturedEnd {
		return "", false, errors.New("Claude chain source changed")
	}
	end := segment.CapturedEnd
	if end == 0 {
		return "", false, nil
	}
	if limit < 1 {
		return "", false, errors.New("invalid Claude segment budget")
	}
	start := int64(0)
	clipped := end > limit
	if clipped {
		start = end - limit
	}
	data := make([]byte, end-start)
	if _, err := source.file.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	if clipped {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	return string(data), clipped, nil
}

func namespaceClaudeEntries(entries []Entry, segment claudeSegment, anchor bool) []Entry {
	if anchor || len(entries) == 0 {
		return entries
	}
	hash := sha256.Sum256([]byte(segment.SessionID + "\x00" + segment.FileRevision))
	prefix := hex.EncodeToString(hash[:])[:12]
	for index := range entries {
		entries[index].ID = prefix + "-" + entries[index].ID
	}
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
	chain, err := resolveClaudeChain(context.Background(), anchor, sessionID)
	if err != nil {
		return Page{}, fmt.Errorf("resolve Claude conversation chain: %w", err)
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
		if remaining <= 0 {
			clipped = true
			break
		}
		segment := chain.Segments[index]
		budget := remaining
		if segment.CapturedEnd < budget {
			budget = segment.CapturedEnd
		}
		text, segmentClipped, readErr := readCapturedSegment(context.Background(), segment, budget)
		if readErr != nil {
			if index == len(chain.Segments)-1 {
				return Page{}, fmt.Errorf("read Claude conversation segment: %w", readErr)
			}
			chain.IncompleteReason = continuationReasonMissingSource
			clipped = true
			break
		}
		entries := parseTranscript("claude", text)
		groups[index] = namespaceClaudeEntries(entries, segment, index == 0)
		consumed := segment.CapturedEnd
		if consumed > remaining {
			consumed = remaining
		}
		remaining -= consumed
		clipped = clipped || segmentClipped
		if segmentClipped {
			// The tail of this segment is readable, but no older segment can be
			// certified within the shared 16 MiB legacy budget.
			if index > 0 {
				clipped = true
			}
			break
		}
	}
	entries := make([]Entry, 0)
	for _, group := range groups {
		entries = append(entries, group...)
	}
	end := len(entries)
	if before != "" {
		found := false
		for index := range entries {
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
	pageEntries := append([]Entry(nil), entries[start:end]...)
	page := Page{
		Available: true, Entries: pageEntries, HasMore: start > 0, Total: len(entries), FileTruncated: clipped,
	}
	if chain.IncompleteReason != "" {
		page.ContinuationIncomplete, page.ContinuationReason = continuationDiagnostic(chain.IncompleteReason)
	}
	return page, nil
}
