package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const fixtureScript = "window.fixture = true;"
const fixtureStyle = "body{}"

var fixtureScriptPath = "/assets/app-" + fixtureDigest(fixtureScript) + ".js"
var fixtureStylePath = "/assets/app-" + fixtureDigest(fixtureStyle) + ".css"

func fixtureDigest(data string) string {
	digest := sha256.Sum256([]byte(data))
	return hex.EncodeToString(digest[:])
}

func fixtureDescriptorFile(path, data string) relayrelease.WebDescriptorFile {
	digest := sha256.Sum256([]byte(data))
	return relayrelease.WebDescriptorFile{
		Path:      strings.TrimPrefix(path, "/"),
		SHA256:    hex.EncodeToString(digest[:]),
		Integrity: "sha256-" + base64.StdEncoding.EncodeToString(digest[:]),
	}
}

func fixtureWebRoot(t *testing.T, version string, marker string) string {
	t.Helper()
	root := t.TempDir()
	script := fixtureDescriptorFile(fixtureScriptPath, fixtureScript)
	style := fixtureDescriptorFile(fixtureStylePath, fixtureStyle)
	entryPath := "builds/" + version + "-1-0123456789abcdef/index.html"
	entry := "<html>" + marker + `<link rel="stylesheet" href="` + fixtureStylePath + `" integrity="` + style.Integrity + `"><script src="` + fixtureScriptPath + `" integrity="` + script.Integrity + `"></script></html>`
	descriptor := relayrelease.WebDescriptor{
		Schema:  relayrelease.WebDescriptorSchema,
		Version: version,
		Assets:  1,
		Build:   strings.Repeat("a", 64),
		Entry:   "/" + entryPath,
		Files: map[string]relayrelease.WebDescriptorFile{
			"entry":      fixtureDescriptorFile(entryPath, entry),
			"javascript": script,
			"stylesheet": style,
		},
	}
	descriptorData, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html":           "<html>" + marker + "</html>",
		entryPath:              entry,
		script.Path:            fixtureScript,
		style.Path:             fixtureStyle,
		"release.json":         string(descriptorData),
		"version.json":         `{"version":"` + version + `","assets":1}`,
		"manifest-loader.js":   "",
		"manifest.webmanifest": "{}",
		"setup.webmanifest":    "{}",
		"sw.js":                "",
	}
	for name, contents := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
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
	firstRedirect := httptest.NewRecorder()
	router.ServeHTTP(firstRedirect, request)
	if firstRedirect.Code != http.StatusTemporaryRedirect || firstRedirect.Header().Get("Location") != "/builds/0.20.8-1-0123456789abcdef/index.html" {
		t.Fatalf("old redirect = %d %#v", firstRedirect.Code, firstRedirect.Header())
	}
	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, firstRedirect.Header().Get("Location"), nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "old") {
		t.Fatalf("old response = %d %q", first.Code, first.Body.String())
	}
	if first.Header().Get("ETag") == "" || first.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("old response lost production headers: %#v", first.Header())
	}
	if err := router.activate("candidate"); err != nil {
		t.Fatal(err)
	}
	secondRedirect := httptest.NewRecorder()
	router.ServeHTTP(secondRedirect, httptest.NewRequest(http.MethodGet, "/", nil))
	if secondRedirect.Code != http.StatusTemporaryRedirect || secondRedirect.Header().Get("Location") != "/builds/0.20.10-1-0123456789abcdef/index.html" {
		t.Fatalf("candidate redirect = %d %#v", secondRedirect.Code, secondRedirect.Header())
	}
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, secondRedirect.Header().Get("Location"), nil))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "candidate") {
		t.Fatalf("candidate response = %d %q", second.Code, second.Body.String())
	}
	requests := router.snapshotRequests()
	if len(requests) != 4 || requests[0].Release != "old" || requests[1].Release != "old" || requests[2].Release != "candidate" || requests[3].Release != "candidate" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestReleaseRouterExpiredFaultCannotBeCleared(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.addFault(responseFault{ID: "short-lived", Generation: "generation-1", Method: http.MethodGet, Path: fixtureScriptPath, Kind: "missing", Remaining: -1, LifetimeMs: 1}); err != nil {
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

func TestReleaseRouterPersistentFaultRequiresExplicitClear(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	if err := router.activate("candidate"); err != nil {
		t.Fatal(err)
	}
	if err := router.addFault(responseFault{ID: "candidate-style", Generation: "generation-1", Method: http.MethodGet, Path: fixtureStylePath, Kind: "missing", Remaining: -1}); err != nil {
		t.Fatal(err)
	}
	responses := make(chan int, 4)
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixtureStylePath, nil))
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
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixtureStylePath, nil))
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
	if err := router.addFault(responseFault{ID: "short-lived", Generation: "generation-1", Method: http.MethodGet, Path: fixtureScriptPath, Kind: "missing", Remaining: -1, LifetimeMs: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixtureScriptPath, nil))
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

