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
	chunks, err := filepath.Glob(filepath.Join(root, "assets", fmt.Sprintf("*-%d.js", descriptor.Assets)))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatalf("no lazy chunk for shipped asset version %d", descriptor.Assets)
	}

	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	requestPath := "/assets/" + filepath.Base(chunks[0])
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if plain.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", requestPath, plain.Code, http.StatusOK)
	}
	if got := plain.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
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
	if compressed.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("compressed and uncompressed representations share an ETag")
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
