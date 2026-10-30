package pibridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type activityTransportRequest struct {
	Identity
	Challenge string `json:"challenge"`
	Operation string `json:"operation"`
	Cursor    string `json:"cursor"`
	Limit     int    `json:"limit"`
}

type activityTransportPeer struct {
	identity  Identity
	directory string
	path      string
	listener  *net.UnixListener
	accepted  atomic.Int32
}

func activityTransportFixture(t *testing.T, pid int, handle func(context.Context, *net.UnixConn, Identity) error) *activityTransportPeer {
	t.Helper()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	peer := &activityTransportPeer{identity: Identity{Instance: hex.EncodeToString(nonce[:]), Pane: "transport-fixture", Session: "transport-session", PID: pid}}
	peer.directory = Directory(peer.identity.Instance)
	if err := os.Mkdir(peer.directory, 0700); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(peer.directory)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := original.Sys().(*syscall.Stat_t)
	if !ok || !original.IsDir() || original.Mode().Perm() != 0700 || int(owner.Uid) != os.Getuid() {
		t.Fatal("new fixture directory is not private and owned")
	}
	t.Logf("owned activity transport fixture: %s", peer.directory)
	t.Cleanup(func() {
		current, err := os.Lstat(peer.directory)
		if err != nil || !current.IsDir() || !os.SameFile(original, current) {
			t.Errorf("refusing to remove changed fixture directory: %v", err)
			return
		}
		currentOwner, ok := current.Sys().(*syscall.Stat_t)
		if !ok || int(currentOwner.Uid) != os.Getuid() {
			t.Error("refusing to remove unowned fixture directory")
			return
		}
		if err := os.RemoveAll(peer.directory); err != nil {
			t.Error(err)
			return
		}
		t.Logf("removed activity transport fixture: %s", peer.directory)
	})
	peer.path = filepath.Join(peer.directory, fmt.Sprintf("%d.activity.sock", pid))
	peer.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: peer.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	peer.listener.SetUnlinkOnClose(false)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	workerErrors := make(chan error, 64)
	workers.Add(1)
	t.Cleanup(func() {
		cancel()
		if err := peer.listener.Close(); err != nil {
			t.Error(err)
		}
		joined := make(chan struct{})
		go func() { workers.Wait(); close(joined) }()
		select {
		case <-joined:
			for len(workerErrors) > 0 {
				t.Error(<-workerErrors)
			}
		case <-time.After(3 * time.Second):
			t.Error("activity transport fixture workers did not join")
		}
	})
	if err := os.Chmod(peer.path, 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer workers.Done()
		for {
			conn, err := peer.listener.AcceptUnix()
			if err != nil {
				if ctx.Err() == nil {
					workerErrors <- err
				}
				return
			}
			peer.accepted.Add(1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
					workerErrors <- err
					return
				}
				if err := handle(ctx, conn, peer.identity); err != nil && ctx.Err() == nil {
					workerErrors <- err
				}
			}()
		}
	}()
	return peer
}

func activityTransportReceive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(25 * time.Second):
		t.Fatal("activity transport observation did not arrive")
		var zero T
		return zero
	}
}

func activityTransportStart(t *testing.T, cancel context.CancelFunc, call func() error) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("activity transport client did not join")
		}
	})
	go func() {
		defer close(joined)
		result <- call()
	}()
	return result
}

func activityTransportDecode(conn *net.UnixConn, identity Identity) (activityTransportRequest, error) {
	var request activityTransportRequest
	err := json.NewDecoder(conn).Decode(&request)
	if err == nil && (request.Identity != identity || request.Limit != 200 || request.Cursor != "" || (request.Operation != "page" && request.Operation != "subscribe")) {
		err = fmt.Errorf("unexpected activity request: %+v", request)
	}
	return request, err
}

func activityTransportFrame(request activityTransportRequest) map[string]any {
	frame := goodActivityFrame(map[string]any{"instance": request.Instance, "pane": request.Pane, "session": request.Session, "pid": request.PID, "challenge": request.Challenge})
	if request.Operation == "subscribe" {
		frame["kind"] = "snapshot"
	}
	return frame
}

func activityTransportEOF(conn *net.UnixConn) error {
	data, err := io.ReadAll(conn)
	if err != nil || len(data) != 0 {
		return fmt.Errorf("expected zero raw bytes through EOF: bytes=%d error=%v", len(data), err)
	}
	return nil
}

func activityTransportReply(_ context.Context, conn *net.UnixConn, identity Identity) error {
	request, err := activityTransportDecode(conn, identity)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(activityTransportFrame(request)); err != nil {
		return err
	}
	return activityTransportEOF(conn)
}

