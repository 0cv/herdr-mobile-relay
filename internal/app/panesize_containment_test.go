package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

// Exercises the hub's shared FrameConn ingress used by gateway and direct
// transports without opening any production network or Herdr connection.
type leaseTestFrames struct {
	name    string
	in, out chan []byte
	done    chan struct{}
	once    sync.Once
}

func (f *leaseTestFrames) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case data := <-f.in:
		return data, nil
	case <-f.done:
		return nil, transport.ErrFrameConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *leaseTestFrames) WriteFrame(ctx context.Context, data []byte) error {
	select {
	case f.out <- slices.Clone(data):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *leaseTestFrames) Close(transport.CloseStatus, string) { f.CloseNow() }
func (f *leaseTestFrames) CloseNow()                           { f.once.Do(func() { close(f.done) }) }
func (f *leaseTestFrames) Codec() transport.FrameCodec         { return transport.CodecJSON }
func (f *leaseTestFrames) TransportName() string               { return f.name }

// There is no pane-size backend: the relay never advertises the lease
// capabilities, and an older app's lease request is answered on every ingress
// with a clean failure while its cleanup still succeeds.
func TestPaneSizeCommandsHaveNoBackendOnAllIngresses(t *testing.T) {
	for _, path := range []string{transport.TransportWebSocket, transport.TransportGateway, transport.TransportWebRTC} {
		t.Run(path, func(t *testing.T) {
			s := testServer()
			for range 2 { // initial and subsequent capability snapshots
				caps := s.effectiveCapabilitiesFor(herdr.ServerStatus{})
				for _, cap := range []string{"pane_size_lease", "pane_size_lease_rows"} {
					if slices.Contains(caps, cap) {
						t.Fatalf("advertised %s", cap)
					}
				}
				for _, cap := range []string{"conversation_history", "slash_commands", "semantic_input"} {
					if !slices.Contains(caps, cap) {
						t.Fatalf("lost controller capability %s", cap)
					}
				}
			}
			// Run installs the production handler; this mirrors its ordering up
			// to dispatch so the request still crosses action classification,
			// authorization, and target validation. A local connection is
			// unauthenticated and therefore a controller.
			s.hub.SetOnConnect(func(*transport.ClientConn) {})
			s.hub.SetHandler(func(client *transport.ClientConn, msg map[string]any, admitted func()) {
				defer admitted()
				inbound, err := protocol.DecodeMap(msg)
				if err != nil {
					t.Error(err)
					return
				}
				scope, known := protocol.ScopeFor(inbound)
				if !known || !protocol.Compatible(inbound) {
					t.Errorf("%s is no longer a compatible known action", inbound.Type)
					return
				}
				if authorizationErr := s.authorizeDeviceAction(client, scope.Action, inbound.DeviceID); authorizationErr != nil {
					t.Errorf("%s authorization: %#v", inbound.Type, authorizationErr)
					return
				}
				_, authenticated := client.Identity()
				if targetErr := validateExactPaneTarget(s.state, inbound, authenticated); targetErr != nil {
					t.Errorf("%s target validation: %#v", inbound.Type, targetErr)
					return
				}
				s.handlePaneSizeCommand(client, inbound)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			defer s.hub.Shutdown(context.Background())
			var send func([]byte) error
			var receive func() ([]byte, error)
			if path == transport.TransportWebSocket {
				httpServer := httptest.NewServer(http.HandlerFunc(s.hub.HandleWebSocket))
				defer httpServer.Close()
				conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				send = func(data []byte) error { return conn.Write(ctx, websocket.MessageText, data) }
				receive = func() ([]byte, error) { _, data, err := conn.Read(ctx); return data, err }
			} else {
				frames := &leaseTestFrames{name: path, in: make(chan []byte, 8), out: make(chan []byte, 8), done: make(chan struct{})}
				served := make(chan struct{})
				go func() { s.hub.Serve(ctx, frames); close(served) }()
				defer func() { frames.CloseNow(); <-served }()
				send = func(data []byte) error { frames.in <- data; return nil }
				receive = func() ([]byte, error) {
					select {
					case data := <-frames.out:
						return data, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			for i, action := range []string{"lease_pane_size", "lease_pane_size", "release_pane_size", "release_pane_size"} {
				payload, _ := json.Marshal(map[string]any{"type": action, "pane_id": "pane-1", "columns": 80, "rows": 24, "request_id": "request", "protocol": protocol.Version})
				if err := send(payload); err != nil {
					t.Fatal(err)
				}
				data, err := receive()
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]any
				if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if result["type"] != "command_result" || result["action"] != action || result["ok"] != (i >= 2) || result["pane_id"] != "pane-1" {
					t.Fatalf("result: %s", data)
				}
				if i < 2 && (result["phase"] != "failed" || result["error"] != "Resize Session is unavailable on this relay") {
					t.Fatalf("unclear failure: %s", data)
				}
				if i >= 2 && (result["phase"] != "completed" || result["error"] != "") {
					t.Fatalf("release was not a clean no-op: %s", data)
				}
			}
		})
	}
}

// Removing the backend changes the answer, not who may ask: both commands
// stay controller-only mutations, and release keeps the stale-target cleanup
// allowance it shares with unwatch_pane and cancel_speech.
func TestPaneSizeCommandsKeepAuthorizationPaths(t *testing.T) {
	for _, operation := range []string{"lease_pane_size", "release_pane_size"} {
		scope, known := protocol.ScopeFor(protocol.Inbound{Type: operation, PaneID: "pane-1"})
		if !known || scope.Action.Class != protocol.ActionMutating {
			t.Fatalf("%s classified as %#v, known=%v", operation, scope.Action, known)
		}
		reader := transport.AuthenticatedIdentity{DeviceID: "device-reader", Role: string(protocol.RoleReader)}
		if err := authorizeAuthenticatedIdentity(reader, true, scope.Action, ""); err == nil ||
			err.Code != protocol.ErrorReaderDenied || err.Args["operation"] != operation {
			t.Fatalf("reader %s authorization = %#v, want reader_denied", operation, err)
		}
		controller := transport.AuthenticatedIdentity{DeviceID: "device-controller", Role: string(protocol.RoleController)}
		if err := authorizeAuthenticatedIdentity(controller, true, scope.Action, ""); err != nil {
			t.Fatalf("controller %s authorization = %#v", operation, err)
		}
		if err := authorizeAuthenticatedIdentity(transport.AuthenticatedIdentity{}, false, scope.Action, ""); err != nil {
			t.Fatalf("local %s authorization = %#v", operation, err)
		}
	}
}
