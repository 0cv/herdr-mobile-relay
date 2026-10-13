package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// paneSizeFakes stand in for Herdr, ps and the TTY behind the real manager:
// one pane, pane-1, whose shell 100 runs on /dev/pts/fake at 120x40.
type paneSizeFakes struct {
	mu      sync.Mutex
	lookups int
	size    panesize.Winsize
	writes  int
}

func (f *paneSizeFakes) PaneGet(_ context.Context, paneID string) (*herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return &herdr.Pane{ID: paneID, TerminalID: "terminal-1"}, nil
}

func (f *paneSizeFakes) PaneProcessInfoFresh(_ context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return &herdr.PaneProcessInfo{PaneID: paneID, ShellPID: 100}, nil
}

func (f *paneSizeFakes) TTYPath(context.Context, int) (string, error) { return "/dev/pts/fake", nil }

func (f *paneSizeFakes) Open(string) (panesize.Terminal, error) { return f, nil }

func (f *paneSizeFakes) Device() (uint64, bool, error) { return 7, true, nil }

func (f *paneSizeFakes) Size() (panesize.Winsize, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.size, nil
}

func (f *paneSizeFakes) SetSize(size panesize.Winsize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.size = size
	f.writes++
	return nil
}

func (f *paneSizeFakes) Close() error { return nil }

func (f *paneSizeFakes) snapshot() (int, panesize.Winsize, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, f.size, f.writes
}

func newPaneSizeFakes(s *Server) *paneSizeFakes {
	fakes := &paneSizeFakes{size: panesize.Winsize{Rows: 40, Columns: 120}}
	s.paneSizeM = panesize.NewManagerWith(fakes, fakes, fakes, time.Now, nil)
	return fakes
}

// The relay never advertises the names apps up to 0.22.7 lease automatically
// on, and advertises the opt-in names only where a backend exists.
func TestPaneSizeCapabilitiesAreOptInOnly(t *testing.T) {
	s := testServer()
	supported := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
	if s.paneSizeM.Available() != supported {
		t.Fatalf("production backend available = %v on %s", s.paneSizeM.Available(), runtime.GOOS)
	}
	for range 2 { // initial and subsequent capability snapshots
		caps := s.effectiveCapabilitiesFor(herdr.ServerStatus{})
		for _, legacy := range []string{"pane_size_lease", "pane_size_lease_rows"} {
			if slices.Contains(caps, legacy) {
				t.Fatalf("advertised legacy %s", legacy)
			}
		}
		for _, optIn := range []string{protocol.PaneSizeOptInCapability, protocol.PaneSizeOptInRowsCapability} {
			if slices.Contains(caps, optIn) != supported {
				t.Fatalf("%s advertised = %v on %s", optIn, slices.Contains(caps, optIn), runtime.GOOS)
			}
		}
		for _, cap := range []string{"conversation_history", "slash_commands", "semantic_input"} {
			if !slices.Contains(caps, cap) {
				t.Fatalf("lost controller capability %s", cap)
			}
		}
	}
	s.paneSizeM = panesize.NewManager(nil, nil)
	for _, cap := range s.effectiveCapabilitiesFor(herdr.ServerStatus{}) {
		if strings.HasPrefix(cap, "pane_size") {
			t.Fatalf("unavailable backend advertised %s", cap)
		}
	}
}

