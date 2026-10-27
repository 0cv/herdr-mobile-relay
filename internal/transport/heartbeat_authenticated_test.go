package transport

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestAuthenticatedHeartbeatWhileHandlersSaturated(t *testing.T) {
	for _, name := range []string{TransportWebSocket, TransportGateway, TransportWebRTC} {
		for _, invalid := range []string{"replay", "tamper"} {
			t.Run(name+"/"+invalid, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					const token = "0123456789abcdef0123456789abcdef"
					hub := NewHub(&config.Config{Token: token}, slog.New(slog.NewTextHandler(io.Discard, nil)))
					hub.SetE2EEAuthResolver(fixedE2EEAuthResolver{secret: []byte(token)})
					defer hub.CloseAll()
					codec := CodecBinary
					if name == TransportWebSocket {
						codec = CodecJSON
					}
					var peer *pipeConn
					conn := newPipeConn(codec, name, 64, func(_ context.Context, frame []byte) error {
						if !peer.Deliver(frame) {
							return ErrFrameConnClosed
						}
						return nil
					}, nil)
					peer = newPipeConn(codec, name, 64, func(_ context.Context, frame []byte) error {
						if !conn.Deliver(frame) {
							return ErrFrameConnClosed
						}
						return nil
					}, nil)
					defer peer.CloseNow()
					defer conn.CloseNow()
					release := make(chan struct{})
					unblock := sync.OnceFunc(func() { close(release) })
					defer unblock()
					registered := make(chan *ClientConn, 1)
					hub.SetOnConnect(func(client *ClientConn) { registered <- client })
					started := make(chan struct{}, handlerCapacity)
					handled := make(chan string, 8)
					hub.SetHandler(func(client *ClientConn, message map[string]any, admitted func()) {
						identity, authenticated := client.Identity()
						if !authenticated || identity.Role != "controller" || identity.DeviceID != "device-test" {
							t.Error("application handler did not retain the authenticated controller identity")
						}
						defer admitted()
						if message["type"] == "get_conversation_history" {
							admitted()
							started <- struct{}{}
							<-release
							return
						}
						handled <- message["type"].(string)
					})
					done := make(chan struct{})
					go func() { hub.Serve(t.Context(), conn); close(done) }()
					session, _, _ := testClientFrameE2EEHandshake(t, t.Context(), peer, token, func() {
						synctest.Wait()
						if len(registered) != 0 || len(handled) != 0 {
							t.Fatal("connection was admitted before the authenticated client finish")
						}
					})
					synctest.Wait()
					if len(registered) != 1 {
						t.Fatal("authenticated connection did not register")
					}
					seal := func(text string) []byte {
						t.Helper()
						frame, err := session.seal([]byte(text))
						if err != nil {
							t.Fatal(err)
						}
						return frame
					}
					send := func(frame []byte) {
						t.Helper()
						if err := peer.WriteFrame(t.Context(), frame); err != nil {
							t.Fatal(err)
						}
					}
					for range handlerCapacity {
						send(seal(`{"type":"get_conversation_history"}`))
					}
					for range handlerCapacity {
						<-started
					}
					for _, text := range []string{`{"type":"refresh_agents"}`, `{"type":"send_input","text":"once"}`, `{"type":"ping","text":"application"}`} {
						send(seal(text))
					}
					ping := seal(`{"type":"ping"}`)
					send(ping)
					synctest.Wait()
					if len(peer.inbound) != 1 || len(handled) != 0 {
						t.Fatal("encrypted heartbeat was blocked or application work bypassed saturation")
					}
					pong, err := peer.ReadFrame(t.Context())
					if err != nil || bytes.Contains(pong, []byte("pong")) {
						t.Fatal("heartbeat was not encrypted")
					}
					plaintext, err := session.open(pong)
					if err != nil || string(plaintext) != `{"type":"pong"}` {
						t.Fatal("heartbeat failed authentication")
					}
					unblock()
					synctest.Wait()
					if len(handled) != 3 {
						t.Fatal("queued application work was lost or duplicated")
					}
					if got := []string{<-handled, <-handled, <-handled}; !reflect.DeepEqual(got, []string{"refresh_agents", "send_input", "ping"}) {
						t.Fatalf("queued work changed order: %v", got)
					}
					bad := ping
					if invalid == "tamper" {
						bad = seal(`{"type":"ping"}`)
						bad[len(bad)-3] ^= 1
					}
					send(bad)
					synctest.Wait()
					select {
					case <-done:
					default:
						t.Fatal("invalid authenticated frame did not close the connection")
					}
					if len(peer.inbound) != 0 || len(handled) != 0 {
						t.Fatal("invalid frame caused a pong or application replay")
					}
				})
			})
		}
	}
}