func TestImmutableFaultsServeDistinctHTMLAndCorruptResponses(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	cases := []struct {
		id          string
		path        string
		kind        string
		contentType string
		body        string
	}{
		{id: "html-poison", path: fixtureScriptPath, kind: "immutable-html", contentType: "text/html; charset=utf-8", body: string(immutableHTML)},
		{id: "corrupt-poison", path: fixtureStylePath, kind: "immutable-corrupt", contentType: "text/css; charset=utf-8", body: "export const fixturePoison = 'not the canonical module';"},
	}
	for _, testCase := range cases {
		t.Run(testCase.id, func(t *testing.T) {
			generation := testCase.id + "-generation"
			if err := router.addFault(responseFault{
				ID: testCase.id, Generation: generation, Method: http.MethodGet, Path: testCase.path,
				Kind: testCase.kind, Remaining: 1,
			}); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, testCase.path, nil))
			if !(response.Code == http.StatusOK && response.Header().Get("Content-Type") == testCase.contentType &&
				response.Header().Get("Cache-Control") == immutablePolicy && response.Body.String() == testCase.body) {
				t.Fatalf("immutable fault response = %d %#v %q", response.Code, response.Header(), response.Body.String())
			}
			requests := router.snapshotRequests()
			request := requests[len(requests)-1]
			if !(request.FaultID == testCase.id && request.FaultGeneration == generation && request.Status == http.StatusOK &&
				request.ContentType == testCase.contentType && request.CacheControl == immutablePolicy && request.Bytes == int64(len(testCase.body)) && request.SHA256 != "") {
				t.Fatalf("fault response accounting = %#v", request)
			}
		})
	}
	if len(immutableHTML) != 59 {
		t.Fatalf("recorded bootstrap body length = %d", len(immutableHTML))
	}
}

func TestImmutableFaultFiniteConsumptionAndExhaustionAreRecorded(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	fault := responseFault{ID: "finite", Generation: "generation-1", Method: http.MethodGet, Path: fixtureScriptPath, Kind: "immutable-html", Remaining: 2}
	if err := router.addFault(fault); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fault.Path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("response = %d", response.Code)
		}
	}
	requests := router.snapshotRequests()
	if len(requests) != 3 || requests[0].Fault != "immutable-html" || requests[1].Fault != "immutable-html" || requests[2].Fault != "" {
		t.Fatalf("request accounting = %#v", requests)
	}
	counts, actions, _ := router.snapshotAccounting()
	if counts[fault.Path] != 3 {
		t.Fatalf("path request count = %#v", counts)
	}
	consumed, exhausted := 0, 0
	for _, action := range actions {
		if action.ID != fault.ID || action.Generation != fault.Generation {
			continue
		}
		if action.Action == "fault_consumed" {
			consumed++
		}
		if action.Action == "fault_exhausted" {
			exhausted++
		}
	}
	if consumed != 2 || exhausted != 1 || len(router.activeFaults()) != 0 {
		t.Fatalf("finite fault actions consumed=%d exhausted=%d active=%#v actions=%#v", consumed, exhausted, router.activeFaults(), actions)
	}
}

