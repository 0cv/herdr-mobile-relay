package stablestate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTripAndOwnership(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "state.json")
	state := Default("/tmp/relay.env")
	state["created_tunnel"] = true
	if err := Write(filename, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	loaded, err := ReadState(filename)
	if err != nil {
		t.Fatal(err)
	}
	if loaded["created_tunnel"] != true {
		t.Fatalf("created_tunnel = %v", loaded["created_tunnel"])
	}
	loaded["owner"] = "other"
	if err := Write(filename, loaded); err == nil {
		t.Fatal("unowned state was written")
	}
}

func TestHealthMatch(t *testing.T) {
	local := map[string]any{"status": "ok", "instance": "one", "version": "1", "protocol": float64(2)}
	public := map[string]any{"status": "ok", "instance": "one", "version": "1", "protocol": float64(2)}
	if err := HealthMatch(local, public); err != nil {
		t.Fatal(err)
	}
	public["version"] = "2"
	if err := HealthMatch(local, public); err == nil {
		t.Fatal("mismatched health accepted")
	}
}
