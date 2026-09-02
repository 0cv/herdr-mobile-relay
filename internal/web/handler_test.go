package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestWebRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.MkdirAll(filepath.Join(dir, "icons"), 0o755)
	os.MkdirAll(filepath.Join(dir, "fonts"), 0o755)

	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>hello</html>"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('app')"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.css"), []byte("body{}"), 0o644)
	os.WriteFile(
		filepath.Join(dir, "assets", "attachment-hash.worker-D_WkX-nj.js"),
		[]byte("self.onmessage = () => {}"),
		0o644,
	)
	os.WriteFile(filepath.Join(dir, "sw.js"), []byte("// sw"), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest-loader.js"), []byte("// manifest loader"), 0o644)
	os.WriteFile(filepath.Join(dir, "setup.webmanifest"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "icons", "icon-192.png"), []byte("png-data"), 0o644)
	os.WriteFile(filepath.Join(dir, "fonts", "nerd-symbols.woff2"), []byte("font-data"), 0o644)
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

func TestServesOnlyVersionedAttachmentHashWorker(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, requestPath := range []string{
		"/assets/attachment-hash.worker-D_WkX-nj.js",
		"/assets/attachment-hash.worker-.js",
		"/assets/attachment-hash.worker-D.WkX.js",
		"/assets/unrelated-worker-D_WkX-nj.js",
	} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		want := http.StatusNotFound
		if requestPath == "/assets/attachment-hash.worker-D_WkX-nj.js" {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Errorf("%s status = %d, want %d", requestPath, w.Code, want)
		}
	}
}

// index.html loads manifest-loader.js, which selects one of the two
// webmanifests at runtime. A relay that refuses any of the three serves an
// app that can never register its manifest, so they belong to the allowlist
// together.
func TestServesManifestLoaderChain(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, contentType := range map[string]string{
		"/manifest-loader.js": "text/javascript; charset=utf-8",
		"/setup.webmanifest":  "application/manifest+json",
	} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s status = %d, want 200", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != contentType {
			t.Errorf("%s content-type = %q, want %q", path, ct, contentType)
		}
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

func TestRedirectsLegacyUpdateReloadToDistinctAppPath(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/?setup=preserved&herdr_reload=0.14.4-42", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}
	if location := w.Header().Get("Location"); location != "/index.html?setup=preserved&herdr_reload=0.14.4-42" {
		t.Fatalf("location = %q", location)
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

func TestServesFontWithWebMIME(t *testing.T) {
	root := setupTestWebRoot(t)
	h, err := NewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	req := httptest.NewRequest(http.MethodGet, "/fonts/nerd-symbols.woff2", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "font/woff2" {
		t.Fatalf("font MIME = %q", got)
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
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "style-src 'self'; style-src-attr 'unsafe-inline'") {
		t.Fatalf("Content-Security-Policy does not allow sanitized ANSI style attributes: %q", csp)
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
