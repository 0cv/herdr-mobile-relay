package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
)

func TestDisposablePiPairedBrowser(t *testing.T) {
	if os.Getenv("HERDR_SCROLL_FIXTURE_MODE") != "fullscreen" || os.Getenv("HERDR_P1_BROWSER_SCRIPT") == "" {
		t.Skip("requires the owned native paired-browser driver")
	}
	contract, err := p1fixture.Current()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, PluginPort: 0, Token: hex.EncodeToString(secret), InstanceID: "p1-native-browser",
		HerdrBin: os.Getenv("HERDR_SCROLL_FIXTURE_BIN"), SocketPath: contract.SocketPath, PollInterval: 2,
		WebRoot: os.Getenv("HERDR_P1_WEB_ROOT"), RuntimeDir: filepath.Join(contract.Root, "relay-runtime"),
		CacheDir: filepath.Join(contract.Root, "relay-cache"), ConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		ReleaseRoot: filepath.Join(contract.Root, "relay-release"),
	}
	s := New(cfg, "0.22.9", "native-fixture", slog.Default())
	observer, err := herdr.NewScrollEvidence(2*1024*1024, 256)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.herdrC.SetScrollEvidence(observer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("native relay shutdown: %v", err)
		}
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, "GET", url+"/readyz", nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("owned relay did not become inventory-ready")
	}
	payload, err := json.Marshal(map[string]any{"url": url, "token": cfg.Token, "pane": os.Getenv("HERDR_SCROLL_FIXTURE_PANE")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contract.Root, "browser-ready.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(contract.Root, "browser-done.json")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("paired browser did not finish")
		case <-time.After(50 * time.Millisecond):
		}
	}
	records, err := observer.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, record := range records {
		if record.Method == "pane.send_text" {
			writes++
			if record.WriteCalls != 1 || !record.Wrote || record.Error != "" || record.Overflow {
				t.Fatal("paired native wheel was not a single complete socket write")
			}
		}
	}
	if writes != 2 {
		t.Fatalf("paired browser raw writes = %d, want two", writes)
	}
	if err := contract.Save("paired-browser-wire.json", records); err != nil {
		t.Fatal(err)
	}
}
