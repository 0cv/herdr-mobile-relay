package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const (
	maxKimiMetadata = 256 * 1024
	maxKimiSessions = 2048
	maxKimiRecords  = 100000
	maxKimiRecord   = 1024 * 1024
)

var kimiSlugRun = regexp.MustCompile(`[^a-z0-9._-]+`)

func IsKimi(agent string) bool {
	switch normalizedAgent(agent) {
	case "kimi", "kimicode":
		return true
	default:
		return false
	}
}

func kimiWorkspaceKey(cwd string) string {
	normalized := kimiDirectoryKey(cwd)
	parts := strings.Split(normalized, "/")
	slug := strings.Trim(kimiSlugRun.ReplaceAllString(strings.ToLower(parts[len(parts)-1]), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" || slug == "." || slug == ".." {
		slug = "workspace"
	}
	digest := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("wd_%s_%x", slug, digest[:6])
}

func kimiDirectoryKey(cwd string) string {
	return strings.TrimRight(strings.ReplaceAll(cwd, "\\", "/"), "/")
}

func kimiCWD(project ProjectContext) string {
	if project.ForegroundCWD != "" {
		return project.ForegroundCWD
	}
	return project.CWD
}

func readKimiFile(location Location, limit int64) ([]byte, bool) {
	location.Path = containedRegularFile(location.Path, location.Root)
	file, info, err := openContainedConversationFile(location)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	if info.Size() > limit {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	pathInfo, pathErr := os.Stat(location.Path)
	return data, err == nil && statErr == nil && pathErr == nil && os.SameFile(info, pathInfo) && len(data) <= int(limit) && after.Size() == info.Size() && fileChangeToken(after) == fileChangeToken(info)
}

type kimiState struct {
	ID        string          `json:"id"`
	Version   int             `json:"version"`
	CWD       string          `json:"cwd"`
	WorkDir   string          `json:"workDir"`
	UpdatedAt json.RawMessage `json:"updatedAt"`
	Archived  bool            `json:"archived"`
	Title     string          `json:"title"`
	Custom    struct {
		CWD      string `json:"cwd"`
		Imported bool   `json:"imported_from_kimi_cli"`
		Archived bool   `json:"archived"`
	} `json:"custom"`
}

type kimiIndexEntry struct {
	SessionID  string `json:"sessionId"`
	SessionDir string `json:"sessionDir"`
	WorkDir    string `json:"workDir"`
	Deleted    bool   `json:"deleted"`
}

func kimiIndex(root string, metadataBytes *int) (map[string]kimiIndexEntry, bool) {
	out := make(map[string]kimiIndexEntry)
	path := filepath.Join(root, "session_index.jsonl")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return out, true
	}
	data, ok := readKimiFile(Location{Root: root, Path: path}, maxKimiMetadata)
	if !ok {
		return nil, false
	}
	*metadataBytes += len(data)
	if *metadataBytes > 4*1024*1024 {
		return nil, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item kimiIndexEntry
		if json.Unmarshal([]byte(line), &item) != nil || !safeSessionID(item.SessionID) {
			return nil, false
		}
		out[item.SessionID] = item
	}
	return out, true
}

func kimiDirectories(root, path string, remaining *int) ([]string, bool) {
	realRoot, rootErr := filepath.EvalSymlinks(root)
	realPath, pathErr := filepath.EvalSymlinks(path)
	if os.IsNotExist(rootErr) || os.IsNotExist(pathErr) {
		return nil, true
	}
	if rootErr != nil || pathErr != nil {
		return nil, false
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Join(realRoot, strings.TrimPrefix(filepath.Clean(path), filepath.Clean(root)+string(filepath.Separator))) != realPath {
		return nil, false
	}
	file, err := openConversationSource(realPath)
	if os.IsNotExist(err) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	defer file.Close()
	opened, statErr := file.Stat()
	current, pathStatErr := os.Stat(realPath)
	if statErr != nil || pathStatErr != nil || !opened.IsDir() || !os.SameFile(opened, current) {
		return nil, false
	}
	entries, err := file.ReadDir(*remaining + 1)
	if err != nil && err != io.EOF || len(entries) > *remaining {
		return nil, false
	}
	*remaining -= len(entries)
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() && safeSessionID(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, true
}

func (r *Reader) selectKimiSession(project ProjectContext, reported string, watch func(string, bool)) (string, Location, string) {
	cwd := kimiCWD(project)
	if !filepath.IsAbs(cwd) || reported != "" && !safeSessionID(reported) {
		return "", Location{}, "invalid_session"
	}
	if strings.HasPrefix(reported, "ses_") {
		return "", Location{}, "kimi_imported"
	}
	remaining := maxKimiSessions
	metadataBytes := 0
	for _, root := range agentroots.KimiCodeHomes(r.home) {
		watch(root, false)
		watch(filepath.Join(root, "session_index.jsonl"), false)
		watch(filepath.Join(root, "sessions"), false)
		index, ok := kimiIndex(root, &metadataBytes)
		if !ok {
			if metadataBytes > 4*1024*1024 {
				return "", Location{}, "output_limit"
			}
			return "", Location{}, "source_corrupt"
		}
		buckets, ok := kimiDirectories(root, filepath.Join(root, "sessions"), &remaining)
		if !ok {
			return "", Location{}, "output_limit"
		}
		preferred := kimiWorkspaceKey(cwd)
		sort.SliceStable(buckets, func(i, j int) bool { return buckets[i] == preferred && buckets[j] != preferred })
		type candidate struct {
			id       string
			location Location
			updated  int64
		}
		candidates := []candidate{}
		for _, bucket := range buckets {
			bucketPath := filepath.Join(root, "sessions", bucket)
			watch(bucketPath, false)
			ids := []string{reported}
			if reported == "" {
				ids, ok = kimiDirectories(root, bucketPath, &remaining)
				if !ok {
					return "", Location{}, "output_limit"
				}
			}
			for _, id := range ids {
				dir := filepath.Join(bucketPath, id)
				watch(filepath.Join(dir, "state.json"), false)
				data, valid := readKimiFile(Location{Root: root, Path: filepath.Join(dir, "state.json")}, maxKimiMetadata)
				if !valid {
					continue
				}
				metadataBytes += len(data)
				if metadataBytes > 4*1024*1024 {
					return "", Location{}, "output_limit"
				}
				var state kimiState
				if json.Unmarshal(data, &state) != nil {
					return "", Location{}, "source_corrupt"
				}
				storedCWD := state.CWD
				if storedCWD == "" {
					storedCWD = state.WorkDir
				}
				if storedCWD == "" {
					storedCWD = state.Custom.CWD
				}
				if state.ID != "" && state.ID != id || state.Version == 2 && state.ID != id || state.Version != 0 && state.Version != 2 {
					if reported != "" {
						return "", Location{}, "invalid_session"
					}
					continue
				}
				if state.Custom.Imported || strings.HasPrefix(id, "ses_") {
					if reported != "" {
						return "", Location{}, "kimi_imported"
					}
					continue
				}
				if storedCWD == "" || kimiDirectoryKey(storedCWD) != kimiDirectoryKey(cwd) {
					continue
				}
				if state.CWD != "" && state.WorkDir != "" && kimiDirectoryKey(state.CWD) != kimiDirectoryKey(state.WorkDir) || state.Custom.CWD != "" && kimiDirectoryKey(state.Custom.CWD) != kimiDirectoryKey(storedCWD) {
					return "", Location{}, "invalid_session"
				}
				if item, exists := index[id]; exists {
					if item.Deleted {
						continue
					}
					if item.SessionDir != dir || kimiDirectoryKey(item.WorkDir) != kimiDirectoryKey(cwd) {
						return "", Location{}, "invalid_session"
					}
				}
				var updated int64
				if json.Unmarshal(state.UpdatedAt, &updated) != nil {
					var stamp string
					if json.Unmarshal(state.UpdatedAt, &stamp) == nil {
						if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
							updated = parsed.UnixMilli()
						}
					}
				}
				if reported == "" && (state.Archived || state.Custom.Archived || updated <= 0) {
					continue
				}
				watch(filepath.Join(dir, "agents", "main", "wire.jsonl"), true)
				path := containedRegularFile(filepath.Join(dir, "agents", "main", "wire.jsonl"), root)
				if path == "" {
					continue
				}
				candidates = append(candidates, candidate{id, Location{Path: path, Root: root, Title: sanitizeText(state.Title)}, updated})
			}
		}
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].updated != candidates[j].updated {
				return candidates[i].updated > candidates[j].updated
			}
			return candidates[i].id > candidates[j].id
		})
		if reported != "" && len(candidates) > 1 {
			return "", Location{}, "ambiguous_session"
		}
		return candidates[0].id, candidates[0].location, ""
	}
	return "", Location{}, "invalid_session"
}

