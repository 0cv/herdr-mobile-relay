package stablestate

import (
	"errors"
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

func TestTunnelListCommands(t *testing.T) {
	const tunnelUUID = "11111111-2222-4333-8444-555555555555"
	const populatedList = `[{"id":"` + tunnelUUID + `","name":"herdr-mobile-relay-workstation"}]`
	const notFound = "Cloudflare tunnel " + tunnelUUID + " was not found"
	const notAList = "Cloudflare tunnel list output was not a JSON list"

	tests := []struct {
		name       string
		input      string
		command    string
		argument   string
		wantOutput string
		wantError  string
	}{
		{
			name:     "null list has no tunnel by name",
			input:    "null\n",
			command:  "tunnel-id-by-name",
			argument: "missing",
		},
		{
			name:     "empty array has no tunnel by name",
			input:    "[]\n",
			command:  "tunnel-id-by-name",
			argument: "missing",
		},
		{
			name:      "null list does not contain UUID",
			input:     "null\n",
			command:   "tunnel-list-has",
			argument:  tunnelUUID,
			wantError: notFound,
		},
		{
			name:      "null list has no name for UUID",
			input:     "null\n",
			command:   "tunnel-name-by-id",
			argument:  tunnelUUID,
			wantError: notFound,
		},
		{
			name:       "populated list resolves UUID by name",
			input:      populatedList,
			command:    "tunnel-id-by-name",
			argument:   "herdr-mobile-relay-workstation",
			wantOutput: tunnelUUID + "\n",
		},
		{
			name:      "populated list contains UUID",
			input:     populatedList,
			command:   "tunnel-list-has",
			argument:  tunnelUUID,
			wantError: "",
		},
		{
			name:       "populated list resolves name by UUID",
			input:      populatedList,
			command:    "tunnel-name-by-id",
			argument:   tunnelUUID,
			wantOutput: "herdr-mobile-relay-workstation\n",
		},
		{
			name:      "object is rejected by name lookup",
			input:     "{}\n",
			command:   "tunnel-id-by-name",
			argument:  "missing",
			wantError: notAList,
		},
		{
			name:      "object is rejected by UUID lookup",
			input:     "{}\n",
			command:   "tunnel-list-has",
			argument:  tunnelUUID,
			wantError: notAList,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := runTunnelCommand(t, test.input, test.command, test.argument)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != test.wantError {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
			if output != test.wantOutput {
				t.Fatalf("output = %q, want %q", output, test.wantOutput)
			}
		})
	}
}

func runTunnelCommand(t *testing.T, input, command, argument string) (string, error) {
	t.Helper()
	directory := t.TempDir()
	inputFile := filepath.Join(directory, "tunnels.json")
	if err := os.WriteFile(inputFile, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	outputFile := filepath.Join(directory, "output")
	stdout, err := os.Create(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	runErr := Run([]string{command, inputFile, argument}, stdout)
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(output), runErr
}

func TestWriteIsIndentedAndTightensDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "state.json")
	if err := os.WriteFile(filename, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(filename, map[string]any{"stage": "new", "owner": Owner}); err != nil {
		t.Fatal(err)
	}
	const want = `{
  "owner": "herdr-mobile-relay-stable-setup-v1",
  "stage": "new"
}
`
	data, err := os.ReadFile(filename)
	if err != nil || string(data) != want {
		t.Fatalf("state = %q, error %v; want %q", data, err, want)
	}
	for path, mode := range map[string]os.FileMode{dir: 0o700, filename: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %v, error %v; want %o", path, info, err, mode)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory entries = %v, error %v; want only the state file", entries, err)
	}
}

func TestWriteCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	if err := Write(filepath.Join(dir, "state.json"), Default("/tmp/relay.env")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, error %v; want 700", info, err)
	}
}

func TestWriteRejectsUnownedStateBeforeTouchingFilesystem(t *testing.T) {
	for name, state := range map[string]map[string]any{
		"nil":          nil,
		"missing":      {"stage": "new"},
		"wrong-type":   {"owner": 1},
		"wrong-string": {"owner": "other"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			if err := Write(filepath.Join(dir, "state.json"), state); err == nil {
				t.Fatal("unowned state was written")
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected state created the directory: %v", err)
			}
		})
	}
}

func TestWriteMarshalFailureKeepsExistingState(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(filename, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(filename, map[string]any{"owner": Owner, "bad": make(chan int)}); err == nil {
		t.Fatal("unmarshalable state was written")
	}
	data, err := os.ReadFile(filename)
	if err != nil || string(data) != "old" {
		t.Fatalf("state = %q, error %v; want old", data, err)
	}
}
