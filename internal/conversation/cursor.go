package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

var multiDash = regexp.MustCompile(`-+`)

// cursorProjectSlug mirrors Cursor's on-disk project directory naming under
// ~/.cursor/projects: strip a leading slash, replace '/' and '.' with '-',
// collapse repeated dashes, and trim edge dashes. Verified against Cursor
// Agent (e.g. /Users/christophe.vidal/Documents → Users-christophe-vidal-Documents).
func cursorProjectSlug(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(cwd) {
		return ""
	}
	trimmed := strings.TrimPrefix(cwd, string(filepath.Separator))
	trimmed = strings.ReplaceAll(trimmed, ".", "-")
	trimmed = strings.ReplaceAll(trimmed, string(filepath.Separator), "-")
	trimmed = multiDash.ReplaceAllString(trimmed, "-")
	return strings.Trim(trimmed, "-")
}

func cursorTranscriptRels(sessionID string) []string {
	id := strings.ToLower(strings.TrimSpace(sessionID))
	return []string{
		filepath.Join("agent-transcripts", id, id+".jsonl"), // nested (current)
		filepath.Join("agent-transcripts", id+".jsonl"),     // flat (older)
	}
}

func findCursorSession(roots []string, project ProjectContext, sessionID string) Location {
	location, _ := findCursorSessionContext(context.Background(), ordinaryReaderDiscoveryDeps(), roots, project, sessionID)
	return location
}

func findCursorByWorkspaceTrusted(root, cwd string, rels []string) Location {
	location, _ := findCursorByWorkspaceTrustedContext(context.Background(), ordinaryReaderDiscoveryDeps(), root, cwd, rels)
	return location
}

func cursorTranscriptInProject(root, projectName string, rels []string) Location {
	location, _ := cursorTranscriptInProjectContext(context.Background(), ordinaryReaderDiscoveryDeps(), root, projectName, rels)
	return location
}

func scanCursorSession(root, rel string) Location {
	location, _ := scanCursorSessionContext(context.Background(), ordinaryReaderDiscoveryDeps(), root, rel)
	return location
}

func parseCursorRecord(record map[string]any) (string, string) {
	role, text, _ := parseCursorRecordContext(context.Background(), record)
	return role, text
}

// humanCursorText keeps only explicit <user_query> bodies. User records without
// that envelope are harness-injected (subagent/system notifications) and must
// stay hidden, matching Claude/Codex filtering of their injected envelopes.
func humanCursorText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	return innerTag(trimmed, "user_query")
}

func cursorTimestampFromRecord(record map[string]any) string {
	stamp, _ := cursorTimestampFromRecordContext(context.Background(), record)
	return stamp
}

// normalizeCursorTimestamp parses Cursor's <timestamp> format and returns RFC 3339 UTC,
// or empty string if parsing fails. Cursor emits "Weekday, Mon DD, YYYY, H:MM AM/PM (UTC±N)"
// but JavaScript Date() constructors ignore parenthesized timezone suffixes, so we must
// parse the offset explicitly and convert to UTC.
func normalizeCursorTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	offsetStr := ""
	if idx := strings.LastIndex(raw, "("); idx >= 0 {
		if end := strings.Index(raw[idx:], ")"); end >= 0 {
			offsetStr = raw[idx+1 : idx+end]
			raw = strings.TrimSpace(raw[:idx])
		}
	}

	if offsetStr == "" || !strings.HasPrefix(offsetStr, "UTC") {
		return ""
	}

	offset := strings.TrimPrefix(offsetStr, "UTC")
	offsetMinutes := 0
	if offset != "" {
		sign := 1
		if strings.HasPrefix(offset, "-") {
			sign = -1
			offset = offset[1:]
		} else if strings.HasPrefix(offset, "+") {
			offset = offset[1:]
		}

		parts := strings.Split(offset, ":")
		hours, errH := strconv.Atoi(parts[0])
		if errH != nil {
			return ""
		}
		offsetMinutes = sign * hours * 60
		if len(parts) > 1 {
			mins, errM := strconv.Atoi(parts[1])
			if errM != nil {
				return ""
			}
			offsetMinutes += sign * mins
		}
	}

	var t time.Time
	var err error
	if t, err = time.Parse("Monday, Jan 2, 2006, 3:04 PM", raw); err != nil {
		return ""
	}

	t = t.Add(-time.Duration(offsetMinutes) * time.Minute)
	return t.UTC().Format(time.RFC3339)
}