func kimiReason(code string) string {
	switch code {
	case "kimi_imported":
		return "Imported Kimi histories are not supported because their input origins cannot be verified."
	case "kimi_human_engine":
		return "Kimi human-engine-only histories are not supported."
	case "ambiguous_session":
		return "Kimi session is ambiguous. Multiple live panes share this directory without reported session IDs."
	case "unsupported_structure":
		return "This Kimi journal contains unsupported structural records."
	case "source_corrupt":
		return "Kimi history is incomplete or malformed; no transcript can be verified."
	case "output_limit":
		return "Kimi history exceeds the bounded replay or discovery limit."
	default:
		return "No verified Kimi conversation is available yet. Sessionless startup is normal."
	}
}

func (r *Reader) readKimi(ctx context.Context, project ProjectContext, session string) (kimiProjection, Location, string) {
	if session == "" {
		return kimiProjection{}, Location{}, "invalid_session"
	}
	_, location, code := r.SelectKimiSession(project, session)
	if code != "" {
		return kimiProjection{}, location, code
	}
	return r.readKimiLocation(ctx, location)
}

func (r *Reader) readKimiLocation(ctx context.Context, location Location) (kimiProjection, Location, string) {
	data, ok := readKimiFile(location, maxConversationBytes)
	if !ok {
		return kimiProjection{}, location, "output_limit"
	}
	projection := projectKimi(ctx, data)
	return projection, location, projection.code
}
