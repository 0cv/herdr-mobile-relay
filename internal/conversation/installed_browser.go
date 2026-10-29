package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// NewInstalledBrowser uses the same recent, chain and snapshot pipeline as an
// ordinary Browser. cacheRoot is the configured cache parent, not a namespace
// supplied by a conversation source or a cursor.
func NewInstalledBrowser(ctx context.Context, reader *Reader, owner *conversationlife.Owner, cacheRoot string, options BrowserOptions) (*Browser, error) {
	return newInstalledBrowser(ctx, reader, owner, options, newBrowseSigningKey, cacheRoot)
}

// Actual installed construction supplies the configured root and gets a new namespace.
func newInstalledBrowser(ctx context.Context, reader *Reader, owner *conversationlife.Owner, options BrowserOptions, keyFn func() ([32]byte, error), roots ...string) (*Browser, error) {
	if owner == nil || reader == nil || reader.InstalledOwner() != owner || reader.installed == nil || !reader.installed.files.complete() || !reader.installed.omo.complete() || keyFn == nil || ctx == nil || len(roots) != 1 || !filepath.IsAbs(roots[0]) {
		return nil, conversationlife.ErrRefused
	}
	var result *Browser
	err := owner.Do(ctx, func(call *conversationlife.Call) error {
		if err := call.Context().Err(); err != nil {
			return err
		}
		key, err := keyFn()
		if err != nil {
			return err
		}
		options = options.normalized()
		parent := roots[0]
		// Only create a fresh namespace; never adopt, chmod, enumerate or
		// delete any other Browser's files in the configured cache parent.
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("conversation history cache parent is unavailable")
		}
		root, err := os.MkdirTemp(parent, "browser-")
		if err != nil {
			return err
		}
		if err := call.Context().Err(); err != nil {
			_ = os.Remove(root)
			return err
		}
		lifetime, cancel := context.WithCancel(context.Background())
		result = &Browser{reader: reader, installed: owner, cacheRoot: root, options: options, key: key,
			ctx: lifetime, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan *browseJob, options.ActiveWorkers+1), closed: make(chan struct{}),
			jobs: make(map[string]*browseJob), dedup: make(map[string]string), recentCache: newRecentProjectionCache(options),
			chains: make(map[string]*claudeChainContext), chainByKey: make(map[string]string),
			chainLineage: make(map[string]claudeChainLineage), chainLineageIdentity: make(map[string]claudeChainLineageIdentity),
			chainValidations: make(map[string]*claudeChainValidation), chainCompactions: make(map[string]*claudeChainEvidenceCompaction)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = result.Close()
		return nil, err
	}
	result.schedulerWG.Add(1)
	go result.scheduler()
	if err := ctx.Err(); err != nil {
		_ = result.Close()
		return nil, err
	}
	return result, nil
}

func (b *Browser) readInstalledPage(ctx context.Context, request BrowseRequest) (result BrowsePage, err error) {
	if b == nil || b.reader == nil || b.reader.InstalledOwner() != b.installed || ctx == nil {
		return BrowsePage{}, conversationlife.ErrRefused
	}
	err = b.installed.Do(ctx, func(parent *conversationlife.Call) error {
		b.mu.Lock()
		if b.closing {
			b.mu.Unlock()
			return conversationlife.ErrRefused
		}
		b.readWG.Add(1)
		b.mu.Unlock()
		defer b.readWG.Done()
		callCtx, cancel := context.WithCancel(parent.Context())
		bridgeDone := make(chan struct{})
		stop := context.AfterFunc(b.ctx, func() { defer close(bridgeDone); cancel() })
		defer func() {
			cancel()
			if stop() {
				close(bridgeDone)
			} else {
				<-bridgeDone
			}
		}()
		return b.installed.DoInContext(callCtx, parent, func(call *conversationlife.Call) error {
			var e error
			result, e = b.readPage(call.Context(), request)
			e = errors.Join(e, call.Context().Err())
			if e != nil {
				result = BrowsePage{}
				if errors.Is(e, ErrSourceCleanupUncertain) {
					// Close joins workers and retires the namespace afterward.
					b.poisonSourceCleanup(e)
				}
			}
			return e
		})
	})
	if err != nil {
		return BrowsePage{}, errors.Join(err, ctx.Err())
	}
	return result, nil
}

// Native revision metadata is acquired through the same installed dependency
// under the request's live Call; nativeSourceIdentityRevision's ambient stat
// remains exclusively in the ordinary Browser path.
func installedNativeRevision(call *conversationlife.Call, path string, deps installedProviderMetadata) (string, error) {
	if path == "" || !deps.complete() {
		return "", conversationlife.ErrRefused
	}
	var info os.FileInfo
	err := sourceEffect(call.Context(), func() (e error) { info, e = deps.stat(path); return e })
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", conversationlife.ErrRefused
	}
	return nativeSourceRevisionForIdentity(path, fileIdentity(info)), nil
}

// Only acquisition differs for native providers; both modes use nativePage's
// cursor, limit, response budget, revision and diagnostic projection.
func (b *Browser) readInstalledOpenCodeNative(call *conversationlife.Call, request BrowseRequest, cursor browseCursor) (BrowsePage, error) {
	entries, more, corrupt, metadata, code, err := b.reader.openCode.readIn(call, request.Scope.SessionID, cursor.NativeBefore, request.Limit)
	if err != nil {
		return BrowsePage{}, err
	}
	if code != "" {
		return nativeFailure(code, "OpenCode conversation history is unavailable."), nil
	}
	revision, err := installedNativeRevision(call, metadata.Database, b.reader.openCode.metadata)
	if err != nil {
		return BrowsePage{}, err
	}
	if cursor.Revision != "" && cursor.Revision != revision {
		return nativeFailure("source_changed", "The OpenCode database changed while history was being browsed."), nil
	}
	return b.nativePage(request.Scope, entries, more, corrupt, metadata.Total, metadata.MessageID, revision, cursor), nil
}

func (b *Browser) readInstalledHermesNative(call *conversationlife.Call, request BrowseRequest, cursor browseCursor) (BrowsePage, error) {
	location, err := b.reader.LocateWithProjectIn(call, request.Scope.Provider, ProjectContext{CWD: request.Scope.CWD}, request.Scope.SessionID)
	if err != nil {
		return BrowsePage{}, err
	}
	if location.Path == "" {
		return browseUnavailable("invalid_session", "Hermes conversation history is unavailable."), nil
	}
	entries, more, corrupt, metadata, code, err := b.reader.hermes.readFromIn(call, location.Path, request.Scope.SessionID, cursor.NativeBefore, request.Limit)
	if err != nil {
		return BrowsePage{}, err
	}
	if code != "" {
		return nativeFailure(code, "Hermes conversation history is unavailable."), nil
	}
	if request.Scope.CWD != "" && metadata.CWD != "" {
		same, err := b.reader.hermes.sameDirectoryIn(call, request.Scope.CWD, metadata.CWD)
		if err != nil {
			return BrowsePage{}, err
		}
		if !same {
			return browseUnavailable("invalid_session", "This conversation belongs to a different workspace."), nil
		}
	}
	revision, err := installedNativeRevision(call, location.Path, b.reader.hermes.metadata)
	if err != nil {
		return BrowsePage{}, err
	}
	if cursor.Revision != "" && cursor.Revision != revision {
		return nativeFailure("source_changed", "The Hermes database changed while history was being browsed."), nil
	}
	return b.nativePage(request.Scope, entries, more, corrupt, metadata.Total, fmt.Sprint(metadata.MessageID), revision, cursor), nil
}
