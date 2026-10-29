//go:build linux || darwin

package localcontrol

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

const installedFrameTimeout = 3 * time.Second
const installedHandlers = 16

var errInstalledEndpoint = errors.New("installed endpoint refused")
var errInstalledCleanup = errors.New("installed endpoint cleanup uncertain")

// ServeInstalledEndpoint owns only operator lifetime. The trusted caller must
// invoke it inside the concrete owned app callback with the ORIGINAL selection.
// Correlation and binding do not establish backend ownership or readiness.
func ServeInstalledEndpoint(ctx context.Context, selected config.InstalledConfig, core InstalledControl, credentials InstalledCredentials) (result error) {
	return installedEndpointEntry(ctx, selected, core, credentials, installedEndpointNative)
}

// Private entry seam lets refusal tests deny all acquisition, even on regression.
func installedEndpointEntry(ctx context.Context, selected config.InstalledConfig, core InstalledControl, credentials InstalledCredentials, acquire func(context.Context, config.InstalledConfig, InstalledControl, InstalledCredentials) error) error {
	defer core.Retire()
	if !installedEndpointAuthority(ctx, selected, core, credentials) {
		return errInstalledEndpoint
	}
	return acquire(ctx, selected, core, credentials)
}

func installedEndpointNative(ctx context.Context, selected config.InstalledConfig, core InstalledControl, credentials InstalledCredentials) (result error) {
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	bridge := installedIOBridge(core.state.ctx, cancel)
	defer bridge()
	h, err := PrepareInstalledHandoff(lifetime, selected, credentials)
	defer func() { core.Retire(); bridge(); result = errors.Join(result, h.Close()) }()
	if err != nil {
		return err
	}
	path, err := h.SocketPath(lifetime)
	if err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return errInstalledEndpoint
	}
	listener.SetUnlinkOnClose(false)
	var closeOnce sync.Once
	var closeErr error
	closeListener := func() error {
		closeOnce.Do(func() {
			if listener.Close() != nil {
				closeErr = errInstalledCleanup
			}
		})
		return closeErr
	}
	var id handoffIdentity
	defer func() {
		result = errors.Join(result, closeListener())
		core.Retire()
		bridge()
		if id == (handoffIdentity{}) || installedSocketCheck(h.state, id, false) != nil || unix.Unlinkat(h.state.gen, "s", 0) != nil || unix.Fsync(h.state.gen) != nil {
			result = errors.Join(result, errInstalledCleanup)
		}
	}()
	var st unix.Stat_t
	if unix.Fstatat(h.state.gen, "s", &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || uint64(st.Dev) != h.state.genID.Dev {
		return errInstalledEndpoint
	}
	id = handoffID(st)
	// No-follow protection of only our newly bound leaf. Same-UID actors are
	// cooperative: the identity checks cannot make check/use atomic against them.
	if installedSocketCheck(h.state, id, false) != nil || unix.Fchmodat(h.state.gen, "s", 0600, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return errInstalledEndpoint
	}
	id.Mode = (id.Mode &^ 07777) | 0600
	if installedSocketCheck(h.state, id, true) != nil {
		return errInstalledEndpoint
	}
	if _, err = h.PublishDiscovery(lifetime); err != nil {
		return err
	}
	return installedAccept(lifetime, listener, core, closeListener)
}

