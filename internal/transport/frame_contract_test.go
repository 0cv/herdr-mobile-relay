package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"testing/synctest"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

type frameFixture struct {
	hub      *Hub
	conn     *pipeConn
	peer     *e2eeSession
	outbound chan []byte
	handled  chan map[string]any
	readDone chan struct{}
}

// newFrameFixture connects one client to a hub through its real read and
// write pumps. The handler records every message it receives. With hold set it
// keeps the first message unadmitted until the client goes, which stops the
// ordered ingress there.
func newFrameFixture(t *testing.T, secure, hold bool) *frameFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(&config.Config{}, logger)
	t.Cleanup(hub.CloseAll)
	f := &frameFixture{
		hub: hub, outbound: make(chan []byte, 256), handled: make(chan map[string]any, 256),
		readDone: make(chan struct{}),
	}
	var serverSession *e2eeSession
	if secure {
		key := bytes.Repeat([]byte{7}, 32)
		var err error
		if f.peer, err = newE2EESession(key, key, e2eeClientDirection, e2eeServerDirection); err != nil {
			t.Fatal(err)
		}
		if serverSession, err = newE2EESession(key, key, e2eeServerDirection, e2eeClientDirection); err != nil {
			t.Fatal(err)
		}
	}
	f.conn = newPipeConn(CodecJSON, TransportWebSocket, 8, func(_ context.Context, frame []byte) error {
		f.outbound <- frame
		return nil
	}, nil)
	t.Cleanup(f.conn.CloseNow)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := &ClientConn{id: "synthetic", conn: f.conn, secure: serverSession, ctx: ctx, cancel: cancel,
		logger: logger, done: make(chan struct{}), buf: newSendBuffer(clientOutboundMaxItems, clientOutboundMaxBytes)}
	hub.SetHandler(func(client *ClientConn, message map[string]any, admitted func()) {
		f.handled <- message
		if hold {
			<-client.ctx.Done()
			return
		}
		admitted()
	})
	go hub.writePump(client)
	go func() {
		hub.readPump(client)
		hub.removeClient(client)
		close(f.readDone)
	}()
	return f
}

func (f *frameFixture) deliver(t *testing.T, frame []byte) {
	t.Helper()
	if f.peer != nil {
		sealed, err := f.peer.seal(frame)
		if err != nil {
			t.Fatal(err)
		}
		frame = sealed
	}
	if !f.conn.Deliver(frame) {
		t.Fatal("fixture frame was not delivered")
	}
	synctest.Wait()
}

func (f *frameFixture) open(t *testing.T) map[string]any {
	t.Helper()
	select {
	case frame := <-f.outbound:
		if f.peer != nil {
			plain, err := f.peer.open(frame)
			if err != nil {
				t.Fatal(err)
			}
			frame = plain
		}
		var message map[string]any
		if err := json.Unmarshal(frame, &message); err != nil {
			t.Fatalf("outbound frame %s: %v", frame, err)
		}
		return message
	default:
		t.Fatal("the relay sent nothing")
		return nil
	}
}

func (f *frameFixture) closed() bool {
	select {
	case <-f.readDone:
		return true
	default:
		return false
	}
}

