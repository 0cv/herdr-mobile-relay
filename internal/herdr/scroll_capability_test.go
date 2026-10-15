package herdr

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
)

func TestPiSchemaRefreshSurvivesConcurrentLiveEvidence(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "scroll-schema-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client := NewClient("/must-not-run-cli", socket)
	instance, err := pibridge.Instance(socket)
	if err != nil {
		t.Fatal(err)
	}
	m := client.capabilities
	m.refreshSeq.Store(1)
	epoch := m.epoch()
	next := initialServerStatus()
	next.Generation = m.snapshot().Generation
	next.ServerVersion = "0.9.3"
	next.InstalledClientVersion = "herdr 0.9.3"
	next.ServerProtocol = 22
	next.ServerProtocolKnown = true
	next.SocketInstance = instance
	next.Features[FeaturePiScrollSocket] = FeatureEvidence{State: FeatureSupported, Reason: "matching_server_schema"}
	m.updateFeatureAt(epoch, FeatureWorkspaceReordered, FeatureSupported, "subscription_acknowledged")
	m.updateFeatureAt(epoch, FeaturePaneRead, FeatureUnsupported, "newer_validation_refusal")
	m.applyRefresh(1, epoch, "selected-server", next)
	status := m.snapshot()
	if !client.SupportsPiScrollSocket() || status.ServerVersion != "0.9.3" || !status.Supports(FeatureWorkspaceReordered) || status.Feature(FeaturePaneRead).State != FeatureUnsupported {
		t.Fatalf("refresh discarded or overwrote concurrent evidence: %+v", status)
	}
	m.mu.Lock()
	m.status.SocketInstance = "different-instance"
	m.mu.Unlock()
	if client.SupportsPiScrollSocket() {
		t.Fatal("capability was reused for a different selected socket instance")
	}
}

func TestPiScrollCapabilityUsesQualifiedSelectedSchema(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		protocol, schemaProtocol int
		installed                string
		missing                  bool
		state                    FeatureState
		reason                   string
	}{
		{"qualified", 22, 22, "herdr 0.9.3", false, FeatureSupported, "matching_server_schema"},
		{"client-mismatch", 22, 22, "herdr 0.9.2", false, FeatureUnknown, "schema_server_mismatch"},
		{"unobserved-protocol", 0, 0, "herdr 0.9.3", false, FeatureUnknown, "schema_server_mismatch"},
		{"different-confirmed-protocol", 23, 23, "herdr 0.9.3", false, FeatureSupported, "matching_server_schema"},
		{"schema-mismatch", 22, 23, "herdr 0.9.3", false, FeatureUnknown, "schema_unavailable"},
		{"missing-raw-method", 22, 22, "herdr 0.9.3", true, FeatureUnsupported, "schema_incompatible_wire"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin, marker, socket := filepath.Join(dir, "herdr"), filepath.Join(dir, "selected-socket"), filepath.Join(dir, "selected.sock")
			root := scrollSchemaFixture(t)
			root["protocol"] = tc.schemaProtocol
			if tc.missing {
				request := root["schemas"].(map[string]any)["request"].(map[string]any)
				var branches []any
				for _, branch := range request["oneOf"].([]any) {
					if schemaFixtureNode(branch.(map[string]any), "properties", "method")["const"] != "pane.send_text" {
						branches = append(branches, branch)
					}
				}
				request["oneOf"] = branches
			}
			schema, _ := json.Marshal(root)
			script := "#!/bin/sh\nprintf '%s' \"$HERDR_SOCKET_PATH\" > '" + marker + "'\nprintf '%s' '" + string(schema) + "'\n"
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			c := NewClient(bin, socket)
			status := ServerStatus{InstalledClientVersion: tc.installed, ServerVersion: "0.9.3", ServerProtocol: tc.protocol, ServerProtocolKnown: true}
			got := c.piScrollSchemaEvidence(context.Background(), status)
			if got.State != tc.state || got.Reason != tc.reason {
				t.Fatalf("evidence: %+v", got)
			}
			data, err := os.ReadFile(marker)
			if tc.reason == "schema_server_mismatch" {
				if !os.IsNotExist(err) {
					t.Fatal("unqualified server queried schema")
				}
				return
			}
			if err != nil || string(data) != socket {
				t.Fatalf("wrong selected socket: %s %v", data, err)
			}
		})
	}
}
