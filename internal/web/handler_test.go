package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setupTestWebRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.MkdirAll(filepath.Join(dir, "icons"), 0o755)

	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>hello</html>"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('app')"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.css"), []byte("body{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "sw.js"), []byte("// sw"), 0o644)
	os.WriteFile(filepath.Join(dir, "icons", "icon-192.png"), []byte("png-data"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644)

	// Create a .br sidecar
	os.WriteFile(filepath.Join(dir, "assets", "app.js.br"), []byte("compressed"), 0o644)

	return dir
}

func TestServesAllowedAsset(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/index.html", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "<html>hello</html>" {
		t.Errorf("body = %q", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if etag := w.Header().Get("ETag"); etag == "" {
		t.Error("missing ETag")
	}
}

func TestServesRootAsIndex(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "<html>hello</html>" {
		t.Errorf("body = %q", body)
	}
}

func TestRejectsDisallowedAsset(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/secret.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/../etc/passwd", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestServesBrotliWhenAccepted(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "br" {
		t.Errorf("content-encoding = %q, want br", enc)
	}
	if body := w.Body.String(); body != "compressed" {
		t.Errorf("body = %q, want compressed", body)
	}
}

func TestServesUncompressedWithoutBr(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("content-encoding = %q, want empty", enc)
	}
	if body := w.Body.String(); body != "console.log('app')" {
		t.Errorf("body = %q", body)
	}
}

func TestConditionalRequest304(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	// First request to get ETag
	req := httptest.NewRequest("GET", "/index.html", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	etag := w.Header().Get("ETag")

	// Second request with If-None-Match
	req2 := httptest.NewRequest("GET", "/index.html", nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w2.Code)
	}
}

func TestServesIconsWildcard(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/icons/icon-192.png", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestSPAFallbackAndUnsupportedMethod(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	req := httptest.NewRequest(http.MethodGet, "/settings/relay", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "<html>hello</html>" {
		t.Fatalf("SPA response = %d %q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/index.html", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST response = %d, Allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

func TestSecurityCacheMIMEAndHEADContract(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	req := httptest.NewRequest(http.MethodHead, "/assets/app.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD response = %d with %d body bytes", w.Code, w.Body.Len())
	}
	if got := w.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("JavaScript MIME = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("asset cache control = %q", got)
	}
	for _, header := range []string{
		"Content-Security-Policy",
		"Permissions-Policy",
		"Referrer-Policy",
		"X-Content-Type-Options",
		"X-Frame-Options",
	} {
		if w.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
}

func TestBrotliQualityWildcardAndCanonicalPathParity(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, header := range []string{"br;q=0.0", "BR;Q=invalid", "*;q=0"} {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
		req.Header.Set("Accept-Encoding", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%q selected encoding %q", header, got)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "*;q=0.5")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("wildcard selected encoding %q, want br", got)
	}
	for _, requestPath := range []string{"/assets/../index.html", "/assets//app.js", "/assets/./app.js"} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%q status = %d, want 404", requestPath, w.Code)
		}
	}
}

func TestWeakETagMatches(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	req.Header.Set("If-None-Match", "W/"+first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	h.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("weak ETag status = %d, want 304", second.Code)
	}
}

func TestBrotliRepresentationHasDistinctETagAndHonorsQZero(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	plainRequest := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, plainRequest)
	compressedRequest := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	compressedRequest.Header.Set("Accept-Encoding", "br")
	compressed := httptest.NewRecorder()
	h.ServeHTTP(compressed, compressedRequest)
	if plain.Header().Get("ETag") == compressed.Header().Get("ETag") {
		t.Fatal("plain and Brotli representations share an ETag")
	}
	if compressed.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Vary = %q", compressed.Header().Get("Vary"))
	}

	disabledRequest := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	disabledRequest.Header.Set("Accept-Encoding", "gzip, br;q=0")
	disabled := httptest.NewRecorder()
	h.ServeHTTP(disabled, disabledRequest)
	if disabled.Header().Get("Content-Encoding") != "" || disabled.Body.String() != "console.log('app')" {
		t.Fatalf("br;q=0 response encoding=%q body=%q", disabled.Header().Get("Content-Encoding"), disabled.Body.String())
	}
}

func TestWebRootSymlinkEscapeIsRejected(t *testing.T) {
	root := setupTestWebRoot(t)
	outside := filepath.Join(t.TempDir(), "outside.js")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "assets", "app.js")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "assets", "app.js")); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("symlink escape status = %d, want 404", w.Code)
	}
}
