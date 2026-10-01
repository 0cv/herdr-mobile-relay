package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestIngressForTransportMapsEveryResolvedMode(t *testing.T) {
	for _, test := range []struct {
		transport string
		want      string
	}{
		{transport: config.TransportCloudflare, want: protocol.IngressCloudflare},
		{transport: config.TransportGateway, want: protocol.IngressGateway},
		{transport: config.TransportTailscale, want: protocol.IngressTailscaleManaged},
		{transport: config.TransportTailscaleExternal, want: protocol.IngressTailscaleExternal},
		{transport: config.TransportTailscaleCLI, want: protocol.IngressTailscaleCLI},
		// Unknown or unresolved modes are omitted, never guessed.
		{transport: "", want: ""},
		{transport: "relay.example.com", want: ""},
	} {
		if got := ingressForTransport(test.transport); got != test.want {
			t.Fatalf("ingressForTransport(%q) = %q, want %q", test.transport, got, test.want)
		}
	}
	if got := (&Server{}).ingressDescriptor(); got != "" {
		t.Fatalf("server without configuration reported ingress %q", got)
	}
}

func TestConnectionSnapshotCarriesIngressDescriptor(t *testing.T) {
	cfg := &config.Config{
		Host:       "127.0.0.1",
		Port:       8375,
		InstanceID: "test-instance",
		Transport:  config.TransportCloudflare,
	}
	server := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	push := readSnapshotPushConfig(t, server)
	if push["ingress"] != protocol.IngressCloudflare {
		t.Fatalf("push_config ingress = %#v, want %q", push["ingress"], protocol.IngressCloudflare)
	}
	// The descriptor must not expose the relay's address or hostname.
	if strings.Contains(push["ingress"].(string), cfg.Host) {
		t.Fatalf("ingress descriptor leaked the listener host: %#v", push["ingress"])
	}
}

func TestConnectionSnapshotOmitsUnknownIngressDescriptor(t *testing.T) {
	push := readSnapshotPushConfig(t, testServer())
	if _, present := push["ingress"]; present {
		t.Fatalf("push_config without a resolved transport advertised ingress: %#v", push["ingress"])
	}
	// The rest of the snapshot contract is unchanged for older apps.
	if push["protocol"] != float64(protocol.Version) {
		t.Fatalf("push_config protocol = %#v", push["protocol"])
	}
}

func readSnapshotPushConfig(t *testing.T, server *Server) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		_ = server.hub.Shutdown(ctx)
	}()
	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		server.sendConnectionSnapshot(client)
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
	defer httpServer.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if message["type"] != "push_config" {
		t.Fatalf("first snapshot message = %#v, want push_config", message)
	}
	return message
}
