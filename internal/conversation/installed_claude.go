package conversation

import (
	"context"
	"errors"
	"io"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// readInstalledClaudeSegmentIn retains one admitted operation through capture,
// descriptor validation, bounded reads, physical close and final publication.
func readInstalledClaudeSegmentIn(owner *conversationlife.Owner, parent *conversationlife.Call, segment claudeSegment, limit int64, deps installedReaderFileDeps) (text string, clipped bool, err error) {
	err = installedFileIn(owner, parent, deps, "claude-segment", func(call *conversationlife.Call, ctx context.Context) (e error) {
		text, clipped, e = readCapturedSegmentContext(ctx, segment, limit, installedClaudeSegmentAccess(owner, call, deps))
		return e
	})
	if err != nil {
		return "", false, err
	}
	return text, clipped, nil
}

// This factory only binds callbacks. Every invocation validates the exact bound
// Call before consulting supplied context methods, observations or dependencies.
// No ordinary factory or native file projection participates in this binding.
func installedClaudeSegmentAccess(owner *conversationlife.Owner, parent *conversationlife.Call, deps installedReaderFileDeps) claudeSegmentAccess {
	return func(ctx context.Context, location Location, reserve, observe func(int64) error, fn func(context.Context, claudeSegmentSource) error) error {
		if !deps.complete() || fn == nil {
			return conversationlife.ErrRefused
		}
		return owner.DoInContext(ctx, parent, func(call *conversationlife.Call) error {
			return installedFileBody(call, deps, "claude-source", func(call *conversationlife.Call, sourceCtx context.Context) (err error) {
				source, err := deps.source.captureIn(owner, call, location, reserve, observe)
				if err != nil {
					return err
				}
				// Arm close immediately, before metadata, observations or callback
				// construction. Cleanup is never gated on cancellation admission.
				defer func() {
					err = sourceResult(err, source.close())
					err = readerDiscoveryResult(sourceCtx, err)
				}()
				read := func(readCtx context.Context, start, end int64) ([]byte, error) {
					return readInstalledClaudeRangeIn(owner, call, readCtx, source, start, end, reserve, observe, deps)
				}
				peek := func(readCtx context.Context, offset int64) (byte, bool, error) {
					data, err := read(readCtx, offset, offset+1)
					if err != nil {
						return 0, false, err
					}
					if len(data) != 1 {
						return 0, false, io.ErrUnexpectedEOF
					}
					return data[0], true, nil
				}
				return fn(sourceCtx, claudeSegmentSource{end: source.end, revision: source.revision, info: source.info, read: read, location: source.location, footerRead: read, peek: peek})
			})
		})
	}
}

// Each range binds the source-owning Call, not its context. A saved read refuses
// after that Call retires, even with fresh context. Borrowers never escape this
// synchronous callback, close the source, or project a native descriptor.
func readInstalledClaudeRangeIn(owner *conversationlife.Owner, parent *conversationlife.Call, ctx context.Context, source fileSource, start, end int64, reserve, observe func(int64) error, deps installedReaderFileDeps) (data []byte, err error) {
	if !deps.complete() {
		return nil, conversationlife.ErrRefused
	}
	err = owner.DoInContext(ctx, parent, func(call *conversationlife.Call) error {
		return installedFileBody(call, deps, "claude-range", func(call *conversationlife.Call, rangeCtx context.Context) error {
			if start < 0 || end < start || end > source.end || end-start > int64(int(^uint(0)>>1)) {
				return conversationlife.ErrRefused
			}
			return source.borrowIn(owner, call, func(reader io.ReaderAt) error {
				data = make([]byte, int(end-start))
				for offset := int64(0); offset < int64(len(data)); {
					if e := readerParseContextErr(rangeCtx); e != nil {
						return e
					}
					want := int64(len(data)) - offset
					if want > 64*1024 {
						want = 64 * 1024
					}
					if e := checkedSourceHook(rangeCtx, reserve)(want); e != nil {
						return e
					}
					n, readErr := reader.ReadAt(data[offset:offset+want], start+offset)
					// Account positive partial bytes even when the read failed or
					// cancellation arrived during it; retain all error siblings.
					var observeErr error
					if n > 0 && observe != nil {
						observeErr = observe(int64(n))
					}
					// Only a bare EOF with the complete planned bytes is benign.
					// EOF membership must not hide joined cancellation or failures.
					if readErr == io.EOF && int64(n) == want {
						readErr = nil
					}
					if int64(n) != want {
						readErr = errors.Join(readErr, io.ErrUnexpectedEOF)
					}
					if e := readerDiscoveryResult(rangeCtx, errors.Join(readErr, observeErr)); e != nil {
						return e
					}
					offset += int64(n)
				}
				return readerParseContextErr(rangeCtx)
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Retain the real operation Call through all captures, closes and assembly.
func resolveInstalledClaudeChainIn(owner *conversationlife.Owner, parent *conversationlife.Call, anchor Location, sessionID string, reserve, observe func(int64) error, deps installedReaderFileDeps) (chain claudeChain, err error) {
	err = installedFileIn(owner, parent, deps, "claude-chain", func(call *conversationlife.Call, ctx context.Context) (e error) {
		chain, e = resolveClaudeChainContext(ctx, anchor, sessionID, reserve, observe, installedClaudeChainAccess(owner, call, deps))
		return e
	})
	if err != nil {
		return claudeChain{}, err
	}
	return chain, nil
}

func installedClaudeChainAccess(owner *conversationlife.Owner, parent *conversationlife.Call, deps installedReaderFileDeps) claudeChainAccess {
	return claudeChainAccess{
		source: installedClaudeSegmentAccess(owner, parent, deps),
		contained: func(ctx context.Context, path, root string) (resolved string, err error) {
			if !deps.complete() {
				return "", conversationlife.ErrRefused
			}
			// Admission precedes even the supplied context's methods. Each
			// discovery effect binds the admitted child, not context authority.
			err = owner.DoInContext(ctx, parent, func(call *conversationlife.Call) error {
				return installedFileBody(call, deps, "claude-containment", func(call *conversationlife.Call, containedCtx context.Context) (e error) {
					resolved, e = containedRegularFileContext(containedCtx, installedFileDiscovery(owner, call, deps), path, root)
					return e
				})
			})
			if err != nil {
				return "", err
			}
			return resolved, nil
		},
	}
}

func readInstalledClaudeChainIn(owner *conversationlife.Owner, parent *conversationlife.Call, sessionID string, anchor Location, before string, limit int, deps installedReaderFileDeps) (page Page, err error) {
	err = installedFileIn(owner, parent, deps, "claude-page", func(call *conversationlife.Call, ctx context.Context) (e error) {
		page, e = readClaudeChainContext(ctx, sessionID, anchor, before, limit, installedClaudeChainAccess(owner, call, deps))
		return e
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}
