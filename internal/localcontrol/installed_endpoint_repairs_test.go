//go:build linux || darwin

package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func differentInstalledHex(s string) string {
	first := byte('0')
	if s[0] == first {
		first = '1'
	}
	return string(first) + s[1:]
}

func TestInstalledEndpointEntryRetiresAndDrains(t *testing.T) {
	for _, name := range []string{"rejected", "cancelled", "nil-context"} {
		t.Run(name, func(t *testing.T) {
			entered, released, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release sync.Once
			core, request, calls := installedFixture(t, func(b *InstalledBackend) {
				b.Status = func(ctx context.Context) (InstalledObservation, error) {
					close(entered)
					<-ctx.Done()
					<-released
					close(exited)
					return InstalledObservation{}, ctx.Err()
				}
			})
			alias := core
			data, _ := json.Marshal(request)
			dispatched := make(chan struct{})
			go func() { defer close(dispatched); alias.Dispatch(context.Background(), data) }()
			returned := make(chan struct{})
			started := false
			defer func() {
				core.state.cancel()
				release.Do(func() { close(released) })
				<-dispatched
				if started {
					<-returned
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("callback not entered")
			}
			ctx := context.Background()
			if name == "nil-context" {
				ctx = nil
			}
			if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			started = true
			acquired := false
			go func() {
				defer close(returned)
				_ = installedEndpointEntry(ctx, config.InstalledConfig{}, core, InstalledCredentials{}, func(context.Context, config.InstalledConfig, InstalledControl, InstalledCredentials) error {
					acquired = true
					return errInstalledEndpoint
				})
			}()
			select {
			case <-core.state.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("core not cancelled")
			}
			select {
			case <-returned:
				t.Fatal("returned before callback drain")
			default:
			}
			release.Do(func() { close(released) })
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("endpoint not drained")
			}
			<-dispatched
			if acquired {
				t.Fatal("acquisition attempted")
			}
			select {
			case <-exited:
			default:
				t.Fatal("callback not joined")
			}
			if got := installedCall(t, alias, context.Background(), request); got.Code != "retired" {
				t.Fatal("alias dispatch accepted")
			}
			if *calls != [5]int{} || len(core.state.slots) != 0 {
				t.Fatal("callback or queue escaped")
			}
		})
	}
}

// Close deliberately reports uncertainty without releasing the pipe. Independent
// rescue below closes its underlying ends before joining, including on failures.
type installedFaultClose struct {
	net.Conn
	calls  atomic.Int32
	closed chan struct{}
	err    error
	once   sync.Once
}

func (c *installedFaultClose) Close() error {
	c.calls.Add(1)
	c.once.Do(func() { close(c.closed) })
	if c.err != nil {
		return c.err
	}
	return c.Conn.Close()
}

