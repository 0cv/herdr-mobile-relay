//go:build linux || darwin

package localcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestInstalledEndpointQueuedDrain(t *testing.T) {
	core, r, calls := installedFixture(t, nil)
	core.state.gate <- struct{}{}
	held := true
	ctx, cancel := context.WithCancel(context.Background())
	var peers []net.Conn
	var workers []chan struct{}
	defer func() {
		cancel()
		for _, p := range peers {
			p.Close()
		}
		if held {
			<-core.state.gate
		}
		for _, done := range workers {
			<-done
		}
	}()
	data, _ := json.Marshal(r)
	for n := 0; n < 16; n++ {
		a, b := net.Pipe()
		peers = append(peers, a, b)
		done := make(chan struct{})
		workers = append(workers, done)
		go func() { defer close(done); installedServeConnection(ctx, a, core) }()
		if installedWriteFrame(b, data, MaxRequestBytes) != nil {
			t.Fatal("request write")
		}
	}
	deadline := time.Now().Add(time.Second)
	for len(core.state.slots) != 16 {
		if time.Now().After(deadline) {
			t.Fatal("queue was not entered")
		}
		time.Sleep(time.Millisecond)
	}
	a, b := net.Pipe()
	peers = append(peers, a, b)
	done := make(chan struct{})
	workers = append(workers, done)
	go func() { defer close(done); installedServeConnection(ctx, a, core) }()
	if installedWriteFrame(b, data, MaxRequestBytes) != nil {
		t.Fatal("excess write")
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	frame, e := installedReadFrame(bufio.NewReader(b), 8192)
	if e != nil || string(frame) != `{"code":"busy"}` {
		t.Fatal("queue not bounded")
	}
	cancel()
	// Join while the serialization holder is STILL held, proving cancellation
	// traverses the real queue. Failure teardown releases before joining.
	for _, done := range workers {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("queued handler not drained")
		}
	}
	if len(core.state.slots) != 0 || *calls != [5]int{} {
		t.Fatal("queue escaped")
	}
	<-core.state.gate
	held = false
}
func TestInstalledEndpointAuthorityBeforeEffects(t *testing.T) {
	selected, _, root := handoffFixture(t)
	fixture, _, _ := installedFixture(t, nil)
	cfg := selected.Snapshot()
	selection := fixture.state.proposal.InstalledSelection
	selection.Origin = cfg.TailscaleCLIOrigin
	selection.RelayPort = uint16(cfg.Port)
	for _, name := range []string{"zero-core", "zero-config", "wrong-installation", "wrong-capability", "retired", "cancelled", "nil-context", "wrong-run", "wrong-origin", "wrong-port"} {
		t.Run(name, func(t *testing.T) {
			core, credentials, e := NewInstalledControl(cfg.InstanceID, selection, fixture.state.backend)
			if e != nil {
				t.Fatal("core fixture")
			}
			defer core.Retire()
			if !installedEndpointAuthority(context.Background(), selected, core, credentials) {
				t.Fatal("matching correlation refused")
			}
			ctx := context.Background()
			useCore, useConfig, useCredentials := core, selected, credentials
			switch name {
			case "zero-core":
				useCore = InstalledControl{}
			case "zero-config":
				useConfig = config.InstalledConfig{}
			case "wrong-installation":
				useCredentials.Installation = "different"
			case "wrong-capability":
				useCredentials.Capability = differentInstalledHex(credentials.Capability)
			case "retired":
				core.Retire()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-context":
				ctx = nil
			case "wrong-run":
				useCredentials.Run = differentInstalledHex(credentials.Run)
			case "wrong-origin":
				core.state.proposal.Origin = "https://different.example"
			case "wrong-port":
				core.state.proposal.RelayPort++
			}
			acquired := false
			if installedEndpointEntry(ctx, useConfig, useCore, useCredentials, func(context.Context, config.InstalledConfig, InstalledControl, InstalledCredentials) error {
				acquired = true
				return errInstalledEndpoint
			}) == nil {
				t.Fatal("authority accepted")
			}
			if acquired {
				t.Fatal("acquisition attempted")
			}
			if useCore.state != nil && useCore.state.ctx.Err() == nil {
				t.Fatal("supplied alias remains live")
			}
			entries, e := os.ReadDir(root)
			if e != nil || len(entries) != 0 {
				t.Fatal("effects before validation")
			}
		})
	}
}

// Scripted I/O, not a socket: records frame deadlines independently of operation
// budgets, and forces a partial write/error without ever exposing OS resources.
type installedScriptConn struct {
	reads                         *strings.Reader
	readDeadlines, writeDeadlines []time.Time
	writes                        int
	successWrite                  bool
}

func (c *installedScriptConn) Read(p []byte) (int, error) { return c.reads.Read(p) }
func (c *installedScriptConn) Write(p []byte) (int, error) {
	c.writes++
	if c.successWrite {
		return len(p), nil
	}
	return 0, errInstalledEndpoint
}
func (*installedScriptConn) Close() error                { return nil }
func (*installedScriptConn) LocalAddr() net.Addr         { return nil }
func (*installedScriptConn) RemoteAddr() net.Addr        { return nil }
func (*installedScriptConn) SetDeadline(time.Time) error { return nil }
func (c *installedScriptConn) SetReadDeadline(t time.Time) error {
	c.readDeadlines = append(c.readDeadlines, t)
	return nil
}
func (c *installedScriptConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadlines = append(c.writeDeadlines, t)
	return nil
}
func TestInstalledEndpointDeadlineAndWriteFailure(t *testing.T) {
	c := &installedScriptConn{reads: strings.NewReader("")}
	before := time.Now()
	if installedWriteFrame(c, []byte("{}"), 2) == nil || c.writes != 1 || len(c.writeDeadlines) != 1 {
		t.Fatal("write uncertainty")
	}
	delta := c.writeDeadlines[0].Sub(before)
	if delta < installedFrameTimeout || delta > installedFrameTimeout+time.Second || installedFrameTimeout >= ArmTimeout || installedFrameTimeout >= ActivateTimeout {
		t.Fatal("deadline class")
	}
	c.writes = 0
	if installedWriteFrame(c, []byte("{}"), 1) == nil || c.writes != 0 {
		t.Fatal("oversize transmission")
	}
}
