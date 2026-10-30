package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestServesShippedConversationHistoryLazyChunk(t *testing.T) {
	root := filepath.Join("..", "..", "web")
	descriptor, err := relayrelease.LoadWebDescriptor(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}

	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, name := range []string{"SettingsView", "Card"} {
		requestPath := fmt.Sprintf("/assets/%s-%d.js", name, descriptor.Assets)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", requestPath, response.Code, http.StatusOK)
		}
	}
	requestPath := fmt.Sprintf("/assets/Card-%d.js", descriptor.Assets)
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if plain.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", requestPath, plain.Code, http.StatusOK)
	}
	if got := plain.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("lazy chunk cache control = %q", got)
	}
	if plain.Header().Get("ETag") == "" {
		t.Fatal("uncompressed lazy chunk is missing an ETag")
	}

	compressedRequest := httptest.NewRequest(http.MethodGet, requestPath, nil)
	compressedRequest.Header.Set("Accept-Encoding", "br")
	compressed := httptest.NewRecorder()
	h.ServeHTTP(compressed, compressedRequest)
	if compressed.Code != http.StatusOK {
		t.Fatalf("Brotli GET %s = %d, want %d", requestPath, compressed.Code, http.StatusOK)
	}
	if compressed.Header().Get("Content-Encoding") != "br" {
		t.Fatal("lazy chunk did not serve its Brotli representation")
	}
	if compressed.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Brotli Vary = %q", compressed.Header().Get("Vary"))
	}
	if got := compressed.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Brotli lazy chunk cache control = %q", got)
	}
	if compressed.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("compressed and uncompressed representations share an ETag")
	}
	for _, requestPath := range []string{
		fmt.Sprintf("/assets/Unreferenced-%d.js", descriptor.Assets),
		fmt.Sprintf("/assets/Card-%d.js", descriptor.Assets-1),
		"/assets/../release.json",
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", requestPath, response.Code)
		}
	}
}

func TestLazyModuleClosureOnlyAdmitsReferencedCurrentModules(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"entry.js":         "import(`./Settings-363.js`);",
		"Settings-363.js":  `import{t as card}from"./Card-363.js";import"./Card-362.js";import"../foreign.js";`,
		"Card-363.js":      `import{t as settings}from"./Settings-363.js";`,
		"Unrelated-363.js": "export default {};",
	} {
		if err := os.WriteFile(filepath.Join(root, "assets", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	h := &Handler{files: opened.FS(), bundleAssets: 363, webFiles: map[string]bool{}, lazyAssets: map[string]bool{}}
	if err := h.loadLazyAssets("assets/entry.js"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"Settings-363.js": true, "Card-363.js": true, "Card-362.js": false, "Unrelated-363.js": false, "foreign.js": false,
	} {
		if got := h.lazyAssets["assets/"+name]; got != want {
			t.Fatalf("admission for %s = %t, want %t", name, got, want)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "entry.js"), make([]byte, 1024*1024+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.loadLazyAssets("assets/entry.js"); err == nil {
		t.Fatal("oversized release module was accepted")
	}
}

func TestRejectsLazyChunkFromAnotherReleaseVersion(t *testing.T) {
	root := setupContentAddressedWebRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "ConversationHistory-364.js"), []byte("export default {};"), 0o644); err != nil {
		t.Fatal(err)
	}
	currentVersion := fmt.Sprintf("ConversationHistory-%d.js", 363)
	if err := os.WriteFile(filepath.Join(root, "assets", currentVersion), []byte("export default {};"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, name := range []string{"ConversationHistory-364.js", currentVersion} {
		request := httptest.NewRequest(http.MethodGet, "/assets/"+name, nil)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("unapproved lazy chunk %s status = %d, want %d", name, response.Code, http.StatusNotFound)
		}
	}
}
