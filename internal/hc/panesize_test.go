package hc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

type framedTestConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *framedTestConn) Write(p []byte) (int, error) {
	a := attachment{conn: c.Conn}
	err := a.write(0, frameData, bytes.TrimSuffix(p, []byte{'\n'}))
	return len(p), err
}

type resizeCore struct {
	mu                                         sync.Mutex
	pid, cols, rows, attaches, credits, closes int
	sizes                                      [][2]int
	conn                                       net.Conn
}

func newResizeCore(t *testing.T) (*Client, *resizeCore) {
	f := &resizeCore{pid: 42, cols: 140, rows: 45}
	path := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch req.Method {
		case "terminal.get":
			reply(conn, req.ID, "result", map[string]any{"address": "t1", "pid": f.pid, "state": "live", "cols": f.cols, "rows": f.rows})
		case "terminal.attach":
			f.attaches++
			f.cols, f.rows = int(req.Params["cols"].(float64)), int(req.Params["rows"].(float64))
			fc := conn.(*framedTestConn)
			f.conn = fc.Conn
			reply(conn, req.ID, "result", map[string]any{"channel": 1})
			go func() {
				defer func() { f.mu.Lock(); f.closes++; f.mu.Unlock() }()
				a := attachment{conn: fc.Conn}
				if a.write(1, frameData, []byte("output")) != nil {
					return
				}
				for {
					_, kind, p, err := readFrame(fc.reader)
					if err != nil {
						return
					}
					f.mu.Lock()
					switch kind {
					case frameCredit:
						f.credits += int(binary.LittleEndian.Uint32(p))
					case frameResize:
						f.cols, f.rows = int(binary.LittleEndian.Uint16(p)), int(binary.LittleEndian.Uint16(p[2:]))
						f.sizes = append(f.sizes, [2]int{f.cols, f.rows})
					case frameClose:
						f.mu.Unlock()
						return
					}
					f.mu.Unlock()
				}
			}()
		default:
			t.Errorf("unexpected %s", req.Method)
		}
	})
	return New(path), f
}
func awaitResize(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestPaneSizerLeases(t *testing.T) {
	c, f := newResizeCore(t)
	m := NewPaneSizer(c)
	defer m.Shutdown(context.Background())
	now := time.Now()
	m.now = func() time.Time { return now }
	acquire := func(owner string, cols, rows, wantCols, wantRows int) {
		t.Helper()
		x, y, err := m.Acquire(t.Context(), owner, "t1", cols, rows)
		if err != nil || x != wantCols || y != wantRows {
			t.Fatalf("acquire: %d %d %v", x, y, err)
		}
	}
	acquire("a", 80, 0, 80, 45)
	acquire("b", 60, 20, 60, 20)
	acquire("a", 50, 0, 50, 20)
	if _, _, ok := m.ReadGeometry("outsider", "t1"); ok {
		t.Fatal("unowned geometry")
	}
	if x, y, ok := m.ReadGeometry("a", "t1"); !ok || x != 50 || y != 20 {
		t.Fatal(x, y, ok)
	}
	if !m.ResizedWithin("t1", time.Second) {
		t.Fatal("resize not recorded")
	}
	if err := m.ReleaseClient(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}
	if x, y, ok := m.ReadGeometry("a", "t1"); !ok || x != 50 || y != 45 {
		t.Fatal(x, y, ok)
	}
	awaitResize(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.credits == 6 && len(f.sizes) == 3 })
	if err := m.Release(t.Context(), "a", "t1", false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(panesize.ReleaseGrace - time.Second)
	m.sweep(t.Context())
	if len(m.panes) != 1 {
		t.Fatal("grace ended early")
	}
	now = now.Add(time.Second)
	m.sweep(t.Context())
	if len(m.panes) != 0 {
		t.Fatal("grace not expired")
	}
	acquire("a", 70, 30, 70, 30)
	now = now.Add(panesize.LeaseTTL)
	m.sweep(t.Context())
	if len(m.panes) != 0 {
		t.Fatal("TTL not expired")
	}
	acquire("a", 70, 30, 70, 30)
	if err := m.Release(t.Context(), "a", "t1", true); err != nil {
		t.Fatal(err)
	}
	acquire("a", 70, 30, 70, 30)
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Acquire(t.Context(), "a", "t1", 60, 20); !errors.Is(err, panesize.ErrClosed) {
		t.Fatal(err)
	}
	awaitResize(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.closes == 4 })
}
func TestPaneSizerTerminalGone(t *testing.T) {
	for _, mode := range []string{"replacement", "EOF", "CLOSE"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newResizeCore(t)
			m := NewPaneSizer(c)
			defer m.Shutdown(context.Background())
			if _, _, err := m.Acquire(t.Context(), "a", "t1", 60, 20); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			conn := f.conn
			if mode == "replacement" {
				f.pid++
			}
			f.mu.Unlock()
			switch mode {
			case "EOF":
				conn.Close()
			case "CLOSE":
				a := attachment{conn: conn}
				if err := a.write(1, frameClose, nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "replacement" {
				awaitResize(t, func() bool { return m.panes["t1"].attach.ended() })
				m.sweep(t.Context())
			}
			if _, _, err := m.Acquire(t.Context(), "a", "t1", 70, 30); !errors.Is(err, panesize.ErrTerminalGone) {
				t.Fatal(err)
			}
			if len(m.panes) != 0 {
				t.Fatal("leases survived")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.attaches != 1 {
				t.Fatal("reattached")
			}
		})
	}
}
func TestAttachmentBoundsAndClose(t *testing.T) {
	var header [9]byte
	binary.LittleEndian.PutUint32(header[5:], maxFramePayload+1)
	if _, _, _, err := readFrame(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	a, b := net.Pipe()
	defer b.Close()
	held := &attachment{conn: a}
	done := make(chan struct{})
	go func() { _ = held.Resize(60, 20); close(done) }()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close blocked")
	}
}