func activityTransportCall(ctx context.Context, identity Identity, mode string, receive func(ActivityFrame) error) error {
	if mode == "stream" {
		return ActivityStream(ctx, identity, receive)
	}
	page, err := ActivityPage(ctx, identity, "", 200)
	if err == nil && (len(page.Entries) != 1 || page.Entries[0].Text != "fixture") {
		return errors.New("page made no public progress")
	}
	return err
}

func activityTransportProgress(t *testing.T, mode string) {
	t.Helper()
	peer := activityTransportFixture(t, os.Getpid(), activityTransportReply)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	sentinel := errors.New("progress callback completed")
	calls := 0
	err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls++; return sentinel })
	if mode == "page" && err != nil || mode == "stream" && (err != sentinel || calls != 1) {
		t.Fatalf("own lane unavailable after release: mode=%s callbacks=%d error=%v", mode, calls, err)
	}
}

func TestActivityTransportFreshChallenge(t *testing.T) {
	for _, mode := range []string{"page", "stream"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan activityTransportRequest, 2)
			closed := make(chan error, 2)
			peer := activityTransportFixture(t, os.Getpid(), func(_ context.Context, conn *net.UnixConn, identity Identity) error {
				request, err := activityTransportDecode(conn, identity)
				if err != nil {
					return err
				}
				requests <- request
				if err := json.NewEncoder(conn).Encode(activityTransportFrame(request)); err != nil {
					return err
				}
				closed <- activityTransportEOF(conn)
				return nil
			})
			seen := make(map[string]bool)
			sentinel := errors.New("fresh challenge callback")
			for range 2 {
				ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
				err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { return sentinel })
				cancel()
				if mode == "page" && err != nil || mode == "stream" && err != sentinel {
					t.Fatalf("%s: %v", mode, err)
				}
				request := activityTransportReceive(t, requests)
				decoded, err := hex.DecodeString(request.Challenge)
				if err != nil || len(decoded) != 16 || request.Challenge != strings.ToLower(request.Challenge) || seen[request.Challenge] {
					t.Fatalf("invalid or reused challenge: %q", request.Challenge)
				}
				seen[request.Challenge] = true
				if err := activityTransportReceive(t, closed); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestActivityTransportWrongPeerZeroRawBytes(t *testing.T) {
	for _, mode := range []string{"page", "stream"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan error, 1)
			peer := activityTransportFixture(t, os.Getpid()+1, func(_ context.Context, conn *net.UnixConn, _ Identity) error {
				closed <- activityTransportEOF(conn)
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			defer cancel()
			calls := 0
			err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls++; return nil })
			if err == nil || (err.Error() != "bridge process mismatch" && err.Error() != "bridge peer mismatch") || calls != 0 {
				t.Fatalf("wrong peer accepted: callbacks=%d error=%v", calls, err)
			}
			if err := activityTransportReceive(t, closed); err != nil {
				t.Fatal(err)
			}
			activityTransportProgress(t, mode)
		})
	}
}

func TestActivityTransportUnsafeEndpoint(t *testing.T) {
	for _, mode := range []string{"page", "stream"} {
		for _, kind := range []string{"directory0755", "socket0666", "nonsocket", "symlink"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				peer := activityTransportFixture(t, os.Getpid(), func(_ context.Context, conn *net.UnixConn, _ Identity) error { return activityTransportEOF(conn) })
				switch kind {
				case "directory0755":
					if err := os.Chmod(peer.directory, 0755); err != nil {
						t.Fatal(err)
					}
				case "socket0666":
					if err := os.Chmod(peer.path, 0666); err != nil {
						t.Fatal(err)
					}
				case "nonsocket", "symlink":
					if err := os.Rename(peer.path, peer.path+".original"); err != nil {
						t.Fatal(err)
					}
					if kind == "nonsocket" {
						if err := os.WriteFile(peer.path, nil, 0600); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Symlink(filepath.Base(peer.path)+".original", peer.path); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
				defer cancel()
				calls := 0
				err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls++; return nil })
				if err == nil || err.Error() != "unsafe bridge endpoint" || calls != 0 {
					t.Fatalf("unsafe endpoint accepted: callbacks=%d error=%v", calls, err)
				}
				time.Sleep(100 * time.Millisecond)
				if peer.accepted.Load() != 0 {
					t.Fatal("a connection escaped to the unsafe endpoint")
				}
				activityTransportProgress(t, mode)
			})
		}
	}
}

