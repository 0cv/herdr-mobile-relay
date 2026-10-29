package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

type installedOMODependencies struct {
	roots func(context.Context, string) ([]string, error)
}

func productionInstalledOMODependencies() installedOMODependencies {
	return installedOMODependencies{roots: agentroots.OMOContext}
}
func (d installedOMODependencies) complete() bool { return d.roots != nil }

func (r *Reader) locateInstalledOMOIn(parent *conversationlife.Call, cwd, sessionID string) (location Location, identity omoIdentity, code string, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		ctx := call.Context()
		if sessionID == "" {
			code = "invalid_session"
			return nil
		}
		var roots []string
		if e := installedFileIn(r.installed.owner, call, r.installed.files, "omo-roots", func(_ *conversationlife.Call, ctx context.Context) (e error) {
			return sourceEffect(ctx, func() error { roots, e = r.installed.omo.roots(ctx, r.home); return e })
		}); e != nil {
			return e
		}
		d := installedFileDiscovery(r.installed.owner, call, r.installed.files)
		if filepath.IsAbs(sessionID) {
			for _, root := range roots {
				path, e := containedRegularFileContext(ctx, d, sessionID, root)
				if e != nil {
					return e
				}
				if path == "" {
					continue
				}
				id, ok := omoIdentityFromPath(path)
				if !ok || !omoPathMatchesCWD(path, cwd) {
					code = "invalid_session"
					return nil
				}
				location, identity = Location{Path: path, Root: root}, id
				return nil
			}
			code = "path_uncontained"
			return nil
		}
		if !safeOMOIdentity(sessionID) {
			code = "invalid_session"
			return nil
		}
		for _, root := range roots {
			var dirs []string
			if strings.TrimSpace(cwd) != "" {
				candidate := filepath.Join(root, encodeOMOCWD(cwd))
				directory, e := isDirContext(ctx, d, candidate)
				if e != nil {
					return e
				}
				if directory {
					dirs = append(dirs, candidate)
				}
			} else {
				entries, e := d.readDir(root)
				if terminalProviderError(e) {
					return e
				}
				for _, entry := range entries {
					if e := ctx.Err(); e != nil {
						return e
					}
					if !strings.HasPrefix(entry.Name(), "--") || !strings.HasSuffix(entry.Name(), "--") {
						continue
					}
					candidate := filepath.Join(root, entry.Name())
					directory, e := isDirContext(ctx, d, candidate)
					if e != nil {
						return e
					}
					if directory {
						dirs = append(dirs, candidate)
					}
				}
			}
			for _, dir := range dirs {
				if e := ctx.Err(); e != nil {
					return e
				}
				entries, e := d.readDir(dir)
				if terminalProviderError(e) {
					return e
				}
				for _, entry := range entries {
					if e := ctx.Err(); e != nil {
						return e
					}
					parts := omoFilename.FindStringSubmatch(entry.Name())
					if len(parts) != 2 || parts[1] != sessionID {
						continue
					}
					path, e := containedRegularFileContext(ctx, d, filepath.Join(dir, entry.Name()), root)
					if e != nil {
						return e
					}
					if path == "" {
						continue
					}
					if location.Path != "" && location.Path != path {
						code = "invalid_session"
						return nil
					}
					location = Location{Path: path, Root: root}
				}
			}
		}
		if location.Path == "" {
			code = "invalid_session"
			return nil
		}
		identity = omoIdentity{ID: sessionID, CWD: cwd}
		return nil
	})
	if err != nil {
		return Location{}, omoIdentity{}, "", err
	}
	return location, identity, code, nil
}

func copyOMOEntries(entries []Entry) []Entry {
	result := make([]Entry, len(entries))
	for i, entry := range entries {
		result[i] = entry
		result[i].Tools = append([]ToolActivity(nil), entry.Tools...)
	}
	return result
}
func copyOMOPlan(plan OMOTodoState) OMOTodoState {
	result := plan
	result.Phases = make([]OMOTodoPhase, len(plan.Phases))
	for i, phase := range plan.Phases {
		result.Phases[i] = phase
		result.Phases[i].Tasks = append([]OMOTodoTask(nil), phase.Tasks...)
	}
	return result
}