func installedEndpointAuthority(ctx context.Context, selected config.InstalledConfig, core InstalledControl, c InstalledCredentials) bool {
	cfg := selected.Snapshot()
	if ctx == nil || ctx.Err() != nil || selected.Validate(&cfg) != nil || core.state == nil || !handoffCredentials(c) || cfg.InstanceID != c.Installation {
		return false
	}
	s := core.state
	s.mu.Lock()
	defer s.mu.Unlock()
	secret, e := hex.DecodeString(c.Capability)
	return e == nil && s.proposal.Origin == cfg.TailscaleCLIOrigin && int(s.proposal.RelayPort) == cfg.Port && !s.retired && s.ctx.Err() == nil && c.Run == s.proposal.Run && c.Installation == s.proposal.Installation && subtle.ConstantTimeCompare(secret, s.capability[:]) == 1
}
func installedSocketCheck(s *handoffState, id handoffIdentity, private bool) error {
	var st unix.Stat_t
	if s.validate() != nil || s.binding() != nil || unix.Fstatat(s.gen, "s", &st, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffID(st) != id || st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || uint64(st.Dev) != s.genID.Dev || (private && st.Mode&07777 != 0600) {
		return errInstalledEndpoint
	}
	return nil
}

// The accept loop is the sole WaitGroup producer and finishes before Wait.
func installedAccept(ctx context.Context, l *net.UnixListener, core InstalledControl, closeListener func() error) error {
	return installedAcceptIO(ctx, l, core, closeListener, func(c net.Conn) error {
		u, ok := c.(*net.UnixConn)
		if !ok {
			return errInstalledEndpoint
		}
		return installedPeer(u)
	})
}

// Private generic accept engine for in-memory lifecycle tests. Exported native
// entrypoints always supply the non-bypassable effective-UID check above.
func installedAcceptIO(ctx context.Context, l net.Listener, core InstalledControl, closeListener func() error, peer func(net.Conn) error) error {
	lifetime, cancel := context.WithCancel(ctx)
	stop := installedIOBridge(lifetime, func() { _ = closeListener() })
	var workers sync.WaitGroup
	slots := make(chan struct{}, installedHandlers)
	var result error
	var cleanupMu sync.Mutex
	var cleanupErr error
	recordClose := func(c net.Conn) {
		if c.Close() != nil {
			cleanupMu.Lock()
			cleanupErr = errInstalledCleanup
			cleanupMu.Unlock()
		}
	}
	for lifetime.Err() == nil {
		c, e := l.Accept()
		if e != nil {
			if lifetime.Err() == nil {
				result = errInstalledEndpoint
			}
			break
		}
		c = &installedOwnedConn{Conn: c}
		select {
		case slots <- struct{}{}:
		default:
			recordClose(c)
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			defer recordClose(c)
			if peer(c.(*installedOwnedConn).Conn) != nil {
				return
			}
			installedServeConnection(lifetime, c, core)
		}()
	}
	cancel()
	listenerErr := closeListener()
	core.Retire()
	workers.Wait()
	stop()
	return errors.Join(result, listenerErr, cleanupErr)
}

// One synchronized close result survives cancellation, handler and owner cleanup.
// Retain only a finite redacted marker; never retain per-connection error lists.
type installedOwnedConn struct {
	net.Conn
	once sync.Once
	err  error
}

func (c *installedOwnedConn) Close() error {
	c.once.Do(func() {
		if err := c.Conn.Close(); err != nil && !installedClosedOnly(err) {
			c.err = errInstalledCleanup
		}
	})
	return c.err
}

// Inspect every branch before suppressing a close error: errors.Is alone can
// match one closed sibling while hiding independent cleanup uncertainty.
func installedClosedOnly(err error) bool {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !installedClosedOnly(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return installedClosedOnly(e.Unwrap())
	default:
		return err == net.ErrClosed || err == os.ErrClosed
	}
}

func installedIOBridge(ctx context.Context, f func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); f() })
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
		})
	}
}
func installedReadFrame(r *bufio.Reader, limit int) ([]byte, error) {
	frame, e := r.ReadSlice('\n')
	if e != nil || len(frame) < 2 || len(frame) > limit+1 {
		return nil, errInstalledEndpoint
	}
	return frame[:len(frame)-1], nil
}
func installedWriteFrame(c net.Conn, data []byte, limit int) error {
	if len(data) == 0 || len(data) > limit {
		return errInstalledEndpoint
	}
	if c.SetWriteDeadline(time.Now().Add(installedFrameTimeout)) != nil {
		return errInstalledEndpoint
	}
	frame := append(append([]byte(nil), data...), '\n')
	for len(frame) > 0 {
		n, e := c.Write(frame)
		if e != nil || n <= 0 {
			return errInstalledEndpoint
		}
		frame = frame[n:]
	}
	return nil
}

// Private in-memory seam: native callers ALWAYS perform peer checks first.
func installedServeConnection(root context.Context, c net.Conn, core InstalledControl) (result error) {
	if _, ok := c.(*installedOwnedConn); !ok {
		c = &installedOwnedConn{Conn: c}
	}
	ctx, cancel := context.WithCancel(root)
	stop := installedIOBridge(ctx, func() { _ = c.Close() })
	defer func() { cancel(); result = c.Close(); stop() }()
	if c.SetReadDeadline(time.Now().Add(installedFrameTimeout)) != nil {
		return
	}
	r := bufio.NewReaderSize(c, MaxRequestBytes+2)
	data, e := installedReadFrame(r, MaxRequestBytes)
	if e != nil || ctx.Err() != nil {
		return
	}
	if c.SetReadDeadline(time.Time{}) != nil {
		return
	}
	// ReadSlice aliases the reader buffer; the watcher must not race decoding.
	data = append([]byte(nil), data...)
	// A single reader watches EOF AND surplus bytes, never dispatches again.
	done := make(chan struct{})
	go func() { defer close(done); var b [1]byte; _, _ = io.ReadFull(r, b[:]); cancel() }()
	defer func() { cancel(); _ = c.Close(); <-done }()
	reply := core.Dispatch(ctx, data)
	if ctx.Err() != nil || core.state == nil || core.state.ctx.Err() != nil {
		return
	}
	_ = installedWriteFrame(c, reply, 8192)
	return
}