func TestFrameContractRefusesFramesThatAreNotJSONObjects(t *testing.T) {
	frames := map[string][]byte{
		"not JSON":           []byte(`{`),
		"plain text":         []byte(`not json`),
		"an array":           []byte(`[{"type":"refresh_agents"}]`),
		"an empty array":     []byte(`[]`),
		"null":               []byte(`null`),
		"a string":           []byte(`"refresh_agents"`),
		"a number":           []byte(`5`),
		"invalid UTF-8":      []byte("{\"type\":\"refresh_agents\",\"text\":\"\xff\"}"),
		"invalid UTF-8 name": []byte("{\"type\":\"refresh_agents\",\"\xff\":1}"),
	}
	for name, frame := range frames {
		t.Run(name+" on a plain connection", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFrameFixture(t, false, false)
				f.deliver(t, frame)
				if len(f.handled) != 0 {
					t.Fatalf("the frame reached the handler: %v", <-f.handled)
				}
				if f.closed() {
					t.Fatal("a plain connection was closed by a refused frame")
				}
				f.deliver(t, []byte(`{"type":"refresh_agents","request_id":"after"}`))
				if len(f.handled) != 1 || (<-f.handled)["request_id"] != "after" {
					t.Fatal("the connection no longer delivers frames after a refused one")
				}
			})
		})
		t.Run(name+" on an encrypted connection", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFrameFixture(t, true, false)
				f.deliver(t, frame)
				if len(f.handled) != 0 {
					t.Fatalf("the frame reached the handler: %v", <-f.handled)
				}
				if !f.closed() {
					t.Fatal("an encrypted connection stayed open after a refused frame")
				}
			})
		})
	}
}

func TestFrameContractPassesObjectsWithoutReadingThem(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", secure), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFrameFixture(t, secure, false)
				for _, frame := range []string{
					`{}`,
					`{"type":5}`,
					`{"request_id":7,"type":"read_pane"}`,
					`{"type":"read_pane","lines":"many"}`,
					`  {"type":"refresh_agents"}  `,
				} {
					f.deliver(t, []byte(frame))
					if len(f.handled) != 1 {
						t.Fatalf("frame %s did not reach the handler", frame)
					}
					<-f.handled
				}
				if f.closed() {
					t.Fatal("the connection closed on a JSON object")
				}
			})
		})
	}
}

func TestFrameContractAnswersOnlyABarePing(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", secure), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFrameFixture(t, secure, false)
				f.deliver(t, []byte(`{"type":"ping"}`))
				if pong := f.open(t); pong["type"] != "pong" || len(pong) != 1 {
					t.Fatalf("heartbeat reply = %v", pong)
				}
				if len(f.handled) != 0 {
					t.Fatal("a heartbeat reached the handler")
				}
				for _, frame := range []string{
					`{"type":"ping","text":"x"}`,
					`{"type":"ping","request_id":"r"}`,
					`{"TYPE":"ping"}`,
					`{"type":"Ping"}`,
					`{"type":"ping ","request_id":"r"}`,
				} {
					f.deliver(t, []byte(frame))
					if len(f.handled) != 1 || len(f.outbound) != 0 {
						t.Fatalf("frame %s: handled %d, replies %d; want it handled without a reply", frame, len(f.handled), len(f.outbound))
					}
					<-f.handled
				}
			})
		})
	}
}

func TestFrameContractRepliesToACommandTheFullIngressRefuses(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", secure), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFrameFixture(t, secure, true)
				for index := range 1 + orderedIngressCapacity {
					f.deliver(t, fmt.Appendf(nil, `{"type":"send_keys","request_id":"queued-%d"}`, index))
				}
				if len(f.outbound) != 0 {
					t.Fatalf("the ingress refused a frame before it was full: %v", f.open(t))
				}
				f.deliver(t, []byte(`{"type":"send_keys","request_id":"overflow","pane_id":"pane-1"}`))
				refusal := f.open(t)
				if refusal["type"] != "command_result" || refusal["request_id"] != "overflow" || refusal["action"] != "send_keys" ||
					refusal["ok"] != false || refusal["phase"] != "not_started" || refusal["error"] != "Relay is busy; command was not sent" {
					t.Fatalf("refusal = %v", refusal)
				}
				f.deliver(t, []byte(`{"type":5,"request_id":9}`))
				if blank := f.open(t); blank["request_id"] != "" || blank["action"] != "" || blank["phase"] != "not_started" {
					t.Fatalf("refusal of a frame with wrong-typed fields = %v", blank)
				}
				if len(f.handled) != 1 {
					t.Fatalf("handler saw %d frames, want only the first", len(f.handled))
				}
			})
		})
	}
}
