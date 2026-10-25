package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestHeartbeatUsesEncryptionAndRejectsReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		hub := NewHub(&config.Config{}, logger)
		defer hub.CloseAll()
		key := bytes.Repeat([]byte{7}, 32)
		peer, err := newE2EESession(key, key, e2eeClientDirection, e2eeServerDirection)
		if err != nil {
			t.Fatal(err)
		}
		secure, err := newE2EESession(key, key, e2eeServerDirection, e2eeClientDirection)
		if err != nil {
			t.Fatal(err)
		}
		outbound := make(chan []byte, 8)
		conn := newPipeConn(CodecJSON, TransportWebSocket, 8, func(_ context.Context, frame []byte) error {
			outbound <- frame
			return nil
		}, nil)
		defer conn.CloseNow()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &ClientConn{id: "synthetic", conn: conn, secure: secure, ctx: ctx, cancel: cancel,
			logger: logger, done: make(chan struct{}), buf: newSendBuffer(clientOutboundMaxItems, clientOutboundMaxBytes)}
		handled := make(chan map[string]any, 8)
		hub.SetHandler(func(_ *ClientConn, message map[string]any, admitted func()) {
			admitted()
			handled <- message
		})
		go hub.writePump(client)
		readDone := make(chan struct{})
		go func() {
			hub.readPump(client)
			hub.removeClient(client)
			close(readDone)
		}()
		ping, err := peer.seal([]byte(`{"type":"ping"}`))
		if err != nil || !conn.Deliver(ping) {
			t.Fatalf("fixture ping: %v", err)
		}
		synctest.Wait()
		if len(outbound) != 1 || len(handled) != 0 {
			t.Fatal("heartbeat entered the application queue or was not answered")
		}
		sealed := <-outbound
		if bytes.Contains(sealed, []byte("pong")) {
			t.Fatal("heartbeat was sent in plaintext")
		}
		pong, err := peer.open(sealed)
		if err != nil || string(pong) != `{"type":"pong"}` {
			t.Fatalf("encrypted heartbeat = %s, error = %v", pong, err)
		}
		other, err := peer.seal([]byte(`{"type":"ping","text":"not a heartbeat"}`))
		if err != nil || !conn.Deliver(other) {
			t.Fatalf("fixture application frame: %v", err)
		}
		synctest.Wait()
		if len(handled) != 1 || len(outbound) != 0 {
			t.Fatal("a non-heartbeat bypassed application dispatch")
		}
		conn.Deliver(ping)
		synctest.Wait()
		select {
		case <-readDone:
		default:
			t.Fatal("replayed heartbeat was not rejected")
		}
		if len(outbound) != 0 || len(handled) != 1 {
			t.Fatal("replayed heartbeat caused work")
		}
		<-client.done
	})
}

func TestHeartbeatDoesNotWaitForSlowHandlers(t *testing.T) {
	for _, name := range []string{TransportWebSocket, TransportGateway, TransportWebRTC} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				defer hub.CloseAll()
				outbound := make(chan []byte, 8)
				conn := newPipeConn(CodecJSON, name, 64, func(_ context.Context, frame []byte) error {
					outbound <- frame
					return nil
				}, nil)
				defer conn.CloseNow()
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				started := make(chan struct{}, handlerCapacity)
				handled := make(chan string, 8)
				hub.SetHandler(func(client *ClientConn, message map[string]any, admitted func()) {
					admitted()
					if message["type"] == "get_conversation_history" {
						started <- struct{}{}
						<-release
						return
					}
					if message["type"] == "refresh_agents" {
						hub.Send(client, map[string]any{"type": "inventory_status", "state": "ready"})
					}
					handled <- message["type"].(string)
				})
				go hub.Serve(context.Background(), conn)
				for range handlerCapacity {
					if !conn.Deliver([]byte(`{"type":"get_conversation_history"}`)) {
						t.Fatal("fixture ingress full")
					}
				}
				for range handlerCapacity {
					<-started
				}
				for _, frame := range []string{`{"type":"refresh_agents"}`, `{"type":"send_input","text":"once"}`, `{"type":"ping"}`} {
					if !conn.Deliver([]byte(frame)) {
						t.Fatal("fixture probe was not delivered")
					}
				}
				synctest.Wait()
				select {
				case frame := <-outbound:
					var message map[string]any
					if err := json.Unmarshal(frame, &message); err != nil || message["type"] != "pong" || len(message) != 1 {
						t.Fatalf("heartbeat response = %s, error = %v", frame, err)
					}
				default:
					t.Fatal("a responsive connection could not answer its heartbeat while 32 history handlers were blocked")
				}
				if len(handled) != 0 {
					t.Fatal("application work bypassed handler capacity")
				}
				unblock()
				synctest.Wait()
				if len(handled) != 2 {
					t.Fatal("queued work was lost or replayed")
				}
				counts := map[string]int{}
				counts[<-handled]++
				counts[<-handled]++
				if counts["refresh_agents"] != 1 || counts["send_input"] != 1 {
					t.Fatal("queued work was lost or replayed")
				}
			})
		})
	}
}
