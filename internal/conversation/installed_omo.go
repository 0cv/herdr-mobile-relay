package conversation

import (
	"bytes"
	"context"
	"io"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// The callback stages private data only; acceptance follows physical close.
func withInstalledOMOSourceIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location, expected omoIdentity, cwd string, deps installedReaderFileDeps, fn func(*conversationlife.Call, context.Context, fileSource) error) (matched bool, err error) {
	if fn == nil || !deps.complete() {
		return false, conversationlife.ErrRefused
	}
	err = installedFileIn(owner, parent, deps, "omo-source", func(call *conversationlife.Call, ctx context.Context) (e error) {
		source, e := deps.source.captureIn(owner, call, location, nil, nil)
		if e != nil {
			return e
		}
		defer func() {
			e = sourceResult(e, source.close())
			e = readerDiscoveryResult(ctx, e)
		}()
		e = source.borrowIn(owner, call, func(reader io.ReaderAt) (identityErr error) {
			matched, identityErr = verifyOMOIdentityReaderContext(ctx, io.NewSectionReader(reader, 0, source.end), expected, cwd)
			return identityErr
		})
		if e = readerDiscoveryResult(ctx, e); e != nil {
			return e
		}
		if !matched {
			return nil
		}
		return fn(call, ctx, source)
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// Read the fixed captured tail of the already verified, still-owned source.
func loadInstalledOMOSourceIn(owner *conversationlife.Owner, parent *conversationlife.Call, source fileSource, identity omoIdentity, deps installedReaderFileDeps) (entries []Entry, plan OMOTodoState, clipped bool, err error) {
	err = installedFileIn(owner, parent, deps, "omo-load", func(call *conversationlife.Call, ctx context.Context) (e error) {
		start := int64(0)
		clipped = source.end > maxConversationBytes
		if clipped {
			start = source.end - maxConversationBytes
		}
		data, e := source.readRangeIn(owner, call, start, source.end, maxConversationBytes)
		if e = readerDiscoveryResult(ctx, e); e != nil {
			return e
		}
		if clipped {
			if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
				data = data[newline+1:]
			} else {
				data = nil
			}
		}
		if e = readerParseContextErr(ctx); e != nil {
			return e
		}
		text := string(data)
		if e = readerParseContextErr(ctx); e != nil {
			return e
		}
		entries, e = parseTranscriptContext(ctx, "pi", text)
		if e = readerDiscoveryResult(ctx, e); e != nil {
			return e
		}
		plan = OMOTodoState{Available: true, SessionID: identity.ID, Phases: []OMOTodoPhase{}, Truncated: clipped}
		state, found, invalidOnly, e := latestValidOMOTodoContext(ctx, text, identity.ID)
		if e = readerDiscoveryResult(ctx, e); e != nil {
			return e
		}
		if found {
			state.Available = true
			state.Truncated = state.Truncated || clipped
			plan = state
		} else if invalidOnly {
			plan.ReasonCode = "source_corrupt"
		}
		return nil
	})
	if err != nil {
		return nil, OMOTodoState{}, false, err
	}
	return entries, plan, clipped, nil
}

// No public routing or cache publication occurs inside the source callback.
func readInstalledOMOContentIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location, expected omoIdentity, cwd string, deps installedReaderFileDeps) (entries []Entry, plan OMOTodoState, clipped bool, matched bool, err error) {
	err = installedFileIn(owner, parent, deps, "omo-content", func(call *conversationlife.Call, ctx context.Context) (e error) {
		matched, e = withInstalledOMOSourceIn(owner, call, location, expected, cwd, deps, func(sourceCall *conversationlife.Call, sourceCtx context.Context, source fileSource) (loadErr error) {
			entries, plan, clipped, loadErr = loadInstalledOMOSourceIn(owner, sourceCall, source, expected, deps)
			return readerDiscoveryResult(sourceCtx, loadErr)
		})
		return readerDiscoveryResult(ctx, e)
	})
	if err != nil {
		return nil, OMOTodoState{}, false, false, err
	}
	if !matched {
		return nil, OMOTodoState{}, false, false, nil
	}
	return entries, plan, clipped, true, nil
}
