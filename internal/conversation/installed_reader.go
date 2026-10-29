package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

type installedReaderBinding struct {
	owner *conversationlife.Owner
	files installedReaderFileDeps
	omo   installedOMODependencies
}

// NewInstalledReader binds both concrete SQL executors and the file providers to
// one service lifetime. No ordinary executor, environment-based SQL factory or
// background-context provider is consulted during construction.
func NewInstalledReader(ctx context.Context, home string, owner *conversationlife.Owner, openCode, hermes *sqliteexec.InstalledExecutor) (*Reader, error) {
	if owner == nil || openCode == nil || hermes == nil || !openCode.OwnedBy(owner) || !hermes.OwnedBy(owner) {
		return nil, conversationlife.ErrRefused
	}
	var result *Reader
	err := owner.Do(ctx, func(call *conversationlife.Call) error {
		oc, err := newInstalledOpenCodeReaderIn(owner, call, home, openCode)
		if err != nil {
			return err
		}
		h, err := newInstalledHermesReaderIn(owner, call, home, hermes)
		if err != nil {
			return err
		}
		err = constructInstalledReaderIn(call, home, oc, h, productionInstalledReaderFileDeps(), productionInstalledOMODependencies(), &result)
		return errors.Join(err, call.Context().Err())
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// This private binding also serves complete, fail-closed logical fixture plans.
func constructInstalledReaderIn(call *conversationlife.Call, home string, oc *openCodeReader, h *hermesReader, files installedReaderFileDeps, omo installedOMODependencies, result **Reader) error {
	if result == nil || oc == nil || h == nil || !oc.installedValid() || !h.installedValid() || !files.complete() || !omo.complete() || oc.installed.owner != h.installed.owner {
		return conversationlife.ErrRefused
	}
	owner := oc.installed.owner
	return owner.DoIn(call, func(c *conversationlife.Call) error {
		if err := c.Context().Err(); err != nil {
			return err
		}
		candidate := &Reader{home: home, locations: make(map[locationKey]locationCacheEntry), locating: make(map[locationKey]chan struct{}), omoCache: make(map[string]omoCacheEntry), openCode: oc, hermes: h, installed: &installedReaderBinding{owner: owner, files: files, omo: omo}}
		if err := c.Context().Err(); err != nil {
			return err
		}
		*result = candidate
		return nil
	})
}

// InstalledOwner identifies the immutable construction association; it is not
// an admission token. All operations must still enter the owner.
func (r *Reader) InstalledOwner() *conversationlife.Owner {
	if r == nil || r.installed == nil {
		return nil
	}
	return r.installed.owner
}

func (r *Reader) installedRoot(ctx context.Context, fn func(*conversationlife.Call) error) error {
	if r == nil || r.installed == nil || r.installed.owner == nil || !r.installed.files.complete() || !r.installed.omo.complete() || r.openCode == nil || r.hermes == nil || !r.openCode.installedValid() || !r.hermes.installedValid() || r.openCode.installed.owner != r.installed.owner || r.hermes.installed.owner != r.installed.owner {
		return conversationlife.ErrRefused
	}
	return r.installed.owner.Do(ctx, func(call *conversationlife.Call) error { return errors.Join(fn(call), call.Context().Err()) })
}

func (r *Reader) installedIn(parent *conversationlife.Call, fn func(*conversationlife.Call) error) error {
	if r == nil || r.installed == nil || r.installed.owner == nil {
		return conversationlife.ErrRefused
	}
	return r.installed.owner.DoIn(parent, func(call *conversationlife.Call) error {
		if !r.installed.files.complete() || !r.installed.omo.complete() || !r.openCode.installedValid() || !r.hermes.installedValid() || r.openCode.installed.owner != r.installed.owner || r.hermes.installed.owner != r.installed.owner {
			return conversationlife.ErrRefused
		}
		if err := call.Context().Err(); err != nil {
			return err
		}
		return errors.Join(fn(call), call.Context().Err())
	})
}

func (r *Reader) LocateWithProjectContext(ctx context.Context, agent string, project ProjectContext, sessionID string) (location Location, err error) {
	err = r.installedRoot(ctx, func(call *conversationlife.Call) error {
		var e error
		location, e = r.locateInstalledIn(call, agent, project, sessionID)
		return e
	})
	if err != nil {
		return Location{}, err
	}
	return location, nil
}

func (r *Reader) LocateWithProjectIn(parent *conversationlife.Call, agent string, project ProjectContext, sessionID string) (location Location, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		var e error
		location, e = r.locateInstalledIn(call, agent, project, sessionID)
		return e
	})
	if err != nil {
		return Location{}, err
	}
	return location, nil
}

// The leader remains within its own admitted call. A waiter cancellation does
// not cancel it; an aborted leader never caches a miss and always retires its
// flight, including panic and Goexit. No lookup or context callback holds mu.
func (r *Reader) locateInstalledIn(parent *conversationlife.Call, agent string, project ProjectContext, sessionID string) (Location, error) {
	var result Location
	err := r.installedIn(parent, func(call *conversationlife.Call) error {
		ctx := call.Context()
		project = NormalizeProjectContext(agent, project)
		sessionID = strings.TrimSpace(sessionID)
		key := locationKey{Agent: normalizedAgent(agent), CWD: project.CWD, ForegroundCWD: project.ForegroundCWD, SessionID: sessionID}
		if isHermesAgent(agent) {
			key.Agent = "hermes"
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.mu.Lock()
			cached, ok := r.locations[key]
			ready := r.locating[key]
			if ok && time.Now().Before(cached.expires) {
				r.mu.Unlock()
				if err := ctx.Err(); err != nil {
					return err
				}
				result = cached.location
				return nil
			}
			if ready == nil {
				ready = make(chan struct{})
				r.locating[key] = ready
				r.mu.Unlock()
				return r.leadInstalledLocation(call, key, agent, project, sessionID, ready, &result)
			}
			r.mu.Unlock()
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	if err != nil {
		return Location{}, err
	}
	return result, nil
}

func (r *Reader) leadInstalledLocation(call *conversationlife.Call, key locationKey, agent string, project ProjectContext, sessionID string, ready chan struct{}, result *Location) error {
	ctx := call.Context()
	published, completed := false, false
	var expiry time.Time
	defer func() {
		cancelled := ctx.Err() != nil
		r.mu.Lock()
		if published && (!completed || cancelled) {
			if cached, ok := r.locations[key]; ok && cached.expires == expiry {
				delete(r.locations, key)
			}
		}
		if r.locating[key] == ready {
			delete(r.locating, key)
			close(ready)
		}
		r.mu.Unlock()
	}()
	var location Location
	var err error
	switch {
	case isHermesAgent(agent):
		location, _, err = r.hermes.locateIn(call, project.CWD, sessionID)
	case installedFileKind(agent) != "":
		location, err = locateInstalledFileIn(r.installed.owner, call, r.home, agent, project, sessionID, r.installed.files)
	}
	if err = readerDiscoveryResult(ctx, err); err != nil {
		return err
	}
	ttl := locationCacheTTL
	if location.Path == "" {
		ttl = locationMissTTL
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	done := ctx.Done()
	r.mu.Lock()
	select {
	case <-done:
	default:
		if len(r.locations) >= maxLocationCacheEntries {
			for k, v := range r.locations {
				if !now.Before(v.expires) {
					delete(r.locations, k)
				}
			}
			if len(r.locations) >= maxLocationCacheEntries {
				clear(r.locations)
			}
		}
		expiry = now.Add(ttl)
		r.locations[key] = locationCacheEntry{location: location, expires: expiry}
		published = true
	}
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	*result = location
	completed = true
	return nil
}

func (r *Reader) ReadWithProjectContext(ctx context.Context, agent string, project ProjectContext, sessionID, before string, limit int) (page Page, err error) {
	err = r.installedRoot(ctx, func(call *conversationlife.Call) error {
		var e error
		page, e = r.readInstalledIn(call, agent, project, sessionID, before, limit)
		return e
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

func (r *Reader) ReadWithProjectIn(parent *conversationlife.Call, agent string, project ProjectContext, sessionID, before string, limit int) (page Page, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		var e error
		page, e = r.readInstalledIn(call, agent, project, sessionID, before, limit)
		return e
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

func installedPage(entries []Entry, before string, limit int, clipped bool) Page {
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	end := len(entries)
	if before != "" {
		for i := range entries {
			if entries[i].ID == before {
				end = i
				break
			}
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return Page{Available: true, Entries: append([]Entry(nil), entries[start:end]...), HasMore: start > 0, Total: len(entries), FileTruncated: clipped}
}

func (r *Reader) readInstalledIn(parent *conversationlife.Call, agent string, project ProjectContext, sessionID, before string, limit int) (page Page, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		ctx := call.Context()
		project = NormalizeProjectContext(agent, project)
		sessionID = strings.TrimSpace(sessionID)
		if limit < 1 {
			limit = defaultPageSize
		}
		if limit > maxPageSize {
			limit = maxPageSize
		}
		switch normalizedAgent(agent) {
		case "opencode":
			entries, more, corrupt, metadata, code, e := r.openCode.readIn(call, sessionID, before, limit)
			if e != nil {
				return e
			}
			if code != "" {
				page = unavailableCode(code, "OpenCode conversation history is unavailable.")
				return nil
			}
			if _, e = normalizeEntriesForResponseContext(ctx, entries); e != nil {
				return e
			}
			page = Page{Available: true, Entries: entries, HasMore: more, Total: metadata.Total, SourceCorrupt: corrupt}
			return nil
		case "omo", "ohmyopencode":
			return r.readInstalledOMOIn(call, project.CWD, sessionID, before, limit, &page)
		}
		if !Supported(agent) {
			page = unavailableCode("invalid_provider", "Conversation history is not available for this agent.")
			return nil
		}
		if sessionID == "" {
			page = unavailableCode("invalid_session", "This agent has not reported a conversation session yet.")
			return nil
		}
		if isHermesAgent(agent) && before != "" {
			cursor, e := strconv.ParseInt(before, 10, 64)
			if e != nil || cursor <= 0 {
				page = unavailableCode("invalid_cursor", "This conversation page cursor is invalid.")
				return nil
			}
		}
		location, e := r.locateInstalledIn(call, agent, project, sessionID)
		if e != nil {
			return e
		}
		if location.Path == "" {
			page = unavailableCode("invalid_session", "No conversation log is available for this session.")
			return nil
		}
		if isHermesAgent(agent) {
			entries, more, corrupt, metadata, code, e := r.hermes.readFromIn(call, location.Path, sessionID, before, limit)
			if e != nil {
				return e
			}
			if code != "" {
				page = unavailableCode(code, "Hermes conversation history is unavailable.")
				return nil
			}
			if project.CWD != "" && metadata.CWD != "" {
				same, e := r.hermes.sameDirectoryIn(call, project.CWD, metadata.CWD)
				if e != nil {
					return e
				}
				if !same {
					page = unavailableCode("invalid_session", "This conversation belongs to a different workspace.")
					return nil
				}
			}
			if _, e = normalizeEntriesForResponseContext(ctx, entries); e != nil {
				return e
			}
			page = Page{Available: true, Entries: entries, HasMore: more, Total: metadata.Total, SourceCorrupt: corrupt}
			return nil
		}
		if isClaudeProvider(agent) {
			page, e = readInstalledClaudeChainIn(r.installed.owner, call, sessionID, location, before, limit, r.installed.files)
			return e
		}
		entries, clipped, e := readInstalledTranscriptIn(r.installed.owner, call, location, agent, r.installed.files)
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		page = installedPage(entries, before, limit, clipped)
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// InstalledSourceRevisionIn captures and closes the selected file under the
// same owner. A later read must recheck it before publishing a browser page.
func (r *Reader) InstalledSourceRevisionIn(parent *conversationlife.Call, location Location) (revision string, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) (e error) {
		source, e := r.installed.files.source.captureIn(r.installed.owner, call, location, nil, nil)
		if e != nil {
			return e
		}
		defer func() { e = readerDiscoveryResult(call.Context(), sourceResult(e, source.close())) }()
		revision = source.revision
		return call.Context().Err()
	})
	if err != nil {
		return "", err
	}
	return revision, nil
}

// Session-title consumers can read exact selected bytes without opening an
// unowned file or re-resolving another copy. Codex index remains beside Root.
func (r *Reader) ReadSelectedContentIn(parent *conversationlife.Call, location Location, index bool) (data []byte, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		if index {
			location = Location{Path: filepath.Join(filepath.Dir(location.Root), "session_index.jsonl"), Root: filepath.Dir(location.Root)}
		}
		var e error
		data, _, e = r.installed.files.source.readContentIn(r.installed.owner, call, location, maxConversationBytes, false)
		return e
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}