func TestImmutableFaultGenerationAndExplicitRepairAreIsolated(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	fault := responseFault{ID: "same-fault", Generation: "generation-2", Method: http.MethodGet, Path: fixtureScriptPath, Kind: "immutable-corrupt", Remaining: -1}
	if err := router.addFault(responseFault{ID: fault.ID, Generation: "generation-1", Method: fault.Method, Path: fault.Path, Kind: fault.Kind, Remaining: -1}); err != nil {
		t.Fatal(err)
	}
	if err := router.addFault(fault); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fault.Path, nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != immutablePolicy {
		t.Fatalf("poison response = %d %#v", response.Code, response.Header())
	}
	if err := router.clearFault(fault.ID, "generation-1", "", ""); err == nil {
		t.Fatal("cleared a different fault generation")
	}
	if err := router.repairFault(fault.ID, fault.Generation, "no-store"); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fault.Path, nil))
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("repaired response policy = %q", response.Header().Get("Cache-Control"))
		}
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fault.Path, nil))
	if response.Header().Get("Cache-Control") == "no-store" {
		t.Fatal("repair policy escaped its bounded response count")
	}
	_, actions, policies := router.snapshotAccounting()
	var repaired, cleared, exhausted int
	for _, action := range actions {
		if action.ID != fault.ID || action.Generation != fault.Generation {
			continue
		}
		switch action.Action {
		case "fault_repaired":
			repaired++
		case "fault_cleared":
			cleared++
		case "fault_exhausted":
			exhausted++
		}
	}
	if repaired != 1 || cleared != 0 || exhausted != 0 || len(policies) != 0 {
		t.Fatalf("repair accounting repaired=%d cleared=%d exhausted=%d policies=%#v actions=%#v", repaired, cleared, exhausted, policies, actions)
	}
}

func TestImmutablePersistentFaultExpiresAndInvalidates(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	fault := responseFault{ID: "persistent-poison", Generation: "generation-1", Method: http.MethodGet, Path: fixtureScriptPath, Kind: "immutable-html", Remaining: -1, LifetimeMs: 20}
	if err := router.addFault(fault); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fault.Path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("persistent response = %d", response.Code)
	}
	if active := router.activeFaults(); len(active) != 1 || active[0].Remaining != -1 {
		t.Fatalf("persistent fault exhausted by request: %#v", active)
	}
	time.Sleep(30 * time.Millisecond)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fault.Path, nil))
	if response.Code != http.StatusServiceUnavailable || !router.invalidated {
		t.Fatalf("expired persistent response = %d invalidated=%v", response.Code, router.invalidated)
	}
	_, actions, _ := router.snapshotAccounting()
	if !slices.ContainsFunc(actions, func(action faultAction) bool {
		return action.Action == "fault_expired" && action.ID == fault.ID && action.Generation == fault.Generation
	}) {
		t.Fatalf("expiry was not recorded: %#v", actions)
	}
}

func TestFaultControlsEnforceLifetimeCountAndResourceBounds(t *testing.T) {
	router, err := newReleaseRouter(fixtureWebRoot(t, "0.20.8", "old"), fixtureWebRoot(t, "0.20.10", "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.close()
	invalid := []responseFault{
		{Path: fixtureScriptPath, Kind: "immutable-html", Remaining: maxFaultResponses + 1},
		{Path: fixtureScriptPath, Kind: "immutable-corrupt", Remaining: -2},
		{Path: fixtureScriptPath, Kind: "immutable-html", Remaining: 1, LifetimeMs: int(maxFaultLifetime/time.Millisecond) + 1},
		{Path: "/index.html", Kind: "immutable-html", Remaining: 1},
		{Path: fixtureScriptPath + "?cache=bypass", Kind: "immutable-corrupt", Remaining: 1},
	}
	for _, fault := range invalid {
		if err := router.addFault(fault); err == nil {
			t.Fatalf("accepted invalid fault %#v", fault)
		}
	}
}