func parseCursorRecordContext(ctx context.Context, record map[string]any) (role string, text string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			role, text = "", ""
		}
	}()
	role = stringValue(record["role"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	role = strings.TrimSpace(role)
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	role = strings.ToLower(role)
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if role != "user" && role != "assistant" {
		return "", "", nil
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", "", nil
	}
	content := message["content"]
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if raw, ok := content.(string); ok {
		if role == "user" {
			return role, humanCursorText(raw), nil
		}
		return role, raw, nil
	}
	if role == "user" {
		blocks, blockErr := textBlockListContext(ctx, content)
		if blockErr != nil {
			return "", "", blockErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return "", "", err
		}
		kept := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if err = readerParseContextErr(ctx); err != nil {
				return "", "", err
			}
			filtered := humanCursorText(block)
			if err = readerParseContextErr(ctx); err != nil {
				return "", "", err
			}
			nonempty := strings.TrimSpace(filtered) != ""
			if err = readerParseContextErr(ctx); err != nil {
				return "", "", err
			}
			if nonempty {
				kept = append(kept, filtered)
			}
		}
		if err = readerParseContextErr(ctx); err != nil {
			return "", "", err
		}
		if len(kept) == 0 {
			return "", "", nil
		}
		return role, strings.Join(kept, "\n"), nil
	}
	text, err = textBlocksContext(ctx, content)
	return role, text, err
}