func (r *Reader) loadInstalledOMOIn(parent *conversationlife.Call, location Location, identity omoIdentity, cwd string) (entries []Entry, plan OMOTodoState, clipped bool, code string, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		ctx := call.Context()
		key := location.Path + "\x00" + identity.ID + "\x00" + cwd
		published := false
		var proposed *omoCacheEntry
		defer func() {
			cancelled := ctx.Err() != nil
			if published && cancelled {
				r.mu.Lock()
				if cached, ok := r.omoCache[key]; ok && cached.identity == proposed.identity && cached.modTime == proposed.modTime {
					delete(r.omoCache, key)
				}
				r.mu.Unlock()
			}
		}()
		matched, e := withInstalledOMOSourceIn(r.installed.owner, call, location, identity, cwd, r.installed.files, func(sourceCall *conversationlife.Call, sourceCtx context.Context, source fileSource) error {
			r.mu.Lock()
			cached, ok := r.omoCache[key]
			r.mu.Unlock()
			stamp := fileIdentity(source.info)
			if ok && cached.size == source.end && cached.modTime == source.info.ModTime().UnixNano() && cached.identity == stamp {
				if e := sourceCtx.Err(); e != nil {
					return e
				}
				entries, plan, clipped = copyOMOEntries(cached.entries), copyOMOPlan(cached.plan), cached.clipped
				return sourceCtx.Err()
			}
			var e error
			entries, plan, clipped, e = loadInstalledOMOSourceIn(r.installed.owner, sourceCall, source, identity, r.installed.files)
			if e != nil {
				return e
			}
			var current os.FileInfo
			e = sourceEffect(sourceCtx, func() (err error) { current, err = r.installed.files.source.stat(source.path); return })
			if terminalProviderError(e) {
				return e
			}
			if e == nil && current != nil && r.installed.files.source.same(source.info, current) && current.Size() == source.end && current.ModTime() == source.info.ModTime() {
				proposed = &omoCacheEntry{size: source.end, modTime: source.info.ModTime().UnixNano(), identity: stamp, entries: copyOMOEntries(entries), plan: copyOMOPlan(plan), clipped: clipped}
			}
			return sourceCtx.Err()
		})
		if e != nil {
			return e
		}
		if !matched {
			code = "invalid_session"
			return nil
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if proposed != nil {
			done := ctx.Done()
			r.mu.Lock()
			select {
			case <-done:
			default:
				if len(r.omoCache) >= maxOMOCacheEntries {
					clear(r.omoCache)
				}
				r.omoCache[key] = *proposed
				published = true
			}
			r.mu.Unlock()
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, OMOTodoState{}, false, "", err
	}
	return entries, plan, clipped, code, nil
}

func (r *Reader) readInstalledOMOIn(parent *conversationlife.Call, cwd, sessionID, before string, limit int, page *Page) error {
	location, identity, code, err := r.locateInstalledOMOIn(parent, cwd, sessionID)
	if err != nil {
		return err
	}
	if code != "" {
		*page = unavailableCode(code, "OMO conversation history is unavailable.")
		return nil
	}
	entries, plan, clipped, code, err := r.loadInstalledOMOIn(parent, location, identity, cwd)
	if err != nil {
		return err
	}
	if code != "" {
		*page = unavailableCode(code, "OMO conversation history is unavailable.")
		return nil
	}
	if before != "" {
		found := false
		for i := range entries {
			if err := parent.Context().Err(); err != nil {
				return err
			}
			if entries[i].ID == before {
				found = true
				break
			}
		}
		if !found {
			*page = unavailableCode("invalid_cursor", "The conversation cursor is invalid.")
			return nil
		}
	}
	*page = installedPage(entries, before, limit, clipped)
	page.SourceCorrupt = plan.ReasonCode == "source_corrupt"
	page.OMOPlan = &plan
	return parent.Context().Err()
}

func (r *Reader) ReadOMOTodoStateContext(ctx context.Context, cwd, sessionID string) (state OMOTodoState, err error) {
	err = r.installedRoot(ctx, func(call *conversationlife.Call) error {
		var e error
		state, e = r.readInstalledOMOTodoIn(call, cwd, sessionID)
		return e
	})
	if err != nil {
		return OMOTodoState{}, err
	}
	return state, nil
}
func (r *Reader) ReadOMOTodoStateIn(parent *conversationlife.Call, cwd, sessionID string) (state OMOTodoState, err error) {
	err = r.installedIn(parent, func(call *conversationlife.Call) error {
		var e error
		state, e = r.readInstalledOMOTodoIn(call, cwd, sessionID)
		return e
	})
	if err != nil {
		return OMOTodoState{}, err
	}
	return state, nil
}
func (r *Reader) readInstalledOMOTodoIn(parent *conversationlife.Call, cwd, sessionID string) (OMOTodoState, error) {
	location, identity, code, err := r.locateInstalledOMOIn(parent, cwd, strings.TrimSpace(sessionID))
	if err != nil {
		return OMOTodoState{}, err
	}
	if code != "" {
		return OMOTodoState{ReasonCode: code, Phases: []OMOTodoPhase{}}, nil
	}
	_, plan, _, code, err := r.loadInstalledOMOIn(parent, location, identity, cwd)
	if err != nil {
		return OMOTodoState{}, err
	}
	if code != "" {
		return OMOTodoState{ReasonCode: code, Phases: []OMOTodoPhase{}}, nil
	}
	return plan, nil
}
