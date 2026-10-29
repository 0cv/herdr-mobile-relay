package conversation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// ErrSourceCleanupUncertain is safe to classify, not a protocol reason. Raw
// close operands stay in Owner.Witnesses and their owning Browser's Close result,
// never in request errors; primary errors survive.
var ErrSourceCleanupUncertain = errors.New("conversation source cleanup uncertain")

// Ordinary captures share first-close state without adopting an installed owner.
// Legacy Browser direct native-file retirement is unchanged by this helper.
type ordinarySourceClose struct {
	once sync.Once
	file *os.File
	err  error
}

func (s *ordinarySourceClose) close() error {
	s.once.Do(func() { s.err = s.file.Close() })
	return s.err
}

type sourceHandle interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Close() error
}

// This private bundle is selected explicitly, never completed from defaults.
// open must use acquireSourceHandle or acquireSourceRaw: ownership starts there,
// not when the returned source reaches its caller.
type installedSourceDeps struct {
	open func(context.Context, *conversationlife.Owner, string, func(error)) (*sourceOwnership, error)
	// Set by the Browser before acquisition, including partial-return unwind.
	onClose func(error)
	eval    func(string) (string, error)
	stat    func(string) (os.FileInfo, error)
	same    func(os.FileInfo, os.FileInfo) bool
}

func productionSourceDeps() installedSourceDeps {
	return installedSourceDeps{open: openInstalledConversationSource, eval: filepath.EvalSymlinks, stat: os.Stat, same: os.SameFile}
}

func (d installedSourceDeps) complete() bool {
	return d.open != nil && d.eval != nil && d.stat != nil && d.same != nil
}

// Copies share this object. mu serializes a synchronous borrow against close;
// the integrating Reader must still retain its enclosing Call through close.
type sourceOwnership struct {
	owner     *conversationlife.Owner
	slot      *conversationlife.CloseSlot
	handle    sourceHandle
	mu        sync.Mutex
	closed    bool
	uncertain bool
	// Only Browser-owned sources retain their own physical close fault for
	// Browser.Close; callers still receive the safe cleanup classification.
	onClose func(error)
}

