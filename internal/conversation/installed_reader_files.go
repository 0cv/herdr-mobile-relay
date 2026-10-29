package conversation

import (
	"context"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// A copied, explicit plan; incomplete plans never borrow production defaults.
type installedReaderFileDeps struct {
	source  installedSourceDeps
	readDir func(string) ([]os.DirEntry, error)
	roots   func(context.Context, string, string) ([]string, error)
	observe func(context.Context, string)
}

func (d installedReaderFileDeps) complete() bool {
	return d.source.complete() && d.readDir != nil && d.roots != nil
}

func productionInstalledReaderFileDeps() installedReaderFileDeps {
	return installedReaderFileDeps{source: productionSourceDeps(), readDir: os.ReadDir, roots: installedFileRoots}
}

func installedFileRoots(ctx context.Context, home, kind string) ([]string, error) {
	switch kind {
	case "claude":
		return agentroots.ClaudeContext(ctx, home)
	case "qoder":
		return agentroots.QoderContext(ctx, home)
	case "codex":
		return agentroots.CodexContext(ctx, home)
	case "pi":
		return agentroots.PiContext(ctx, home)
	case "omp":
		return agentroots.OMPContext(ctx, home)
	case "cursor":
		return agentroots.CursorContext(ctx, home)
	default:
		return nil, conversationlife.ErrRefused
	}
}

func installedFileKind(agent string) string {
	switch normalizedAgent(agent) {
	case "claude", "claudecode":
		return "claude"
	case "qoder", "qodercli":
		return "qoder"
	case "codex", "openaicodex":
		return "codex"
	case "pi", "picodingagent":
		return "pi"
	case "omp", "ohmypi":
		return "omp"
	case "cursor", "cursoragent":
		return "cursor"
	default:
		return ""
	}
}

// Observations see the raw counted context, not this checkpoint wrapper.
// Neither the wrapper nor the observer carries Call authority.
type installedFileContext struct {
	context.Context
	observe func(context.Context, string)
	stage   string
}

func (c installedFileContext) Err() error {
	c.observe(c.Context, c.stage)
	return c.Context.Err()
}

func installedFileBody(call *conversationlife.Call, d installedReaderFileDeps, stage string, fn func(*conversationlife.Call, context.Context) error) (err error) {
	ctx := call.Context()
	if d.observe != nil {
		ctx = installedFileContext{Context: ctx, observe: d.observe, stage: stage}
	}
	defer func() { err = readerDiscoveryResult(ctx, err) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn(call, ctx)
}

func installedFileIn(owner *conversationlife.Owner, parent *conversationlife.Call, d installedReaderFileDeps, stage string, fn func(*conversationlife.Call, context.Context) error) error {
	if fn == nil || !d.complete() {
		return conversationlife.ErrRefused
	}
	return owner.DoIn(parent, func(call *conversationlife.Call) error {
		return installedFileBody(call, d, stage, fn)
	})
}

// Every retained callback binds this exact parent; even a fresh supplied context
// cannot resurrect it. Each effect has its own independently counted child.
func installedFileDiscovery(owner *conversationlife.Owner, parent *conversationlife.Call, d installedReaderFileDeps) readerDiscoveryDeps {
	return readerDiscoveryDeps{
		stat: func(path string) (info os.FileInfo, err error) {
			err = installedFileIn(owner, parent, d, "stat", func(_ *conversationlife.Call, ctx context.Context) error {
				return sourceEffect(ctx, func() (e error) { info, e = d.source.stat(path); return })
			})
			if err != nil {
				return nil, err
			}
			return info, nil
		},
		eval: func(path string) (resolved string, err error) {
			err = installedFileIn(owner, parent, d, "eval", func(_ *conversationlife.Call, ctx context.Context) error {
				return sourceEffect(ctx, func() (e error) { resolved, e = d.source.eval(path); return })
			})
			if err != nil {
				return "", err
			}
			return resolved, nil
		},
		readDir: func(path string) (entries []os.DirEntry, err error) {
			err = installedFileIn(owner, parent, d, "read-dir", func(_ *conversationlife.Call, ctx context.Context) error {
				return sourceEffect(ctx, func() (e error) { entries, e = d.readDir(path); return })
			})
			if err != nil {
				return nil, err
			}
			return entries, nil
		},
		readContent: func(ctx context.Context, location Location) (data []byte, err error) {
			if !d.complete() {
				return nil, conversationlife.ErrRefused
			}
			err = owner.DoInContext(ctx, parent, func(call *conversationlife.Call) error {
				return installedFileBody(call, d, "content", func(call *conversationlife.Call, _ context.Context) (e error) {
					data, _, e = d.source.readContentIn(owner, call, location, int64(^uint64(0)>>1), false)
					return e
				})
			})
			if err != nil {
				return nil, err
			}
			return data, nil
		},
	}
}

func locateInstalledFileIn(owner *conversationlife.Owner, parent *conversationlife.Call, home, agent string, project ProjectContext, sessionID string, deps installedReaderFileDeps) (location Location, err error) {
	err = installedFileIn(owner, parent, deps, "locate", func(call *conversationlife.Call, ctx context.Context) (e error) {
		kind := installedFileKind(agent)
		if e = ctx.Err(); e != nil {
			return e
		}
		if kind == "" {
			return conversationlife.ErrRefused
		}
		project = NormalizeProjectContext(agent, project)
		if e = ctx.Err(); e != nil {
			return e
		}
		// Private locator semantics: do not trim sessionID. These three families
		// validate before roots; Cursor and Pi/OMP validate in their shared cores.
		valid := true
		switch kind {
		case "claude", "qoder":
			valid = safeSessionID(sessionID)
		case "codex":
			valid = canonicalSessionID.MatchString(sessionID)
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if !valid {
			return nil
		}
		var roots []string
		e = installedFileIn(owner, call, deps, "roots", func(_ *conversationlife.Call, rootCtx context.Context) error {
			return sourceEffect(rootCtx, func() (rootErr error) { roots, rootErr = deps.roots(rootCtx, home, kind); return })
		})
		if e != nil {
			return e
		}
		discovery := installedFileDiscovery(owner, call, deps)
		switch kind {
		case "claude":
			location, e = findProjectSessionWithProjectContext(ctx, discovery, roots, project, sessionID+".jsonl", func(cwd string) string {
				return claudeProjectNonAlphanumeric.ReplaceAllString(cwd, "-")
			})
		case "qoder":
			location, e = findProjectSessionWithProjectContext(ctx, discovery, roots, project, sessionID+".jsonl", func(string) string { return "" })
		case "codex":
			location, e = findCodexSessionContext(ctx, discovery, roots, sessionID)
		case "pi", "omp":
			location, e = resolvePathOrSessionContext(ctx, discovery, roots, sessionID, "_")
		case "cursor":
			location, e = findCursorSessionContext(ctx, discovery, roots, project, sessionID)
		}
		return e
	})
	if err != nil {
		return Location{}, err
	}
	return location, nil
}

func readInstalledContentIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location, deps installedReaderFileDeps) (data []byte, err error) {
	err = installedFileIn(owner, parent, deps, "content", func(call *conversationlife.Call, _ context.Context) (e error) {
		data, _, e = deps.source.readContentIn(owner, call, location, int64(^uint64(0)>>1), false)
		return e
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Selected single transcript only: no Claude continuation, OMO plan or paging.
func readInstalledTranscriptIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location, agent string, deps installedReaderFileDeps) (entries []Entry, clipped bool, err error) {
	err = installedFileIn(owner, parent, deps, "transcript", func(call *conversationlife.Call, ctx context.Context) (e error) {
		kind := installedFileKind(agent)
		if e = ctx.Err(); e != nil {
			return e
		}
		if kind == "" {
			return conversationlife.ErrRefused
		}
		var data []byte
		data, clipped, e = deps.source.readContentIn(owner, call, location, maxConversationBytes, true)
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		// The source has physically closed, but this operation remains live
		// through conversion, parsing and deferred final error reconciliation.
		entries, e = parseTranscriptContext(ctx, agent, string(data))
		return e
	})
	if err != nil {
		return nil, false, err
	}
	return entries, clipped, nil
}
