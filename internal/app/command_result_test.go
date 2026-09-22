package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestSlashCommandCatalogFitsOutboundHubBudget(t *testing.T) {
	commands := make([]slashcmd.Command, 1999)
	for index := range commands {
		commands[index] = slashcmd.Command{
			Command:      "/skill-" + formatTestIndex(index),
			Description:  strings.Repeat("&", 240),
			ArgumentHint: strings.Repeat("&", 120),
			Source:       "personal",
		}
	}
	metadata := make(map[string]slashcmd.Metadata)
	for _, command := range commands {
		metadata[command.Command] = slashcmd.Metadata{Kind: "extension", Provenance: &slashcmd.Provenance{Path: strings.Repeat("&", 1024), Source: "local", Scope: "user", Origin: "top-level"}}
	}
	catalog := fitSlashCommandCatalog(
		slashcmd.Catalog{Commands: commands, Metadata: metadata, Status: "partial"},
		"req-large-catalog", "list_slash_commands", "pane-large-catalog",
	)
	message := commandResultMessage(&coordinator.CommandResult{
		RequestID: "req-large-catalog",
		Action:    "list_slash_commands",
		OK:        true,
		Phase:     "completed",
		PaneID:    "pane-large-catalog",
		Data:      catalog,
	})
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > transport.MaxOutboundMessageBytes {
		t.Fatalf("slash catalog response = %d bytes, exceeds outbound limit %d", len(encoded), transport.MaxOutboundMessageBytes)
	}
	if !catalog.Truncated || len(catalog.Commands) >= len(commands) {
		t.Fatalf("large catalog = %d commands, truncated %v; want byte-clipped response", len(catalog.Commands), catalog.Truncated)
	}
	if catalog.Status != "partial" || len(catalog.Metadata) != len(catalog.Commands) || catalog.Revision != slashcmd.Revise(catalog).Revision {
		t.Fatal("byte guard lost runtime status, metadata, or revision")
	}
	t.Logf("byte-bounded slash catalog: %d commands, %d serialized bytes (limit %d)", len(catalog.Commands), len(encoded), transport.MaxOutboundMessageBytes)

	hub := transport.NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	accepted := make(chan bool, 1)
	hub.SetOnConnect(func(client *transport.ClientConn) {
		accepted <- hub.Send(client, message)
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleWebSocket))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(transport.MaxOutboundMessageBytes)

	select {
	case ok := <-accepted:
		if !ok {
			t.Fatal("hub rejected a byte-bounded slash catalog")
		}
	case <-ctx.Done():
		t.Fatal("hub did not attempt slash catalog delivery")
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read slash catalog response: %v", err)
	}
	var wire struct {
		Data slashcmd.Catalog `json:"data"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.Data.Truncated || len(wire.Data.Commands) != len(catalog.Commands) {
		t.Fatalf("hub delivered catalog = %d commands, truncated %v; want %d and true", len(wire.Data.Commands), wire.Data.Truncated, len(catalog.Commands))
	}
	conn.CloseNow()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := hub.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func formatTestIndex(index int) string {
	return fmt.Sprintf("%04d", index)
}

func TestCommandResultMessageKeepsPythonMandatoryEmptyFields(t *testing.T) {
	got := commandResultMessage(&coordinator.CommandResult{
		RequestID: "req-001",
		Action:    "prompt",
		OK:        true,
		Phase:     "completed",
		PaneID:    "pane-1",
	})
	want := map[string]any{
		"type":       "command_result",
		"request_id": "req-001",
		"action":     "prompt",
		"ok":         true,
		"phase":      "completed",
		"error":      "",
		"pane_id":    "pane-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command result = %#v, want %#v", got, want)
	}
}

func TestPaneSizeLeaseCommandResultIncludesAppliedColumns(t *testing.T) {
	got := commandResultMessage(&coordinator.CommandResult{
		RequestID: "req-size",
		Action:    "lease_pane_size",
		OK:        true,
		Phase:     "completed",
		PaneID:    "pane-1",
		Data:      map[string]any{"columns": 84},
	})
	if got["action"] != "lease_pane_size" ||
		!reflect.DeepEqual(got["data"], map[string]any{"columns": 84}) {
		t.Fatalf("pane size lease result = %#v", got)
	}
}

func TestPaneSizeLeaseMutationsRemainOrdered(t *testing.T) {
	for _, action := range []string{"lease_pane_size", "release_pane_size"} {
		if !isCoordinatorMutation(action) {
			t.Errorf("isCoordinatorMutation(%q) = false", action)
		}
	}
}

func TestCanonicalHTTPPathRejectsDotAndEmptySegments(t *testing.T) {
	for _, candidate := range []string{"/assets/../index.html", "/assets//app.js", "/assets/./app.js"} {
		if canonicalHTTPPath(candidate) {
			t.Errorf("canonicalHTTPPath(%q) = true", candidate)
		}
	}
	for _, candidate := range []string{"/", "/healthz", "/assets/app.js"} {
		if !canonicalHTTPPath(candidate) {
			t.Errorf("canonicalHTTPPath(%q) = false", candidate)
		}
	}
}