func TestInstalledEndpointCloseAccounting(t *testing.T) {
	for _, name := range []string{"active", "peer-refused", "capacity-refused", "normal-cancel", "already-closed", "joined-closed-unknown"} {
		t.Run(name, func(t *testing.T) {
			callback := make(chan struct{})
			core, request, _ := installedFixture(t, func(b *InstalledBackend) {
				b.Status = func(ctx context.Context) (InstalledObservation, error) {
					close(callback)
					<-ctx.Done()
					return InstalledObservation{}, ctx.Err()
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			l := &installedMemoryListener{incoming: make(chan net.Conn), closed: make(chan struct{})}
			entered := make(chan struct{}, installedHandlers+1)
			holders := make(chan struct{})
			var release sync.Once
			var peerEntries atomic.Int32
			done := make(chan struct{})
			var result error
			var peers []net.Conn
			go func() {
				defer close(done)
				result = installedAcceptIO(ctx, l, core, l.Close, func(net.Conn) error {
					peerEntries.Add(1)
					entered <- struct{}{}
					if name == "capacity-refused" {
						<-holders
						return errInstalledEndpoint
					}
					if name == "peer-refused" {
						return errInstalledEndpoint
					}
					return nil
				})
			}()
			defer func() {
				cancel()
				release.Do(func() { close(holders) })
				l.Close()
				for _, p := range peers {
					p.Close()
				}
				<-done
			}()
			send := func(c net.Conn) {
				t.Helper()
				select {
				case l.incoming <- c:
				case <-time.After(time.Second):
					t.Fatal("accept not entered")
				}
			}
			if name == "capacity-refused" {
				for n := 0; n < installedHandlers; n++ {
					a, b := net.Pipe()
					peers = append(peers, a, b)
					send(a)
					select {
					case <-entered:
					case <-time.After(time.Second):
						t.Fatal("holder not entered")
					}
				}
			}
			a, b := net.Pipe()
			peers = append(peers, a, b)
			fault := &installedFaultClose{Conn: a, closed: make(chan struct{}), err: errors.New("private close failure")}
			if name == "joined-closed-unknown" {
				fault.err = fmt.Errorf("private wrapper: %w", errors.Join(net.ErrClosed, fault.err))
			}
			if name == "normal-cancel" {
				fault.err = nil
			}
			if name == "already-closed" {
				fault.err = net.ErrClosed
				a.Close()
			}
			send(fault)
			if name != "capacity-refused" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("peer not entered")
				}
			}
			if name == "active" {
				data, _ := json.Marshal(request)
				if installedWriteFrame(b, data, MaxRequestBytes) != nil {
					t.Fatal("request write")
				}
				select {
				case <-callback:
				case <-time.After(time.Second):
					t.Fatal("callback not entered")
				}
			}
			if name != "capacity-refused" {
				cancel()
			}
			select {
			case <-fault.closed:
			case <-time.After(time.Second):
				t.Fatal("close not attempted")
			}
			if name == "capacity-refused" {
				if ctx.Err() != nil || peerEntries.Load() != installedHandlers {
					t.Fatal("fault admitted or holders cancelled before refusal")
				}
				select {
				case <-callback:
					t.Fatal("handler callback entered")
				default:
				}
			}
			cancel()
			release.Do(func() { close(holders) })
			// Always rescue independently, then join the complete accept engine.
			for _, p := range peers {
				p.Close()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("workers not joined")
			}
			wantFailure := name != "normal-cancel" && name != "already-closed"
			if errors.Is(result, errInstalledCleanup) != wantFailure || fault.calls.Load() != 1 {
				t.Fatal("close accounting")
			}
		})
	}
}

func TestInstalledEndpointClosedOnly(t *testing.T) {
	unknown := errors.New("private cleanup detail")
	for _, tc := range []struct {
		name   string
		err    error
		benign bool
	}{
		{"net-closed", net.ErrClosed, true},
		{"os-closed", os.ErrClosed, true},
		{"wrapped-closed", fmt.Errorf("private: %w", net.ErrClosed), true},
		{"joined-closed", errors.Join(net.ErrClosed, os.ErrClosed), true},
		{"wrapped-joined-closed", fmt.Errorf("private: %w", errors.Join(net.ErrClosed, fmt.Errorf("nested: %w", os.ErrClosed))), true},
		{"unknown", unknown, false},
		{"joined-closed-unknown", errors.Join(net.ErrClosed, unknown), false},
		{"wrapped-joined-unknown", fmt.Errorf("private: %w", errors.Join(fmt.Errorf("nested: %w", os.ErrClosed), unknown)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault := &installedFaultClose{closed: make(chan struct{}), err: tc.err}
			owned := &installedOwnedConn{Conn: fault}
			got := owned.Close()
			if tc.benign {
				if got != nil {
					t.Fatal("closed-only error retained")
				}
			} else if got != errInstalledCleanup {
				t.Fatal("uncertainty lost or not redacted")
			}
			if owned.Close() != got || fault.calls.Load() != 1 {
				t.Fatal("close result not retained")
			}
		})
	}
}

func TestInstalledEndpointCloseOnce(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	fault := &installedFaultClose{Conn: a, closed: make(chan struct{}), err: errors.New("private")}
	owned := &installedOwnedConn{Conn: fault}
	var workers sync.WaitGroup
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() { defer workers.Done(); _ = owned.Close() }()
	}
	workers.Wait()
	fault.err = nil
	if !errors.Is(owned.Close(), errInstalledCleanup) || fault.calls.Load() != 1 {
		t.Fatal("first uncertainty erased")
	}
}
