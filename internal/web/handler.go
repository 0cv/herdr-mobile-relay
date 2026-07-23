package web

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

var allowedAssets = map[string]bool{
	"index.html":            true,
	"manifest.webmanifest":  true,
	"notification-icons.js": true,
	"sw.js":                 true,
	"version.json":          true,
	"assets/app.js":         true,
	"assets/app.css":        true,
}

type Handler struct {
	root           *os.Root
	files          fs.FS
	bundleHash     string
	bundleVersion  string
	bundleRevision string
}

func NewHandler(webRoot string) (*Handler, error) {
	root, err := os.OpenRoot(webRoot)
	if err != nil {
		return nil, fmt.Errorf("open web root %s: %w", webRoot, err)
	}
	handler := &Handler{root: root, files: root.FS()}
	handler.loadIdentity()
	return handler, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requestPath, ok := canonicalAssetPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !isAllowedAsset(requestPath) {
		// Extensionless paths are SPA routes. Asset-looking paths remain 404.
		if path.Ext(requestPath) != "" {
			http.NotFound(w, r)
			return
		}
		requestPath = "index.html"
	}

	body, err := fs.ReadFile(h.files, requestPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	compressed, hasCompressed := h.readCompressed(requestPath)
	useBrotli := hasCompressed && acceptsBrotli(r.Header.Get("Accept-Encoding"))
	representation := body
	if useBrotli {
		representation = compressed
	}

	setSecurityHeaders(w)
	setCacheHeaders(w, requestPath)
	if hasCompressed {
		w.Header().Set("Vary", "Accept-Encoding")
	}
	contentType := mime.TypeByExtension(filepath.Ext(requestPath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if useBrotli {
		w.Header().Set("Content-Encoding", "br")
	}
	etag := computeETag(representation)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(representation)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(representation)
}

func canonicalAssetPath(raw string) (string, bool) {
	if strings.Contains(raw, "\\") || strings.Contains(raw, "\x00") {
		return "", false
	}
	trimmed := strings.TrimPrefix(raw, "/")
	if trimmed == "" {
		return "index.html", true
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	return cleaned, true
}

func isAllowedAsset(asset string) bool {
	return allowedAssets[asset] || strings.HasPrefix(asset, "icons/")
}

func acceptsBrotli(header string) bool {
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		if strings.TrimSpace(parts[0]) != "br" {
			continue
		}
		for _, parameter := range parts[1:] {
			parameter = strings.TrimSpace(parameter)
			if strings.HasPrefix(parameter, "q=") && strings.TrimSpace(strings.TrimPrefix(parameter, "q=")) == "0" {
				return false
			}
		}
		return true
	}
	return false
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimSpace(candidate) == etag || strings.TrimSpace(candidate) == "*" {
			return true
		}
	}
	return false
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' https: wss:; img-src 'self' blob: data:; style-src 'self'; script-src 'self'; worker-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
}

func setCacheHeaders(w http.ResponseWriter, asset string) {
	if strings.HasPrefix(asset, "assets/") || strings.HasPrefix(asset, "icons/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

func (h *Handler) readCompressed(asset string) ([]byte, bool) {
	body, err := fs.ReadFile(h.files, asset+".br")
	return body, err == nil
}

func computeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + fmt.Sprintf("%x", sum[:16]) + `"`
}

func (h *Handler) loadIdentity() {
	versionData, err := fs.ReadFile(h.files, "version.json")
	if err == nil {
		var version struct {
			Version        string `json:"version"`
			ReleaseVersion string `json:"release_version"`
			Revision       string `json:"revision"`
		}
		if json.Unmarshal(versionData, &version) == nil {
			h.bundleVersion = version.ReleaseVersion
			if h.bundleVersion == "" {
				h.bundleVersion = version.Version
			}
			h.bundleRevision = version.Revision
		}
	}
	if bundleHash, err := relayrelease.WebHashFS(h.files); err == nil {
		h.bundleHash = bundleHash
	}
}

func (h *Handler) Close() error           { return h.root.Close() }
func (h *Handler) BundleHash() string     { return h.bundleHash }
func (h *Handler) BundleVersion() string  { return h.bundleVersion }
func (h *Handler) BundleRevision() string { return h.bundleRevision }
