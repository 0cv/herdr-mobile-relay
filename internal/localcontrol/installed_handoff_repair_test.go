//go:build linux || darwin

package localcontrol

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInstalledHandoffRepairRenames(t *testing.T) {
	for _, phase := range []string{"init", "current"} {
		for _, effect := range []string{"ack-error", "ambiguous-no-effect", "collision"} {
			t.Run(phase+"/"+effect, func(t *testing.T) {
				selected, c, root := handoffFixture(t)
				rename := func(fd int, from, to string, exclusive bool) error {
					if effect == "collision" {
						return unix.EEXIST
					}
					if effect == "ack-error" {
						if e := handoffExclusive(fd, from, to); e != nil {
							return e
						}
					}
					return unix.EIO
				}
				var h InstalledHandoff
				var e error
				if phase == "init" {
					h, e = prepareHandoffRename(context.Background(), selected, c, nil, rename)
				} else {
					h = handoffPrepared(t, selected, c)
					h.state.rename = rename
					var out InstalledHandoffOutcome
					out, e = h.PublishDiscovery(context.Background())
					want := InstalledHandoffUncertain
					if effect == "collision" {
						want = InstalledHandoffUnpublished
					}
					if out != want {
						t.Error("outcome")
					}
				}
				defer h.Close()
				if e == nil || (effect != "collision" && !errors.Is(e, errHandoffUncertain)) {
					t.Error("error classification")
				}
				if phase == "current" && effect != "collision" {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					if out, _ := h.PublishDiscovery(ctx); out != InstalledHandoffUncertain {
						t.Error("lost retained outcome")
					}
				}
				if e := h.Close(); e != nil {
					t.Error("captured cleanup", e)
				}
				if phase == "init" && effect == "ack-error" {
					handoffExists(t, root+"/ic/owner")
				}
			})
		}
	}
}

func TestInstalledHandoffRepairReuseFence(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "ordered"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			selected, c, _ := handoffFixture(t)
			h, e := prepareInstalledHandoff(context.Background(), selected, c, func(p string) error {
				if p == "init-parent-sync" {
					return errHandoff
				}
				return nil
			})
			defer h.Close()
			if !errors.Is(e, errHandoffUncertain) {
				t.Fatal("interruption")
			}
			if e = h.Close(); e != nil {
				t.Fatal(e)
			}
			c.Run = strings.Repeat("d", 32)
			fenced := false
			generation := false
			next, e := prepareInstalledHandoff(context.Background(), selected, c, func(p string) error {
				if p == "reuse-parent-sync" && fail {
					return errHandoff
				}
				if p == "reuse-parent-synced" {
					fenced = true
				}
				if p == "generation-create" {
					generation = true
					if !fenced {
						t.Error("generation before fence")
					}
				}
				return nil
			})
			defer next.Close()
			if fail {
				if !errors.Is(e, errHandoffUncertain) || generation {
					t.Error("failed fence continued")
				}
			} else if e != nil || !fenced || !generation {
				t.Error("missing fence")
			}
		})
	}
}

func TestInstalledHandoffRepairPreopen(t *testing.T) {
	_, _, root := handoffFixture(t)
	if e := unix.Mkfifo(root+"/fifo", 0600); e != nil {
		t.Fatal(e)
	}
	fd, e := unix.Open(root, handoffFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fd)
	opened := false
	child, _, e := handoffChildObserved(fd, "fifo", false, func() { opened = true })
	if child >= 0 {
		unix.Close(child)
	}
	if e == nil || opened {
		t.Fatal("unsafe open attempted")
	}
	_, _, e = handoffChildObserved(fd, "absent", false, func() { opened = true })
	if !errors.Is(e, unix.ENOENT) || opened {
		t.Fatal("absence classification")
	}
}

func TestInstalledHandoffRepairReaderBoundary(t *testing.T) {
	for _, kind := range []string{"cancel", "binding", "validated-cancel", "validated-binding"} {
		t.Run(kind, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			h := handoffPrepared(t, selected, c)
			if _, e := h.PublishDiscovery(context.Background()); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			closed := false
			snap, e := readInstalledHandoff(ctx, selected, func(p string, fds []int) {
				if p == "closed" {
					closed = true
					for _, fd := range fds {
						if _, e := handoffStat(fd); !errors.Is(e, unix.EBADF) {
							t.Error("reader descriptor not closed")
						}
					}
				}
				stage := "closed"
				if strings.HasPrefix(kind, "validated-") {
					stage = "validated"
				}
				if p != stage {
					return
				}
				if strings.HasSuffix(kind, "cancel") {
					cancel()
					return
				}
				if e := os.Rename(root, root+"-old"); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(root, 0700); e != nil {
					t.Fatal(e)
				}
			})
			if !closed || e == nil || snap.SocketPath() != "" || snap.Credentials() != (InstalledCredentials{}) {
				t.Fatal("usable result")
			}
		})
	}
}

// A nil waiterDone means failure occurred before the waiter was started.
func handoffRepairJoin(cancel context.CancelFunc, release func(), holderDone, waiterDone <-chan struct{}) {
	cancel()
	release()
	<-holderDone
	if waiterDone != nil {
		<-waiterDone
	}
}

func TestInstalledHandoffRepairTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{})
	holderDone := make(chan struct{})
	waiterDone := make(chan struct{})
	teardownDone := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	go func() {
		defer close(holderDone)
		<-released
	}()
	go func() {
		defer close(waiterDone)
		<-ctx.Done()
		// Intentionally ignore cancellation until the holder exits.
		<-holderDone
	}()
	// Independent of the helper: rescue a join-before-release regression,
	// then join every goroutine, including the helper invocation itself.
	defer func() {
		cancel()
		release()
		<-holderDone
		<-waiterDone
		<-teardownDone
	}()
	go func() {
		defer close(teardownDone)
		handoffRepairJoin(cancel, func() {
			if ctx.Err() == nil {
				t.Error("release before cancellation")
			}
			release()
		}, holderDone, waiterDone)
	}()
	select {
	case <-teardownDone:
		select {
		case <-holderDone:
		default:
			t.Error("holder not joined")
		}
		select {
		case <-waiterDone:
		default:
			t.Error("waiter not joined")
		}
	case <-time.After(time.Second):
		t.Fatal("teardown blocked on release-dependent waiter")
	}
}

func TestInstalledHandoffRepairQueueAndAck(t *testing.T) {
	selected, c, _ := handoffFixture(t)
	h := handoffPrepared(t, selected, c)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	h.state.rename = func(fd int, from, to string, exclusive bool) error {
		e := handoffExclusive(fd, from, to)
		close(entered)
		<-release
		return e
	}
	ctx, cancel := context.WithCancel(context.Background())
	var waiterDone chan struct{}
	defer func() { handoffRepairJoin(cancel, func() { close(release) }, done, waiterDone) }()
	go func() { defer close(done); h.PublishDiscovery(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("rename entry")
	}
	queued := make(chan struct{})
	h.state.queued = func() { close(queued) }
	result := make(chan InstalledHandoffOutcome, 1)
	waiterDone = make(chan struct{})
	go func() {
		defer close(waiterDone)
		out, _ := h.PublishDiscovery(ctx)
		result <- out
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("queue entry")
	}
	cancel()
	select {
	case out := <-result:
		if out != InstalledHandoffUncertain {
			t.Error("in-flight effect lost")
		}
	case <-time.After(time.Second):
		t.Fatal("queue cancellation blocked")
	}
	select {
	case <-waiterDone:
	case <-time.After(time.Second):
		t.Fatal("queue waiter did not exit while holder held")
	}
}

func TestInstalledHandoffRepairAmbiguousReplacement(t *testing.T) {
	for _, effect := range []string{"ack-error", "no-effect", "foreign-destination"} {
		t.Run(effect, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			old := handoffPrepared(t, selected, c)
			if _, e := old.PublishDiscovery(context.Background()); e != nil {
				t.Fatal(e)
			}
			before, e := os.ReadFile(root + "/ic/current")
			if e != nil {
				t.Fatal(e)
			}
			c.Run = strings.Repeat("d", 32)
			h := handoffPrepared(t, selected, c)
			h.state.rename = func(fd int, from, to string, exclusive bool) error {
				if exclusive {
					t.Error("unexpected exclusive replacement")
				}
				if effect != "no-effect" {
					if e := unix.Renameat(fd, from, fd, to); e != nil {
						return e
					}
				}
				return unix.EIO
			}
			if out, e := h.PublishDiscovery(context.Background()); out != InstalledHandoffUncertain || !errors.Is(e, errHandoffUncertain) {
				t.Fatal("lost ambiguity")
			}
			if effect == "foreign-destination" {
				if e := os.Rename(root+"/ic/current", root+"/ic/captured"); e != nil {
					t.Fatal(e)
				}
				handoffPut(t, root+"/ic/current", before)
			}
			ce := h.Close()
			if effect == "foreign-destination" {
				if ce == nil {
					t.Error("missing cleanup refusal")
				}
			} else if ce != nil {
				t.Error(ce)
			}
			if effect != "ack-error" {
				after, e := os.ReadFile(root + "/ic/current")
				if e != nil || string(after) != string(before) {
					t.Error("old/foreign discovery changed")
				}
			}
			if out, _ := h.PublishDiscovery(context.Background()); out != InstalledHandoffUncertain {
				t.Error("cleanup erased uncertainty")
			}
		})
	}
}

func TestInstalledHandoffRepairSticky(t *testing.T) {
	for _, first := range []string{"published", "uncertain"} {
		for _, refusal := range []string{"cancel", "drift", "closed"} {
			t.Run(first+"/"+refusal, func(t *testing.T) {
				selected, c, root := handoffFixture(t)
				h := handoffPrepared(t, selected, c)
				if first == "uncertain" {
					h.state.fault = func(p string) error {
						if p == "current-directory-sync" {
							return errHandoff
						}
						return nil
					}
				}
				out, _ := h.PublishDiscovery(context.Background())
				if out == InstalledHandoffUnpublished {
					t.Fatal("initial effect")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch refusal {
				case "cancel":
					cancel()
				case "closed":
					h.Close()
				case "drift":
					if e := os.Rename(root, root+"-old"); e != nil {
						t.Fatal(e)
					}
				}
				if out, e := h.PublishDiscovery(ctx); e == nil || out != InstalledHandoffUncertain {
					t.Fatal("lost sticky effect")
				}
			})
		}
	}
}
