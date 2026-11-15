package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestHealth(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealth(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want \"ok\\n\"", body)
	}
	if inst := w.Header().Get("X-Herdr-Relay-Instance"); inst != "test-instance" {
		t.Errorf("instance header = %q, want test-instance", inst)
	}
}

func TestHealthz(t *testing.T) {
	s := testServer()
	s.ready = true
	s.state.CommitInventory(nil, 0)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["readiness"] != "ready" {
		t.Errorf("readiness = %v, want ready", resp["readiness"])
	}
	if resp["instance"] != "test-instance" {
		t.Errorf("instance = %v, want test-instance", resp["instance"])
	}
	if resp["release_version"] != "0.9.0" {
		t.Errorf("release_version = %v, want 0.9.0", resp["release_version"])
	}
	if resp["revision"] != "abc123" {
		t.Errorf("revision = %v, want abc123", resp["revision"])
	}
	if resp["protocol"] != float64(protocol.Version) {
		t.Errorf("protocol = %v, want %d", resp["protocol"], protocol.Version)
	}
	if resp["gateway_available_version"] != "0.9.0" {
		t.Errorf("gateway_available_version = %v, want 0.9.0", resp["gateway_available_version"])
	}
}

func TestReadyzIncludesReleaseIdentityForEmptyInventory(t *testing.T) {
	s := testServer()
	s.ready = true
	s.state.CommitInventory(nil, 0)
	w := httptest.NewRecorder()
	s.handleReadyz(w, httptest.NewRequest("GET", "/readyz", nil))
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response["status"] != "ready" || response["instance"] != "test-instance" || response["release_version"] != "0.9.0" || response["revision"] != "abc123" {
		t.Fatalf("readiness response: %d %+v", w.Code, response)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("readiness may be cached")
	}
}

func TestReadyzNotReady(t *testing.T) {
	s := testServer()
	s.ready = false
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	s.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "unavailable" {
		t.Errorf("status = %v, want unavailable", resp["status"])
	}
}