func nilSourceHandle(h sourceHandle) bool {
	if h == nil {
		return true
	}
	v := reflect.ValueOf(h)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func (s *sourceOwnership) close() error {
	if s == nil || s.slot == nil {
		return conversationlife.ErrRefused
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uncertain {
		return ErrSourceCleanupUncertain
	}
	s.closed = true
	// sync.Once also retires on panic/Goexit. Never let that look like cached
	// success if an internal logical closer exits without returning a result.
	s.uncertain = true
	if err := s.slot.Close(); err != nil {
		if s.onClose != nil {
			s.onClose(err)
		}
		return ErrSourceCleanupUncertain
	}
	s.uncertain = false
	return nil
}

func sourceResult(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, ErrSourceCleanupUncertain)
}

// The acquisition callback may return a handle AND an error. No metadata or
// cancellation observation intervenes between that return and arming unwind.
func acquireSourceHandle(owner *conversationlife.Owner, onClose func(error), open func() (sourceHandle, error)) (result *sourceOwnership, err error) {
	h, primary := open()
	if nilSourceHandle(h) {
		if primary != nil {
			return nil, primary
		}
		return nil, conversationlife.ErrRefused
	}
	s := &sourceOwnership{owner: owner, handle: h, onClose: onClose}
	keep := false
	defer func() {
		if !keep {
			if s.slot != nil {
				err = sourceResult(err, s.close())
			} else {
				err = errors.Join(err, h.Close())
			}
		}
	}()
	s.slot, err = owner.Own(h.Close)
	if err != nil {
		return nil, err
	}
	if primary != nil {
		return nil, primary
	}
	keep = true
	return s, nil
}

// Raw authority is represented by a closure, never by a second os.File. The
// single slot switches to the adapted handle BEFORE any further observation.
// adapt must transfer that same resource, not acquire an unrelated handle.
func acquireSourceRaw(ctx context.Context, owner *conversationlife.Owner, onClose func(error), open func() (func() error, func() (sourceHandle, error), error)) (result *sourceOwnership, err error) {
	rawClose, adapt, primary := open()
	if rawClose == nil {
		if primary != nil {
			return nil, primary
		}
		return nil, conversationlife.ErrRefused
	}
	closeAuthority := rawClose
	s := &sourceOwnership{owner: owner, onClose: onClose}
	keep := false
	defer func() {
		if !keep {
			if s.slot != nil {
				err = sourceResult(err, s.close())
			} else {
				err = errors.Join(err, closeAuthority())
			}
		}
	}()
	s.slot, err = owner.Own(func() error { return closeAuthority() })
	if err != nil {
		return nil, err
	}
	if primary != nil {
		return nil, primary
	}
	if adapt == nil {
		return nil, conversationlife.ErrRefused
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, adaptErr := adapt()
	if !nilSourceHandle(h) {
		closeAuthority = h.Close
		s.handle = h
	}
	if cancelled := ctx.Err(); cancelled != nil {
		return nil, errors.Join(adaptErr, cancelled)
	}
	if adaptErr != nil {
		return nil, adaptErr
	}
	if nilSourceHandle(h) {
		return nil, conversationlife.ErrRefused
	}
	keep = true
	return s, nil
}

func sourceEffect(ctx context.Context, effect func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := effect()
	if cancelled := ctx.Err(); cancelled != nil {
		return errors.Join(err, cancelled)
	}
	return err
}

func (d installedSourceDeps) contained(ctx context.Context, path, root string) (string, error) {
	var realRoot, realPath string
	var info os.FileInfo
	if err := sourceEffect(ctx, func() (err error) { realRoot, err = d.eval(root); return }); err != nil {
		return "", err
	}
	if err := sourceEffect(ctx, func() (err error) { realPath, err = d.eval(path); return }); err != nil {
		return "", err
	}
	relative, err := filepath.Rel(realRoot, realPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("conversation source is not contained")
	}
	if err := sourceEffect(ctx, func() (err error) { info, err = d.stat(realPath); return }); err != nil {
		return "", err
	}
	if info == nil || !info.Mode().IsRegular() {
		return "", errors.New("conversation source is not a regular file")
	}
	return realPath, nil
}

// openIn is metadata-only. captureIn adds the shared bounded revision anchor.
// Neither return grants a lease: retain the enclosing operation and close even
// on downstream validation/panic/Goexit. No native projection is exposed here.
func (d installedSourceDeps) openIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location) (source fileSource, err error) {
	err = owner.DoIn(parent, func(call *conversationlife.Call) (err error) {
		if !d.complete() {
			return conversationlife.ErrRefused
		}
		ctx := call.Context()
		if err := ctx.Err(); err != nil {
			return err
		}
		if location.Path == "" || location.Root == "" {
			return errors.New("conversation source is not contained")
		}
		resolved, err := d.contained(ctx, location.Path, location.Root)
		if err != nil {
			return err
		}
		if filepath.Clean(resolved) != filepath.Clean(location.Path) {
			return errors.New("conversation source is not contained")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s, openErr := d.open(ctx, owner, location.Path, d.onClose)
		keep := false
		defer func() {
			if s != nil && !keep {
				err = sourceResult(err, s.close())
			}
		}()
		if openErr != nil {
			return openErr
		}
		if s == nil || s.owner != owner || s.slot == nil || nilSourceHandle(s.handle) {
			return conversationlife.ErrRefused
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var info, pathInfo os.FileInfo
		if err := sourceEffect(ctx, func() (err error) { info, err = s.handle.Stat(); return }); err != nil {
			return err
		}
		if info == nil || !info.Mode().IsRegular() || info.Size() < 0 {
			return errors.New("conversation source is not a regular file")
		}
		if err := sourceEffect(ctx, func() (err error) { resolved, err = d.eval(location.Path); return }); err != nil {
			return err
		}
		if filepath.Clean(resolved) != filepath.Clean(location.Path) {
			return errors.New("conversation source changed while opening")
		}
		resolved, err = d.contained(ctx, location.Path, location.Root)
		if err != nil {
			return err
		}
		if resolved != location.Path {
			return errors.New("conversation source changed while opening")
		}
		if err := sourceEffect(ctx, func() (err error) { pathInfo, err = d.stat(location.Path); return }); err != nil {
			return err
		}
		if pathInfo == nil || !pathInfo.Mode().IsRegular() {
			return errors.New("conversation source changed while opening")
		}
		var same bool
		if err := sourceEffect(ctx, func() error { same = d.same(info, pathInfo); return nil }); err != nil {
			return err
		}
		if !same {
			return errors.New("conversation source changed while opening")
		}
		source = fileSource{location: location, path: location.Path, info: info, end: info.Size(), owned: s}
		keep = true
		return nil
	})
	if err != nil {
		return fileSource{}, err
	}
	return source, nil
}

// borrowIn exposes only a cancellable ReaderAt. It cannot close, seek, unwrap a
// native descriptor, or be used after the synchronous callback returns. The
// callback must not close this source or launch concurrent/retained borrowers.
func (source fileSource) borrowIn(owner *conversationlife.Owner, parent *conversationlife.Call, fn func(io.ReaderAt) error) error {
	return owner.DoIn(parent, func(call *conversationlife.Call) error {
		s := source.owned
		if s == nil || s.owner != owner || fn == nil {
			return conversationlife.ErrRefused
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || nilSourceHandle(s.handle) {
			return conversationlife.ErrRefused
		}
		reader := &sourceBorrow{ctx: call.Context(), handle: s.handle, active: true}
		defer func() { reader.active = false }()
		return sourceEffect(call.Context(), func() error { return fn(reader) })
	})
}

type sourceBorrow struct {
	ctx    context.Context
	handle io.ReaderAt
	active bool
}

func (r *sourceBorrow) ReadAt(p []byte, offset int64) (n int, err error) {
	if !r.active || offset < 0 {
		return 0, conversationlife.ErrRefused
	}
	err = sourceEffect(r.ctx, func() error { var e error; n, e = r.handle.ReadAt(p, offset); return e })
	if n < 0 || n > len(p) {
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

func (d installedSourceDeps) captureIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location, reserve, observe func(int64) error) (source fileSource, err error) {
	err = owner.DoIn(parent, func(call *conversationlife.Call) (err error) {
		candidate, err := d.openIn(owner, call, location)
		if err != nil {
			return err
		}
		keep := false
		defer func() {
			if !keep {
				err = sourceResult(err, candidate.close())
			}
		}()
		err = candidate.borrowIn(owner, call, func(reader io.ReaderAt) error {
			var revisionErr error
			candidate.revision, revisionErr = fileRevisionWithReservation(reader, candidate.info,
				checkedSourceHook(call.Context(), reserve), checkedSourceHook(call.Context(), observe))
			return revisionErr
		})
		if err != nil {
			return err
		}
		source = candidate
		keep = true
		return nil
	})
	if err != nil {
		return fileSource{}, err
	}
	return source, nil
}

func checkedSourceHook(ctx context.Context, hook func(int64) error) func(int64) error {
	return func(n int64) error {
		return sourceEffect(ctx, func() error {
			if hook != nil {
				return hook(n)
			}
			return nil
		})
	}
}

// readRangeIn is a bounded captured-segment/footer primitive. No partial data
// on error. 64 KiB chunks keep cooperative cancellation observable in loops.
func (source fileSource) readRangeIn(owner *conversationlife.Owner, parent *conversationlife.Call, start, end, limit int64) (data []byte, err error) {
	if start < 0 || end < start || end > source.end || limit < 0 || end-start > limit || end-start > int64(int(^uint(0)>>1)) {
		return nil, conversationlife.ErrRefused
	}
	err = source.borrowIn(owner, parent, func(reader io.ReaderAt) error {
		data = make([]byte, int(end-start))
		for offset := int64(0); offset < int64(len(data)); {
			want := int64(len(data)) - offset
			if want > 64*1024 {
				want = 64 * 1024
			}
			n, e := reader.ReadAt(data[offset:offset+want], start+offset)
			if e != nil {
				return e
			}
			if int64(n) != want {
				return io.ErrUnexpectedEOF
			}
			offset += int64(n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// readContentIn closes before publishing bytes. tail preserves the ordinary
// clipped-first-record rule; non-tail refuses oversized planned content.
func (d installedSourceDeps) readContentIn(owner *conversationlife.Owner, parent *conversationlife.Call, location Location, limit int64, tail bool) (data []byte, clipped bool, err error) {
	err = owner.DoIn(parent, func(call *conversationlife.Call) (err error) {
		if limit < 0 {
			return conversationlife.ErrRefused
		}
		source, err := d.openIn(owner, call, location)
		if err != nil {
			return err
		}
		defer func() {
			err = sourceResult(err, source.close())
			if e := call.Context().Err(); e != nil {
				err = errors.Join(err, e)
			}
		}()
		start := int64(0)
		clipped = source.end > limit
		if clipped {
			if !tail {
				return errors.New("conversation source exceeds content limit")
			}
			start = source.end - limit
		}
		data, err = source.readRangeIn(owner, call, start, source.end, limit)
		if err != nil {
			return err
		}
		if clipped {
			if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
				data = data[newline+1:]
			} else {
				data = nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return data, clipped, nil
}
