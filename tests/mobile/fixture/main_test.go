package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func fixtureWebRoot(t *testing.T, version string, marker string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html":           "<html>" + marker + "</html>",
		"assets/app.js":        "window.fixture = '" + marker + "';",
		"assets/app.css":       "body{}",
		"version.json":         `{"version":"` + version + `","assets":1}`,
		"manifest-loader.js":   "",
		"manifest.webmanifest": "{}",
		"setup.webmanifest":    "{}",
		"sw.js":                "",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReleaseRouterSwapKeepsOriginAndChangesBundle(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, request)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "old") {
		t.Fatalf("old response = %d %q", first.Code, first.Body.String())
	}
	if first.Header().Get("ETag") == "" || first.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("old response lost production headers: %#v", first.Header())
	}
	if err := router.activate("candidate"); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "candidate") {
		t.Fatalf("candidate response = %d %q", second.Code, second.Body.String())
	}
	requests := router.snapshotRequests()
	if len(requests) != 2 || requests[0].Release != "old" || requests[1].Release != "candidate" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestReleaseRouterBarrierFaultIsDeterministic(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.addFault(responseFault{Method: http.MethodGet, Path: "/index.html", Kind: "stall", Barrier: "entry", Remaining: 1}); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/index.html", nil)
		router.ServeHTTP(httptest.NewRecorder(), request)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("stalled response completed before the barrier was released")
	case <-time.After(30 * time.Millisecond):
	}
	if err := router.releaseBarrier("entry"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stalled response did not finish after the barrier was released")
	}
	requests := router.snapshotRequests()
	if len(requests) != 1 || requests[0].Fault != "stall" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestReleaseRouterExpiredFaultCannotBeCleared(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.addFault(responseFault{ID: "short-lived", Generation: "generation-1", Method: http.MethodGet, Path: "/assets/app.js", Kind: "missing", Remaining: -1, LifetimeMs: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := router.clearFault("short-lived", "generation-1", "", ""); err == nil {
		t.Fatal("cleared an expired fault")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cleared expired fault served status = %d", response.Code)
	}
}

func TestReleaseRouterOneShotStallExpiryRemainsInvalidated(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.addFault(responseFault{ID: "stalled", Generation: "generation-1", Method: http.MethodGet, Path: "/index.html", Kind: "stall", Barrier: "entry", Remaining: 1, LifetimeMs: 1}); err != nil {
		t.Fatal(err)
	}
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/index.html", nil))
		responseDone <- response
	}()
	deadline := time.Now().Add(time.Second)
	for len(router.snapshotRequests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(router.snapshotRequests()) == 0 {
		t.Fatal("stalled request was not recorded")
	}
	time.Sleep(20 * time.Millisecond)
	if err := router.releaseBarrier("entry"); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responseDone:
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("expired stalled response = %d, body = %q", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("expired stalled response did not finish")
	}
	if !router.invalidated {
		t.Fatal("expired stalled fault did not invalidate the fixture")
	}
}

func TestReleaseRouterPersistentFaultRequiresExplicitClear(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.activate("candidate"); err != nil {
		t.Fatal(err)
	}
	if err := router.addFault(responseFault{ID: "candidate-style", Generation: "generation-1", Method: http.MethodGet, Path: "/assets/app.css", Kind: "missing", Remaining: -1}); err != nil {
		t.Fatal(err)
	}
	responses := make(chan int, 4)
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
			responses <- response.Code
		}()
	}
	group.Wait()
	close(responses)
	for status := range responses {
		if status != http.StatusNotFound {
			t.Fatalf("persistent fault response = %d", status)
		}
	}
	requests := router.snapshotRequests()
	if len(requests) != 4 || requests[0].FaultID != "candidate-style" || requests[0].FaultGeneration != "generation-1" {
		t.Fatalf("requests = %#v", requests)
	}
	if err := router.clearFault("candidate-style", "wrong-generation", "", ""); err == nil {
		t.Fatal("cleared a fault from the wrong generation")
	}
	if err := router.clearFault("candidate-style", "generation-1", "", ""); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("cleared fault response = %d", response.Code)
	}
}

func TestReleaseRouterFaultExpiryInvalidatesFixture(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.addFault(responseFault{ID: "short-lived", Generation: "generation-1", Method: http.MethodGet, Path: "/assets/app.js", Kind: "missing", Remaining: -1, LifetimeMs: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired fault status = %d, body = %q", response.Code, response.Body.String())
	}
	if !router.invalidated || router.invalidationReason == "" {
		t.Fatalf("router was not invalidated: %#v", router)
	}
	active := router.activeFaults()
	if len(active) != 1 || active[0].ID != "short-lived" {
		t.Fatalf("expired fault was removed: %#v", active)
	}
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalidated fixture served a healthy response: %d", second.Code)
	}
}

func TestDeviceCredentialPersistsAcrossResolverRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := store.CreateInvitation("phone", deviceauth.RoleController, "en")
	if err != nil {
		t.Fatal(err)
	}
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version, Locale: "en"}
	if _, err := store.ResolveE2EESecret(context.Background(), selector); err != nil {
		t.Fatal(err)
	}
	resolver := &recordingResolver{store: store, evidence: newAuthEvidence()}
	result, err := resolver.CompleteE2EEAuth(context.Background(), selector, true)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := result.Identity.CredentialID
	credentialSelector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: credentialID, Version: result.Identity.CredentialVersion, Locale: "en"}
	if _, err := resolver.ResolveE2EESecret(context.Background(), credentialSelector); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.CompleteE2EEAuth(context.Background(), credentialSelector, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	credentials := reopened.ListCredentials(credentialID)
	if len(credentials) != 1 || credentials[0].CredentialID != credentialID {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestControlSecretRejectsForgedRequest(t *testing.T) {
	fixture := &fixture{controlSecret: "known-secret"}
	request := httptest.NewRequest(http.MethodGet, "/state", nil)
	request.Header.Set("X-Herdr-Fixture-Secret", "wrong-secret")
	response := httptest.NewRecorder()
	fixture.controlHandler(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("forged control status = %d", response.Code)
	}
}

func TestControlStateIsSanitizedShape(t *testing.T) {
	fixture := &fixture{
		controlSecret: "known-secret",
		appURL:        "https://localhost:1234",
		router:        &releaseRouter{active: "old", requests: []requestRecord{}},
	}
	request := httptest.NewRequest(http.MethodGet, "/state", nil)
	request.Header.Set("X-Herdr-Fixture-Secret", "known-secret")
	response := httptest.NewRecorder()
	fixture.controlHandler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("state status = %d", response.Code)
	}
	var state map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["active_release"] != "old" {
		t.Fatalf("state = %#v", state)
	}
}
