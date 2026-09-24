package app

import (
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func testUDPServer(t *testing.T, pluginPort int, managedRunID string) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Server{
		cfg: &config.Config{
			PluginPort:   pluginPort,
			ManagedRunID: managedRunID,
			SocketPath:   filepath.Join(t.TempDir(), "herdr.sock"),
		},
		state:  coordinator.NewState(logger),
		logger: logger,
	}
}

func occupiedUDPPort(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("occupy UDP port: %v", err)
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		t.Fatalf("unexpected local addr type %T", conn.LocalAddr())
	}
	return conn, addr.Port
}

func TestManagedUDPBindFailureIsFatal(t *testing.T) {
	blocker, port := occupiedUDPPort(t)
	defer blocker.Close()

	server := testUDPServer(t, port, "managed-run-1")
	err := server.startUDPListener()
	if err == nil {
		t.Fatal("expected fatal error for managed UDP bind failure, got nil")
	}
	if server.udp != nil {
		t.Fatal("expected no UDP listener to be assigned on managed bind failure")
	}
	udpAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if !strings.Contains(err.Error(), udpAddr) {
		t.Fatalf("error %q does not name bound address %q", err.Error(), udpAddr)
	}
	if got := server.recentSafeErrors(); len(got) != 0 {
		t.Fatalf("expected no safe-error record on managed bind failure, got %v", got)
	}
}

func TestLegacyUDPBindFailureStillWarns(t *testing.T) {
	blocker, port := occupiedUDPPort(t)
	defer blocker.Close()

	server := testUDPServer(t, port, "")
	err := server.startUDPListener()
	if err != nil {
		t.Fatalf("expected legacy bind failure to be non-fatal, got %v", err)
	}
	if server.udp != nil {
		t.Fatal("expected no UDP listener to be assigned on legacy bind failure")
	}
	got := server.recentSafeErrors()
	if len(got) != 1 {
		t.Fatalf("expected exactly one safe-error record, got %v", got)
	}
	if !strings.Contains(got[0], "UDP event listener") {
		t.Fatalf("safe-error record %q does not mention the UDP listener", got[0])
	}
}

func TestUDPListenerBindsWhenPortFree(t *testing.T) {
	server := testUDPServer(t, 0, "managed-run-2")
	if err := server.startUDPListener(); err != nil {
		t.Fatalf("expected free-port bind to succeed, got %v", err)
	}
	if server.udp == nil {
		t.Fatal("expected UDP listener to be assigned on successful bind")
	}
	if err := server.udp.Close(); err != nil {
		t.Fatalf("close UDP listener: %v", err)
	}
}
