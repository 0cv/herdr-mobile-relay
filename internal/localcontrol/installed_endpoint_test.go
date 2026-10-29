//go:build linux || darwin

package localcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// All callbacks below are synthetic in-memory evidence, NOT ownership/readiness.
func TestInstalledEndpointFrames(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		limit      int
		ok         bool
	}{
		{"exact", "{}\n", 2, true}, {"oversize", "{} \n", 2, false}, {"missing-delimiter", "{}", 2, false}, {"empty", "\n", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := installedReadFrame(bufio.NewReaderSize(strings.NewReader(tc.data), 16), tc.limit)
			if (e == nil) != tc.ok {
				t.Fatal("frame decision")
			}
		})
	}
}
func TestInstalledEndpointAuthentication(t *testing.T) {
	for _, field := range []string{"protocol", "run", "installation", "capability"} {
		t.Run(field, func(t *testing.T) {
			core, r, calls := installedFixture(t, nil)
			switch field {
			case "protocol":
				r.Protocol = "wrong"
			case "run":
				r.Run = "wrong"
			case "installation":
				r.Installation = "wrong"
			case "capability":
				r.Capability = "wrong"
			}
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); installedServeConnection(ctx, a, core) }()
			defer func() { cancel(); a.Close(); b.Close(); <-done }()
			data, _ := json.Marshal(r)
			if installedWriteFrame(b, data, MaxRequestBytes) != nil {
				t.Fatal("write")
			}
			b.SetReadDeadline(time.Now().Add(time.Second))
			frame, e := installedReadFrame(bufio.NewReader(b), 8192)
			if e != nil || !strings.Contains(string(frame), "unauthorized") {
				t.Fatal("authentication")
			}
			cancel()
			b.Close()
			<-done
			if *calls != [5]int{} {
				t.Fatal("callback before authentication")
			}
		})
	}
}
func TestInstalledEndpointRequestCancellation(t *testing.T) {
	for _, mode := range []string{"disconnect", "extra-byte", "root-cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			exited := make(chan struct{})
			core, r, _ := installedFixture(t, func(b *InstalledBackend) {
				b.Status = func(ctx context.Context) (InstalledObservation, error) {
					close(entered)
					<-ctx.Done()
					close(exited)
					return InstalledObservation{}, ctx.Err()
				}
			})
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); installedServeConnection(ctx, a, core) }()
			defer func() { cancel(); a.Close(); b.Close(); <-done }()
			data, _ := json.Marshal(r)
			if installedWriteFrame(b, data, MaxRequestBytes) != nil {
				t.Fatal("write")
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("no callback entry")
			}
			switch mode {
			case "disconnect":
				b.Close()
			case "extra-byte":
				b.SetWriteDeadline(time.Now().Add(time.Second))
				b.Write([]byte("x"))
			case "root-cancel":
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler not joined")
			}
			<-exited
			if core.state.ctx.Err() != nil {
				t.Fatal("request retired core")
			}
		})
	}
}

type installedObservedWrite struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *installedObservedWrite) Write(b []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(b)
}