// Every ingress reaches the same ordered handler. A lease without opt_in is
// refused before any Herdr, process or TTY access (an older app that leases
// automatically); an opt-in lease reaches the manager; release completes,
// immediately when asked to.
func TestPaneSizeCommandsOnAllIngresses(t *testing.T) {
	for _, path := range []string{transport.TransportWebSocket, transport.TransportGateway, transport.TransportWebRTC} {
		t.Run(path, func(t *testing.T) {
			s := testServer()
			fakes := newPaneSizeFakes(s)
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
			exchange := func(fields map[string]any) map[string]any {
				t.Helper()
				request := map[string]any{"pane_id": "pane-1", "request_id": "request", "protocol": protocol.Version}
				for key, value := range fields {
					request[key] = value
				}
				payload, _ := json.Marshal(request)
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
				if result["type"] != "command_result" || result["action"] != request["type"] || result["pane_id"] != "pane-1" {
					t.Fatalf("result: %s", data)
				}
				return result
			}

			for _, stale := range []map[string]any{
				{"type": "lease_pane_size", "columns": 80, "rows": 24},
				{"type": "lease_pane_size", "columns": 80, "opt_in": false},
			} {
				result := exchange(stale)
				if result["ok"] != false || result["phase"] != "failed" || result["error"] != "Resize Session is unavailable on this relay" {
					t.Fatalf("lease without opt_in: %v", result)
				}
			}
			if lookups, size, writes := fakes.snapshot(); lookups != 0 || writes != 0 || size.Columns != 120 {
				t.Fatalf("a lease without opt_in reached the backend: lookups=%d writes=%d size=%+v", lookups, writes, size)
			}

			for range 2 { // acquire, then renew
				result := exchange(map[string]any{"type": "lease_pane_size", "columns": 80, "opt_in": true})
				data, _ := result["data"].(map[string]any)
				if result["ok"] != true || result["phase"] != "completed" || data["columns"] != float64(80) || data["rows"] != float64(40) {
					t.Fatalf("opt-in lease: %v", result)
				}
			}
			if lookups, size, writes := fakes.snapshot(); lookups == 0 || writes != 1 || size.Columns != 80 {
				t.Fatalf("opt-in lease: lookups=%d writes=%d size=%+v", lookups, writes, size)
			}

			// A plain release keeps the navigation grace.
			if result := exchange(map[string]any{"type": "release_pane_size"}); result["ok"] != true || result["phase"] != "completed" {
				t.Fatalf("release: %v", result)
			}
			if _, size, _ := fakes.snapshot(); size.Columns != 80 {
				t.Fatalf("a grace release restored at once: %+v", size)
			}
			// Turning Resize Session off releases without the grace.
			for range 2 {
				if result := exchange(map[string]any{"type": "release_pane_size", "immediate": true}); result["ok"] != true || result["error"] != "" {
					t.Fatalf("immediate release: %v", result)
				}
			}
			if _, size, writes := fakes.snapshot(); size.Columns != 120 || size.Rows != 40 || writes != 2 {
				t.Fatalf("immediate release: size=%+v writes=%d", size, writes)
			}
		})
	}
}

// Only a client that holds an active lease on a pane reads it at the leased
// geometry; client-supplied values never select the resized read path.
func TestPaneReadGeometryFollowsTheReadersOwnLease(t *testing.T) {
	s := testServer()
	newPaneSizeFakes(s)
	message := func() map[string]any {
		return map[string]any{"pane_id": "pane-1", "terminal_columns": 50, "terminal_rows": 12}
	}
	other := message()
	s.applyPaneReadLease("client-a", other)
	if _, present := other["terminal_columns"]; present {
		t.Fatalf("client-supplied geometry kept without a lease: %v", other)
	}
	if _, _, err := s.paneSizeM.Acquire(context.Background(), "client-a", "pane-1", 80, 0); err != nil {
		t.Fatal(err)
	}
	owner := message()
	s.applyPaneReadLease("client-a", owner)
	if owner["terminal_columns"] != 80 || owner["terminal_rows"] != 40 {
		t.Fatalf("lease holder read geometry = %v", owner)
	}
	reader := message()
	s.applyPaneReadLease("client-b", reader)
	if _, present := reader["terminal_columns"]; present {
		t.Fatalf("a client without its own lease read the leased geometry: %v", reader)
	}
}

// Both commands stay controller-only mutations, and release keeps the
// stale-target cleanup allowance it shares with unwatch_pane and cancel_speech.
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