func TestActivityTransportRejectsFrameBinding(t *testing.T) {
	for _, mode := range []string{"page", "stream"} {
		for _, field := range []string{"instance", "pane", "session", "pid", "challenge", "incarnation"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				closed := make(chan error, 1)
				peer := activityTransportFixture(t, os.Getpid(), func(_ context.Context, conn *net.UnixConn, identity Identity) error {
					request, err := activityTransportDecode(conn, identity)
					if err != nil {
						return err
					}
					frame := activityTransportFrame(request)
					if field == "pid" {
						frame[field] = identity.PID + 1
					} else {
						frame[field] = "wrong"
					}
					if err := json.NewEncoder(conn).Encode(frame); err != nil {
						return err
					}
					closed <- activityTransportEOF(conn)
					return nil
				})
				ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
				defer cancel()
				calls := 0
				err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls++; return nil })
				if err == nil || err.Error() != "activity source replaced" || calls != 0 {
					t.Fatalf("binding accepted: callbacks=%d error=%v", calls, err)
				}
				if err := activityTransportReceive(t, closed); err != nil {
					t.Fatal(err)
				}
				activityTransportProgress(t, mode)
			})
		}
	}
}

func TestActivityTransportRejectsChangeBetweenFrames(t *testing.T) {
	for _, kind := range []string{"socket_inode", "incarnation"} {
		t.Run(kind, func(t *testing.T) {
			accepted := make(chan struct{})
			closed := make(chan error, 1)
			peer := activityTransportFixture(t, os.Getpid(), func(ctx context.Context, conn *net.UnixConn, identity Identity) error {
				request, err := activityTransportDecode(conn, identity)
				if err != nil {
					return err
				}
				frame := activityTransportFrame(request)
				if err := json.NewEncoder(conn).Encode(frame); err != nil {
					return err
				}
				select {
				case <-accepted:
				case <-ctx.Done():
					return ctx.Err()
				}
				if kind == "socket_inode" {
					path := filepath.Join(Directory(identity.Instance), fmt.Sprintf("%d.activity.sock", identity.PID))
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
					replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
					if err != nil {
						return err
					}
					replacement.SetUnlinkOnClose(false)
					defer replacement.Close()
					if err := os.Chmod(path, 0600); err != nil {
						return err
					}
				} else {
					frame["incarnation"] = strings.Repeat("f", 36)
				}
				if err := json.NewEncoder(conn).Encode(frame); err != nil {
					return err
				}
				closed <- activityTransportEOF(conn)
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			defer cancel()
			calls := 0
			err := ActivityStream(ctx, peer.identity, func(ActivityFrame) error {
				calls++
				if calls == 1 {
					close(accepted)
				}
				return nil
			})
			if err == nil || err.Error() != "activity source replaced" || calls != 1 {
				t.Fatalf("changed second frame reached callback: callbacks=%d error=%v", calls, err)
			}
			if err := activityTransportReceive(t, closed); err != nil {
				t.Fatal(err)
			}
			activityTransportProgress(t, "stream")
		})
	}
}

func TestActivityTransportCancellation(t *testing.T) {
	for _, mode := range []string{"page", "stream"} {
		t.Run(mode+"/precancel", func(t *testing.T) {
			peer := activityTransportFixture(t, os.Getpid(), activityTransportReply)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			calls := 0
			start := time.Now()
			err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls++; return nil })
			if err == nil || calls != 0 || time.Since(start) > time.Second {
				t.Fatalf("pre-cancel not bounded: callbacks=%d error=%v", calls, err)
			}
			activityTransportProgress(t, mode)
		})
		t.Run(mode+"/before_first_frame", func(t *testing.T) {
			ready := make(chan struct{}, 1)
			closed := make(chan error, 1)
			peer := activityTransportFixture(t, os.Getpid(), func(_ context.Context, conn *net.UnixConn, identity Identity) error {
				if _, err := activityTransportDecode(conn, identity); err != nil {
					return err
				}
				ready <- struct{}{}
				closed <- activityTransportEOF(conn)
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			var calls atomic.Int32
			result := activityTransportStart(t, cancel, func() error {
				return activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls.Add(1); return nil })
			})
			activityTransportReceive(t, ready)
			start := time.Now()
			cancel()
			if err := activityTransportReceive(t, result); err == nil || calls.Load() != 0 || time.Since(start) > time.Second {
				t.Fatalf("cancel before frame: callbacks=%d error=%v", calls.Load(), err)
			}
			if err := activityTransportReceive(t, closed); err != nil {
				t.Fatal(err)
			}
			activityTransportProgress(t, mode)
		})
	}
	t.Run("stream/after_accepted_frame", func(t *testing.T) {
		accepted := make(chan struct{}, 1)
		closed := make(chan error, 1)
		peer := activityTransportFixture(t, os.Getpid(), func(ctx context.Context, conn *net.UnixConn, identity Identity) error {
			err := activityTransportReply(ctx, conn, identity)
			closed <- err
			return nil
		})
		ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
		var calls atomic.Int32
		result := activityTransportStart(t, cancel, func() error {
			return ActivityStream(ctx, peer.identity, func(ActivityFrame) error { calls.Add(1); accepted <- struct{}{}; return nil })
		})
		activityTransportReceive(t, accepted)
		start := time.Now()
		cancel()
		if err := activityTransportReceive(t, result); err == nil || calls.Load() != 1 || time.Since(start) > time.Second {
			t.Fatalf("cancel after frame: callbacks=%d error=%v", calls.Load(), err)
		}
		if err := activityTransportReceive(t, closed); err != nil {
			t.Fatal(err)
		}
		activityTransportProgress(t, "stream")
	})
}

