package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	if !NewerVersion("1.2.4", "1.2.3") || NewerVersion("1.2.3", "1.2.3") || NewerVersion("latest", "1.2.3") {
		t.Fatal("semantic version comparison failed")
	}
}

func TestFetchReleaseRequiresExactTargetAssetsAndCommit(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	target := strings.ReplaceAll(CurrentTestTarget(), "/", "_")
	_ = target
	archive := fmt.Sprintf("herdr-mobile-relay_1.2.4_%s_%s.tar.gz", testGOOS(), testGOARCH())
	mux.HandleFunc("/releases/latest", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"tag_name": "v1.2.4",
			"assets": []map[string]string{
				{"name": archive, "url": server.URL + "/archive"},
				{"name": "checksums.txt", "url": server.URL + "/checksums"},
			},
		})
	})
	mux.HandleFunc("/git/ref/tags/v1.2.4", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": map[string]string{"type": "commit", "sha": revision},
		})
	})
	manager := NewManager(t.TempDir(), t.TempDir(), "1.2.3", "old", "service", "http://127.0.0.1:8375/healthz")
	manager.apiBase = server.URL
	manager.client = server.Client()
	metadata, err := manager.fetchRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != "1.2.4" || metadata.Revision != revision || metadata.ArchiveName != archive {
		t.Fatalf("metadata = %#v", metadata)
	}
}

// These wrappers keep the expected archive construction explicit in the test.
func testGOOS() string   { return strings.Split(CurrentTestTarget(), "/")[0] }
func testGOARCH() string { return strings.Split(CurrentTestTarget(), "/")[1] }
func CurrentTestTarget() string {
	return currentTargetForTest()
}
