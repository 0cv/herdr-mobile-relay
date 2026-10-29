package tailscalecli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type acknowledgementDenyHTTP struct{}

func (acknowledgementDenyHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("ack fixture denies HTTP")
}

func isolateAcknowledgementFixture(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		t.Setenv(key, "")
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "PATH"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	t.Chdir(root)
	previous := http.DefaultTransport
	http.DefaultTransport = acknowledgementDenyHTTP{}
	t.Cleanup(func() { http.DefaultTransport = previous })
}
