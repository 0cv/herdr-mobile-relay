package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadFileReturnsTheChecksumOfWhatItWrote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("payload"))
	}))
	defer server.Close()
	filename := filepath.Join(t.TempDir(), "payload")
	out, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	checksum, err := DownloadFile(t.Context(), server.Client(), server.URL, out, 64)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("payload"))
	if checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum = %s", checksum)
	}
	if data, err := os.ReadFile(filename); err != nil || string(data) != "payload" {
		t.Fatalf("file = %q, %v", data, err)
	}
}

func TestDownloadFileEnforcesTheLimitAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/missing" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Transfer-Encoding", "chunked")
		_, _ = writer.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer server.Close()
	for path, want := range map[string]string{"/big": "size limit", "/missing": "HTTP 404"} {
		out, err := os.CreateTemp(t.TempDir(), "download")
		if err != nil {
			t.Fatal(err)
		}
		_, err = DownloadFile(t.Context(), server.Client(), server.URL+path, out, 10)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s error = %v, want %q", path, err, want)
		}
	}
}
