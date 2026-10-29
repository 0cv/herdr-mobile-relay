package conversation

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// A Browser source is a shared close slot, never a native descriptor. A request
// and a queued job bind separate contexts to the same handle; neither can read
// after its own scope is cancelled or after the first physical close.
type browserOwnedFile struct {
	owner  *conversationlife.Owner
	ctx    context.Context
	source *sourceOwnership
}

func (f *browserOwnedFile) ReadAt(p []byte, offset int64) (n int, err error) {
	err = f.owner.Do(f.ctx, func(call *conversationlife.Call) error {
		return (fileSource{owned: f.source}).borrowIn(f.owner, call, func(reader io.ReaderAt) error {
			var readErr error
			n, readErr = reader.ReadAt(p, offset)
			return readErr
		})
	})
	return n, err
}

func (f *browserOwnedFile) Stat() (info os.FileInfo, err error) {
	err = f.owner.Do(f.ctx, func(call *conversationlife.Call) error {
		f.source.mu.Lock()
		defer f.source.mu.Unlock()
		if f.source.closed || nilSourceHandle(f.source.handle) {
			return conversationlife.ErrRefused
		}
		return sourceEffect(call.Context(), func() (e error) { info, e = f.source.handle.Stat(); return })
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (f *browserOwnedFile) Close() error { return f.source.close() }

func (b *Browser) doBrowserBackgroundContext(fn func(context.Context) error) error {
	if b.installed == nil {
		return fn(b.ctx)
	}
	return b.installed.Do(b.ctx, func(call *conversationlife.Call) error {
		if err := call.Context().Err(); err != nil {
			return err
		}
		return errors.Join(fn(call.Context()), call.Context().Err())
	})
}

func (b *Browser) bindBrowserSource(source fileSource, ctx context.Context) fileSource {
	if source.owned != nil {
		source.file = &browserOwnedFile{owner: b.installed, ctx: ctx, source: source.owned}
	}
	return source
}

// Bind only this Browser's acquired sources to its close result. The shared
// owner's witness ledger also contains other consumers' resources and cannot
// be used as the Browser's shutdown result.
func (b *Browser) browserSourceDeps() installedSourceDeps {
	deps := b.reader.installed.files.source
	if !deps.complete() {
		return deps
	}
	deps.onClose = func(fault error) {
		b.noteSourceCleanup(errors.Join(ErrSourceCleanupUncertain, fault))
	}
	return deps
}

func (b *Browser) captureBrowserSource(ctx context.Context, location Location, reserve, observe func(int64) error) (source fileSource, err error) {
	if b.installed == nil {
		return captureFileSourceWithHooks(location, reserve, observe)
	}
	deps := b.browserSourceDeps()
	if !deps.complete() {
		return fileSource{}, conversationlife.ErrRefused
	}
	err = b.installed.Do(ctx, func(call *conversationlife.Call) (e error) {
		source, e = deps.captureIn(b.installed, call, location, reserve, observe)
		if e == nil {
			if cancelled := call.Context().Err(); cancelled != nil {
				e = sourceResult(cancelled, source.close())
			}
		}
		return e
	})
	if err != nil {
		return fileSource{}, err
	}
	return b.bindBrowserSource(source, ctx), nil
}

// Metadata-only reopen uses the same owned acquisition and first-close slot.
func (b *Browser) openBrowserSource(ctx context.Context, location Location) (source fileSource, err error) {
	if b.installed == nil {
		file, info, e := openContainedConversationFile(location)
		if e != nil {
			return fileSource{}, e
		}
		return fileSource{location: location, path: location.Path, file: file, info: info, end: info.Size(), ordinary: &ordinarySourceClose{file: file}}, nil
	}
	deps := b.browserSourceDeps()
	if !deps.complete() {
		return fileSource{}, conversationlife.ErrRefused
	}
	err = b.installed.Do(ctx, func(call *conversationlife.Call) (e error) {
		source, e = deps.openIn(b.installed, call, location)
		if e == nil {
			if cancelled := call.Context().Err(); cancelled != nil {
				e = sourceResult(cancelled, source.close())
			}
		}
		return e
	})
	if err != nil {
		return fileSource{}, err
	}
	return b.bindBrowserSource(source, ctx), nil
}

func (b *Browser) browserSourcePath(ctx context.Context, source fileSource) error {
	if b.installed == nil {
		resolved, err := os.Stat(source.path)
		if err != nil || !resolved.Mode().IsRegular() || !os.SameFile(source.info, resolved) || containedRegularFile(source.path, source.location.Root) != source.path {
			return errors.New("conversation source changed")
		}
		return nil
	}
	deps := b.reader.installed.files.source
	return b.installed.Do(ctx, func(call *conversationlife.Call) error {
		resolved, err := deps.contained(call.Context(), source.path, source.location.Root)
		if err != nil {
			return err
		}
		if resolved != source.path {
			return errors.New("conversation source changed")
		}
		var info os.FileInfo
		if err := sourceEffect(call.Context(), func() (e error) { info, e = deps.stat(source.path); return }); err != nil {
			return err
		}
		if info == nil || !info.Mode().IsRegular() || !deps.same(source.info, info) {
			return errors.New("conversation source changed")
		}
		return nil
	})
}

func (b *Browser) locateBrowserClaude(ctx context.Context, scope BrowseScope) (location Location, err error) {
	if b.installed == nil {
		return b.reader.LocateWithProject(scope.Provider, ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, scope.SessionID), nil
	}
	err = b.installed.Do(ctx, func(call *conversationlife.Call) (e error) {
		location, e = b.reader.LocateWithProjectIn(call, scope.Provider, ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, scope.SessionID)
		return errors.Join(e, call.Context().Err())
	})
	return location, err
}

func (b *Browser) resolveBrowserClaude(ctx context.Context, anchor Location, sessionID string, reserve, observe func(int64) error) (chain claudeChain, err error) {
	if b.installed == nil {
		return resolveClaudeChainWithReadHooks(ctx, anchor, sessionID, reserve, observe)
	}
	err = b.installed.Do(ctx, func(call *conversationlife.Call) (e error) {
		deps := b.reader.installed.files
		deps.source = b.browserSourceDeps()
		chain, e = resolveInstalledClaudeChainIn(b.installed, call, anchor, sessionID, reserve, observe, deps)
		return errors.Join(e, call.Context().Err())
	})
	if err != nil {
		return claudeChain{}, err
	}
	return chain, nil
}

func (b *Browser) sourceForContext(ctx context.Context, scope BrowseScope, omo bool) (fileSource, string, error) {
	if b.installed == nil {
		source, code := b.ordinarySourceFor(scope, omo)
		return source, code, nil
	}
	if b.reader == nil || b.reader.installed == nil || !b.reader.installed.files.complete() || !b.reader.installed.omo.complete() {
		return fileSource{}, "", conversationlife.ErrRefused
	}
	scope = normalizeBrowseScope(scope)
	var location Location
	var locateCode string
	var identity *omoIdentity
	err := b.installed.Do(ctx, func(call *conversationlife.Call) (e error) {
		if omo {
			var found omoIdentity
			location, found, locateCode, e = b.reader.locateInstalledOMOIn(call, scope.CWD, scope.SessionID)
			identity = &found
		} else {
			location, e = b.reader.LocateWithProjectIn(call, scope.Provider, ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, scope.SessionID)
		}
		return errors.Join(e, call.Context().Err())
	})
	if err != nil {
		return fileSource{}, "", err
	}
	if location.Path == "" {
		if locateCode == "" {
			locateCode = "invalid_session"
		}
		return fileSource{}, locateCode, nil
	}
	source, err := b.captureBrowserSource(ctx, location, nil, nil)
	if err != nil {
		if terminalProviderError(err) || errors.Is(err, ErrSourceCleanupUncertain) {
			return fileSource{}, "", err
		}
		return fileSource{}, "source_unavailable", nil
	}
	if identity != nil {
		var matched bool
		err = b.installed.Do(ctx, func(call *conversationlife.Call) error {
			return source.borrowIn(b.installed, call, func(reader io.ReaderAt) (e error) {
				matched, e = verifyOMOIdentityReaderContext(call.Context(), io.NewSectionReader(reader, 0, source.end), *identity, scope.CWD)
				return e
			})
		})
		if err != nil || !matched {
			closeErr := source.close()
			if err != nil || closeErr != nil {
				return fileSource{}, "", sourceResult(err, closeErr)
			}
			return fileSource{}, "invalid_session", nil
		}
	}
	return source, "", nil
}
