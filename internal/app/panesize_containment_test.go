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
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

type noPaneLookup struct{ calls int }

func (p *noPaneLookup) PaneProcessInfo(context.Context, string) (*herdr.PaneProcessInfo, error) {
	p.calls++
	return nil, panesize.ErrProcessUnavailable
}

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

func TestPaneSizeContainmentAllIngresses(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, path := range []string{transport.TransportWebSocket, transport.TransportGateway, transport.TransportWebRTC} {
			t.Run(path+"/enabled="+map[bool]string{false: "false", true: "true"}[enabled], func(t *testing.T) {
				s := testServer()
				s.cfg.PaneSizeLeaseEnabled = enabled
				provider := &noPaneLookup{}
				s.paneSizeM = panesize.NewManager(provider, nil)
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
				s.hub.SetHandler(func(client *transport.ClientConn, msg map[string]any, admitted func()) {
					defer admitted()
					inbound, err := protocol.DecodeMap(msg)
					if err != nil {
						t.Error(err)
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
					if result["type"] != "command_result" || result["action"] != action || result["ok"] != (i >= 2) {
						t.Fatalf("result: %s", data)
					}
					if i < 2 && !strings.Contains(result["error"].(string), "disabled or unavailable") {
						t.Fatalf("unclear failure: %s", data)
					}
				}
				if provider.calls != 0 {
					t.Fatalf("resolved %d panes", provider.calls)
				}
			})
		}
	}
}