func TestInstalledEndpointBlockedOutputJoin(t *testing.T) {
	entered := make(chan struct{})
	writing := make(chan struct{})
	core, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Reprint = func(context.Context) (string, error) { close(entered); return "synthetic-secret", nil }
	})
	r.Op = "reprint"
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		installedServeConnection(ctx, &installedObservedWrite{Conn: a, entered: writing}, core)
	}()
	defer func() { cancel(); a.Close(); b.Close(); <-done }()
	data, _ := json.Marshal(r)
	if installedWriteFrame(b, data, MaxRequestBytes) != nil {
		t.Fatal("write")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no secret callback")
	}
	select {
	case <-writing:
	case <-time.After(time.Second):
		t.Fatal("output phase not entered")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("output not joined")
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	var out [100]byte
	if n, _ := b.Read(out[:]); n != 0 {
		t.Fatal("secret released after cancellation")
	}
}
func TestInstalledClientPinsAndRedaction(t *testing.T) {
	_, r, _ := installedFixture(t, nil)
	c := InstalledCredentials{r.Run, r.Installation, r.Capability}
	for _, tc := range []struct {
		name string
		i    InstalledIntent
		ok   bool
	}{
		{"arm-pinned", InstalledIntent{Kind: InstalledArm, ExpectedRun: c.Run, OperationID: "stable"}, true},
		{"arm-unpinned", InstalledIntent{Kind: InstalledArm, OperationID: "stable"}, false},
		{"arm-stale", InstalledIntent{Kind: InstalledArm, ExpectedRun: strings.Repeat("f", 32), OperationID: "stable"}, false},
		{"unknown", InstalledIntent{Kind: "stop"}, false},
		{"status-receipt", InstalledIntent{Kind: InstalledGetStatus, OperationID: "stable"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, e := installedIntentFrame(c, tc.i)
			if (e == nil) != tc.ok {
				t.Fatal("pin decision")
			}
			if tc.ok {
				var decoded InstalledRequest
				if installedDecode(data, &decoded) != nil || decoded.OperationID != tc.i.OperationID {
					t.Fatal("ID changed")
				}
			}
		})
	}
	secret := "not-for-formatting"
	result := InstalledClientResult{Reply: InstalledReply{Invitation: secret}}
	if strings.Contains(fmt.Sprintf("%v %#v", result, result), secret) {
		t.Fatal("secret formatting")
	}
}
func TestInstalledClientReplyCodec(t *testing.T) {
	c := InstalledCredentials{Run: strings.Repeat("a", 32), Installation: "fixture", Capability: strings.Repeat("b", 64)}
	for _, tc := range []struct {
		name, data string
		ok         bool
	}{
		{"valid", `{"code":"ok","publication":"disabled","invitation":"synthetic"}`, true},
		{"duplicate", `{"code":"ok","code":"ok","invitation":"synthetic"}`, false},
		{"case", `{"Code":"ok","invitation":"synthetic"}`, false},
		{"unknown", `{"code":"ok","invitation":"synthetic","other":1}`, false},
		{"null", `{"code":"ok","invitation":null}`, false},
		{"error-secret", `{"code":"cancelled","invitation":"synthetic"}`, false},
		{"trailing", `{"code":"ok","invitation":"synthetic"}{}`, false},
		{"wrong-shape", `{"code":"ok","status":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := installedDecodeReply([]byte(tc.data), InstalledIntent{Kind: InstalledReprint}, c)
			if (e == nil) != tc.ok {
				t.Fatal("codec decision")
			}
			if e != nil && r.Invitation != "" {
				t.Fatal("usable error secret")
			}
		})
	}
}
func TestInstalledClientLostReply(t *testing.T) {
	for _, mode := range []string{"before-send", "lost-reply", "validated"} {
		t.Run(mode, func(t *testing.T) {
			core, r, calls := installedFixture(t, nil)
			credentials := InstalledCredentials{r.Run, r.Installation, r.Capability}
			intent := InstalledIntent{Kind: InstalledArm, ExpectedRun: r.Run, OperationID: "stable"}
			data, e := installedIntentFrame(credentials, intent)
			if e != nil {
				t.Fatal("intent")
			}
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			if mode == "before-send" {
				cancel()
			}
			go func() {
				defer close(done)
				defer a.Close()
				if mode == "before-send" {
					return
				}
				reader := bufio.NewReader(a)
				frame, e := installedReadFrame(reader, MaxRequestBytes)
				if e != nil {
					return
				}
				reply := core.Dispatch(ctx, frame)
				if mode == "validated" {
					installedWriteFrame(a, reply, 8192)
				}
			}()
			defer func() { cancel(); a.Close(); b.Close(); <-done }()
			result, e := installedClientExchange(ctx, b, credentials, intent, data)
			<-done
			want := InstalledPossibleDispatch
			if mode == "before-send" {
				want = InstalledNotSent
			}
			if mode == "validated" {
				want = InstalledValidatedReply
			}
			if result.Transmission != want {
				t.Fatal("transmission classification")
			}
			if mode != "validated" && (e == nil || result.Reply.Invitation != "") {
				t.Fatal("error secret")
			}
			expected := 1
			if mode == "before-send" {
				expected = 0
			}
			if calls[3] != expected {
				t.Fatal("automatic retry or missing dispatch")
			}
		})
	}
}
