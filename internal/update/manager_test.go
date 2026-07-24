package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestNewerVersion(t *testing.T) {
	if !NewerVersion("1.2.4", "1.2.3") || NewerVersion("1.2.3", "1.2.3") || NewerVersion("latest", "1.2.3") {
		t.Fatal("semantic version comparison failed")
	}
}

func TestActiveManifestIdentityRequiresExactRevision(t *testing.T) {
	manifest := relayrelease.Manifest{Version: "1.2.3", Revision: "abcdef"}
	if ok, reason := activeManifestIdentity(manifest, "1.2.3", "ABCDEF"); !ok || reason != "" {
		t.Fatalf("matching identity rejected: ok=%v reason=%q", ok, reason)
	}
	if ok, reason := activeManifestIdentity(manifest, "1.2.3", "different"); ok ||
		!strings.Contains(reason, "revision") {
		t.Fatalf("revision mismatch accepted: ok=%v reason=%q", ok, reason)
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

func TestManagerRecoversOrphanedUpdateState(t *testing.T) {
	for _, stateName := range []string{"scheduled", "installing", "restarting"} {
		t.Run(stateName, func(t *testing.T) {
			root := t.TempDir()
			releaseRoot := filepath.Join(root, "installed")
			runtimeDir := filepath.Join(root, "runtime")
			started := time.Now().Add(-updateStartupGrace - time.Second).UTC().Format(time.RFC3339)
			if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
				State:          stateName,
				TargetVersion:  "1.2.4",
				TargetRevision: "new",
				StartedAt:      started,
			}); err != nil {
				t.Fatal(err)
			}

			manager := NewManager(releaseRoot, runtimeDir, "1.2.3", "old", "service", "http://127.0.0.1/healthz")
			state := manager.State()
			if state.State != "failed" || state.Error != "Update worker stopped before completion" || state.FinishedAt == "" {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestManagerPreservesUpdateStateOwnedByWorker(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(releaseRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(filepath.Join(releaseRoot, "update.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
		State:          "installing",
		TargetVersion:  "1.2.4",
		TargetRevision: "new",
		StartedAt:      time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(releaseRoot, runtimeDir, "1.2.3", "old", "service", "http://127.0.0.1/healthz")
	if state := manager.State(); state.State != "installing" || state.Error != "" {
		t.Fatalf("state = %#v", state)
	}
}

func TestManagerAllowsScheduledUpdateStartupGrace(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	runtimeDir := filepath.Join(root, "runtime")
	if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
		State:          "scheduled",
		TargetVersion:  "1.2.4",
		TargetRevision: "new",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(releaseRoot, runtimeDir, "1.2.3", "old", "service", "http://127.0.0.1/healthz")
	if state := manager.State(); state.State != "scheduled" {
		t.Fatalf("state = %#v", state)
	}
}