func cursorTimestampFromRecordContext(ctx context.Context, record map[string]any) (stamp string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			stamp = ""
		}
	}()
	role := stringValue(record["role"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	role = strings.TrimSpace(role)
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	role = strings.ToLower(role)
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	if role != "user" {
		return "", nil
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", nil
	}
	content := message["content"]
	var raw string
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	if s, ok := content.(string); ok {
		raw = innerTag(s, "timestamp")
		if err = readerParseContextErr(ctx); err != nil {
			return "", err
		}
	} else {
		blocks, blockErr := textBlockListContext(ctx, content)
		if blockErr != nil {
			return "", blockErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return "", err
		}
		for _, block := range blocks {
			if err = readerParseContextErr(ctx); err != nil {
				return "", err
			}
			candidate := innerTag(block, "timestamp")
			if err = readerParseContextErr(ctx); err != nil {
				return "", err
			}
			if candidate != "" {
				raw = candidate
				break
			}
		}
	}
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	if raw == "" {
		return "", nil
	}
	return normalizeCursorTimestamp(raw), nil
}

func findCursorSessionContext(ctx context.Context, d readerDiscoveryDeps, roots []string, project ProjectContext, sessionID string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	canonical := canonicalSessionID.MatchString(sessionID)
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !canonical {
		return Location{}, nil
	}
	rels := cursorTranscriptRels(sessionID)
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	candidates := projectDirectoriesForContext(project)
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	for _, root := range roots {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		seenSlug := make(map[string]bool)
		for _, cwd := range candidates {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			slug := cursorProjectSlug(cwd)
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			if slug == "" || seenSlug[slug] {
				continue
			}
			seenSlug[slug] = true
			location, err = cursorTranscriptInProjectContext(ctx, d, root, slug, rels)
			if err = readerDiscoveryResult(ctx, err); err != nil {
				return Location{}, err
			}
			if location.Path != "" {
				return location, nil
			}
		}
		for _, cwd := range candidates {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			location, err = findCursorByWorkspaceTrustedContext(ctx, d, root, cwd, rels)
			if err = readerDiscoveryResult(ctx, err); err != nil {
				return Location{}, err
			}
			if location.Path != "" {
				return location, nil
			}
		}
		for _, rel := range rels {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			location, err = scanCursorSessionContext(ctx, d, root, rel)
			if err = readerDiscoveryResult(ctx, err); err != nil {
				return Location{}, err
			}
			if location.Path != "" {
				return location, nil
			}
		}
	}
	return Location{}, nil
}

func findCursorByWorkspaceTrustedContext(ctx context.Context, d readerDiscoveryDeps, root, cwd string, rels []string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	cwd = strings.TrimSpace(cwd)
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if cwd == "" {
		return Location{}, nil
	}

	// Resolve symlinks once for the target path.
	var resolvedCWD string
	errCWD := sourceEffect(ctx, func() (effectErr error) { resolvedCWD, effectErr = d.eval(cwd); return })
	if terminalProviderError(errCWD) {
		return Location{}, errCWD
	}
	var entries []os.DirEntry
	err = sourceEffect(ctx, func() (effectErr error) { entries, effectErr = d.readDir(root); return })
	if terminalProviderError(err) {
		return Location{}, err
	}
	if err != nil {
		return Location{}, nil
	}
	for _, entry := range entries {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		projectDir := filepath.Join(root, entry.Name())
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		directory, directoryErr := isDirContext(ctx, d, projectDir)
		if err = readerDiscoveryResult(ctx, directoryErr); err != nil {
			return Location{}, err
		}
		if !directory {
			continue
		}
		trustedPath, trustedErr := containedRegularFileContext(ctx, d, filepath.Join(projectDir, ".workspace-trusted"), root)
		if err = readerDiscoveryResult(ctx, trustedErr); err != nil {
			return Location{}, err
		}
		if trustedPath == "" {
			continue
		}
		var data []byte
		err = sourceEffect(ctx, func() (effectErr error) {
			data, effectErr = d.readContent(ctx, Location{Path: trustedPath, Root: root})
			return
		})
		if terminalProviderError(err) {
			return Location{}, err
		}
		if err != nil {
			continue
		}
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		var payload struct {
			WorkspacePath string `json:"workspacePath"`
		}
		decodeErr := json.Unmarshal(data, &payload)
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		if decodeErr != nil {
			continue
		}
		recorded := strings.TrimSpace(payload.WorkspacePath)
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		if recorded == "" {
			continue
		}

		// Compare paths: direct match or resolved match.
		match := (cwd == recorded)
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		if !match && errCWD == nil {
			var resolvedRecorded string
			errRecorded := sourceEffect(ctx, func() (effectErr error) { resolvedRecorded, effectErr = d.eval(recorded); return })
			if terminalProviderError(errRecorded) {
				return Location{}, errRecorded
			}
			match = (errRecorded == nil && resolvedCWD == resolvedRecorded)
		}
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		if !match {
			continue
		}
		location, err = cursorTranscriptInProjectContext(ctx, d, root, entry.Name(), rels)
		if err = readerDiscoveryResult(ctx, err); err != nil {
			return Location{}, err
		}
		if location.Path != "" {
			return location, nil
		}
	}
	return Location{}, nil
}

func cursorTranscriptInProjectContext(ctx context.Context, d readerDiscoveryDeps, root, projectName string, rels []string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	for _, rel := range rels {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		path := filepath.Join(root, projectName, rel)
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		found, foundErr := containedRegularFileContext(ctx, d, path, root)
		if err = readerDiscoveryResult(ctx, foundErr); err != nil {
			return Location{}, err
		}
		if found != "" {
			return Location{Path: found, Root: root}, nil
		}
	}
	return Location{}, nil
}

func scanCursorSessionContext(ctx context.Context, d readerDiscoveryDeps, root, rel string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	var entries []os.DirEntry
	err = sourceEffect(ctx, func() (effectErr error) { entries, effectErr = d.readDir(root); return })
	if terminalProviderError(err) {
		return Location{}, err
	}
	if err != nil {
		return Location{}, nil
	}
	for _, entry := range entries {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		projectDir := filepath.Join(root, entry.Name())
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		directory, directoryErr := isDirContext(ctx, d, projectDir)
		if err = readerDiscoveryResult(ctx, directoryErr); err != nil {
			return Location{}, err
		}
		if !directory {
			continue
		}
		path := filepath.Join(projectDir, rel)
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		found, foundErr := containedRegularFileContext(ctx, d, path, root)
		if err = readerDiscoveryResult(ctx, foundErr); err != nil {
			return Location{}, err
		}
		if found != "" {
			return Location{Path: found, Root: root}, nil
		}
	}
	return Location{}, nil
}