func TestActivityTransportCallbackInProgress(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	closed := make(chan error, 1)
	peer := activityTransportFixture(t, os.Getpid(), func(ctx context.Context, conn *net.UnixConn, identity Identity) error {
		err := activityTransportReply(ctx, conn, identity)
		closed <- err
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	sentinel := errors.New("callback owns its result after cancellation")
	result := activityTransportStart(t, cancel, func() error {
		return ActivityStream(ctx, peer.identity, func(ActivityFrame) error {
			entered <- struct{}{}
			<-release
			return sentinel
		})
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	activityTransportReceive(t, entered)
	cancel()
	if err := activityTransportReceive(t, closed); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatalf("cancellation interrupted the callback: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	unblock()
	if err := activityTransportReceive(t, result); err != sentinel {
		t.Fatalf("callback error lost precedence: %v", err)
	}
	activityTransportProgress(t, "stream")
}

func TestActivityTransportCallbackSentinelAndCapacity(t *testing.T) {
	closed := make(chan error, 10)
	peer := activityTransportFixture(t, os.Getpid(), func(ctx context.Context, conn *net.UnixConn, identity Identity) error {
		err := activityTransportReply(ctx, conn, identity)
		closed <- err
		return nil
	})
	sentinel := errors.New("receive callback sentinel")
	for range 10 {
		ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
		calls := 0
		err := ActivityStream(ctx, peer.identity, func(ActivityFrame) error { calls++; return sentinel })
		cancel()
		if err != sentinel || calls != 1 {
			t.Fatalf("callback sentinel/capacity lost: callbacks=%d error=%v", calls, err)
		}
		if err := activityTransportReceive(t, closed); err != nil {
			t.Fatal(err)
		}
	}
	activityTransportProgress(t, "stream")
	activityTransportProgress(t, "page")
}

func TestActivityTransportInitialTwoSecondDeadline(t *testing.T) {
	for _, mode := range []string{"page", "stream"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan error, 1)
			peer := activityTransportFixture(t, os.Getpid(), func(_ context.Context, conn *net.UnixConn, identity Identity) error {
				if _, err := activityTransportDecode(conn, identity); err != nil {
					return err
				}
				closed <- activityTransportEOF(conn)
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			defer cancel()
			calls := 0
			start := time.Now()
			err := activityTransportCall(ctx, peer.identity, mode, func(ActivityFrame) error { calls++; return nil })
			elapsed := time.Since(start)
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil || calls != 0 || elapsed < 1700*time.Millisecond || elapsed > 4*time.Second {
				t.Fatalf("initial deadline: elapsed=%s callbacks=%d context=%v error=%v", elapsed, calls, ctx.Err(), err)
			}
			t.Logf("actual initial timeout: %s", elapsed)
			if err := activityTransportReceive(t, closed); err != nil {
				t.Fatal(err)
			}
			activityTransportProgress(t, mode)
		})
	}
}

func TestActivityTransportContinuingFifteenSecondReadDeadline(t *testing.T) {
	accepted := make(chan time.Time, 1)
	closed := make(chan error, 1)
	peer := activityTransportFixture(t, os.Getpid(), func(ctx context.Context, conn *net.UnixConn, identity Identity) error {
		err := activityTransportReply(ctx, conn, identity)
		closed <- err
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	var calls atomic.Int32
	result := activityTransportStart(t, cancel, func() error {
		return ActivityStream(ctx, peer.identity, func(ActivityFrame) error { calls.Add(1); accepted <- time.Now(); return nil })
	})
	first := activityTransportReceive(t, accepted)
	select {
	case err := <-result:
		t.Fatalf("stream ended before continuing read window: %v", err)
	case err := <-closed:
		t.Fatalf("transport closed before continuing read window: %v", err)
	case <-time.After(3 * time.Second):
	}
	err := activityTransportReceive(t, result)
	elapsed := time.Since(first)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil || calls.Load() != 1 || elapsed < 14*time.Second || elapsed > 22*time.Second {
		t.Fatalf("continuing read deadline: elapsed=%s callbacks=%d context=%v error=%v", elapsed, calls.Load(), ctx.Err(), err)
	}
	t.Logf("actual continuing read timeout after accepted callback: %s", elapsed)
	if err := activityTransportReceive(t, closed); err != nil {
		t.Fatal(err)
	}
	activityTransportProgress(t, "stream")
}
